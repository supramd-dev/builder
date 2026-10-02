package runner

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logSpool is the on-disk copy of one stage's output: the bytes that become
// the run's full log in object storage. It exists because the stored chunks
// are capped (the live viewer must stay cheap) while the download and the
// summary have to see the whole log — including its end, which is where a
// failing stage says what went wrong.
//
// The spool is two regions of one file:
//
//   - the head region keeps the first head bytes verbatim: the command line,
//     the first error, the context a reader wants before the noise;
//   - the tail region is a ring of tail bytes that later output rotates
//     through, so a runaway log always keeps its END.
//
// A log that fits under the cap is stored byte for byte, with no marker: the
// head region fills first, and the ring only ever holds what the head could
// not. Past the cap, the assembled file is head, one marker line naming how
// much was dropped, then the ring in arrival order — a reader is never left
// guessing whether it holds the whole log.
//
// Spool failures (a full disk, an unwritable directory) are never fatal to the
// stage: the spool is abandoned, the stored chunks still carry the output, and
// the run simply has no full-log object.
type logSpool struct {
	f    *os.File
	path string
	head int64 // size of the head region
	tail int64 // size of the ring region

	// err is the first I/O failure. Once set, the spool is dead: Write
	// stops touching the file and the upload is skipped.
	err error

	headN  int64 // bytes written into the head region (≤ head)
	ringN  int64 // bytes written into the ring region, including overwritten ones
	ringAt int64 // where the next ring byte goes
	total  int64 // bytes the writer has offered
}

// spoolHeadBytes is how much of the beginning a capped log keeps verbatim.
const spoolHeadBytes = 1 << 20

// newLogSpool creates the spool file in dir. maxBytes is the assembled log's
// bound (the head and the ring together); it is clamped up to a page so a
// misconfigured tiny value still produces a usable spool.
func newLogSpool(dir string, maxBytes int64) (*logSpool, error) {
	if maxBytes < 4096 {
		maxBytes = 4096
	}
	head := int64(spoolHeadBytes)
	if head > maxBytes/2 {
		// A cap below the default head still splits, so the beginning and
		// the end both survive.
		head = maxBytes / 2
	}
	f, err := os.CreateTemp(dir, spoolPrefix+"*.tmp")
	if err != nil {
		return nil, err
	}
	return &logSpool{f: f, path: f.Name(), head: head, tail: maxBytes - head}, nil
}

// spoolPrefix marks this package's temp files, so the startup sweep can find
// the ones a crash left behind (see CleanStaleSpools).
const spoolPrefix = "md-builder-log-"

// Write appends output, filling the head region first and then rotating the
// ring. It never returns an error: the caller is an SSH stream that must not
// fail because a spool file did.
func (sp *logSpool) Write(p []byte) (int, error) {
	if sp == nil || sp.err != nil || len(p) == 0 {
		return len(p), nil
	}
	n := len(p)
	if room := sp.head - sp.headN; room > 0 {
		chunk := p
		if int64(len(chunk)) > room {
			chunk = chunk[:room]
		}
		if _, err := sp.f.WriteAt(chunk, sp.headN); err != nil {
			sp.abandon(err)
			return n, nil
		}
		sp.headN += int64(len(chunk))
		p = p[len(chunk):]
	}
	for len(p) > 0 {
		chunk := p
		if room := sp.tail - sp.ringAt; int64(len(chunk)) > room {
			chunk = chunk[:room]
		}
		if _, err := sp.f.WriteAt(chunk, sp.head+sp.ringAt); err != nil {
			sp.abandon(err)
			return n, nil
		}
		sp.ringAt = (sp.ringAt + int64(len(chunk))) % sp.tail
		sp.ringN += int64(len(chunk))
		p = p[len(chunk):]
	}
	sp.total += int64(n)
	return n, nil
}

// abandon gives up on the spool after an I/O error: the bytes are stored as
// chunks either way, and a failed spool must not fail a stage. It reports the
// failure once — the reason a run has no full log should not be a mystery.
func (sp *logSpool) abandon(err error) {
	sp.err = fmt.Errorf("log spool %s: %w", sp.path, err)
	log.Printf("tasklog: %v; this run will have no full log", sp.err)
}

// broken reports whether the spool is unusable: it failed, or it was already
// closed (its file removed) and holds nothing a caller could still read.
func (sp *logSpool) broken() bool { return sp == nil || sp.err != nil || sp.f == nil }

// dropped is how many bytes the cap pushed out of the ring (0 when the whole
// log fits).
func (sp *logSpool) dropped() int64 {
	held := sp.headN + sp.ringHeld()
	return sp.total - held
}

// ringHeld is how many bytes the ring currently holds, in arrival order.
func (sp *logSpool) ringHeld() int64 {
	if sp.ringN < sp.tail {
		return sp.ringN
	}
	return sp.tail
}

// piece is one run of the assembled log: either bytes held in memory (the
// dropped-bytes marker) or a span of the spool file.
type piece struct {
	text string // non-empty for an in-memory piece
	off  int64  // file offset, for a file piece
	n    int64
}

// pieces lays the assembled log out in order: head, marker (only when
// something was dropped), then the ring from its oldest byte.
func (sp *logSpool) pieces() []piece {
	if sp.broken() {
		return nil
	}
	out := []piece{{off: 0, n: sp.headN}}
	if d := sp.dropped(); d > 0 {
		out = append(out, piece{text: sp.marker(d)})
	}
	if held := sp.ringHeld(); held > 0 {
		// ringAt is where the next byte would go: while the ring has not
		// wrapped it is past the end of what is there (which therefore
		// starts at 0), and once it has, it is exactly where the oldest
		// surviving byte sits.
		var start int64
		if sp.ringN >= sp.tail {
			start = sp.ringAt
		}
		if first := sp.tail - start; first >= held {
			out = append(out, piece{off: sp.head + start, n: held})
		} else {
			// Wrapped: the ring's contents are the tail of the region
			// followed by its head.
			out = append(out, piece{off: sp.head + start, n: first},
				piece{off: sp.head, n: held - first})
		}
	}
	return out
}

// marker names what the cap dropped, between the two halves that were kept.
func (sp *logSpool) marker(dropped int64) string {
	return fmt.Sprintf("\n… %s dropped (log capped at %s): the first %s and the last %s are kept …\n",
		bytesLabel(dropped), bytesLabel(sp.head+sp.tail),
		bytesLabel(sp.headN), bytesLabel(sp.ringHeld()))
}

// bytesLabel renders a byte count for a human reader (one decimal).
func bytesLabel(n int64) string {
	const mib = 1 << 20
	if n >= mib {
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	}
	if n >= 1<<10 {
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Size is the assembled log's length, marker included.
func (sp *logSpool) Size() int64 {
	var n int64
	for _, p := range sp.pieces() {
		n += int64(len(p.text)) + p.n
	}
	return n
}

// Reader streams the assembled log, for the upload: nothing is buffered, so a
// capped-at-tens-of-mebibytes log costs no memory.
func (sp *logSpool) Reader() io.Reader {
	readers := make([]io.Reader, 0, 4)
	for _, p := range sp.pieces() {
		if p.text != "" {
			readers = append(readers, strings.NewReader(p.text))
			continue
		}
		readers = append(readers, io.NewSectionReader(sp.f, p.off, p.n))
	}
	return io.MultiReader(readers...)
}

// Tail returns the last n bytes of the assembled log (fewer when it is
// shorter) — what a reader that wants the stage's outcome asks for. It reads
// the spool, not the stored chunks, so a capped stage still yields the output
// it actually ended with.
func (sp *logSpool) Tail(n int) string {
	parts := sp.pieces()
	// Walk backwards, taking what fits.
	var chunks []string
	want := int64(n)
	for i := len(parts) - 1; i >= 0 && want > 0; i-- {
		p := parts[i]
		length := int64(len(p.text)) + p.n
		if p.text != "" {
			if length > want {
				chunks = append(chunks, p.text[length-want:])
			} else {
				chunks = append(chunks, p.text)
			}
			want -= length
			continue
		}
		take := length
		off := p.off
		if take > want {
			take = want
			off = p.off + p.n - take
		}
		buf := make([]byte, take)
		if _, err := sp.f.ReadAt(buf, off); err != nil && err != io.EOF {
			sp.err = fmt.Errorf("log spool %s: read tail: %w", sp.path, err)
			return ""
		}
		chunks = append(chunks, string(buf))
		want -= take
	}
	// Collected from the end, so put them back in order.
	var b strings.Builder
	for i := len(chunks) - 1; i >= 0; i-- {
		b.WriteString(chunks[i])
	}
	return b.String()
}

// Close removes the spool file. It is safe to call more than once.
func (sp *logSpool) Close() error {
	if sp == nil || sp.f == nil {
		return nil
	}
	err := sp.f.Close()
	errRemove := os.Remove(sp.path)
	sp.f = nil
	if err == nil {
		err = errRemove
	}
	return err
}

// CleanStaleSpools removes the spool files a crash left behind: every file
// this package created in dir that is older than maxAge. It runs at startup,
// where nothing of ours can be writing: a stage the previous process was
// running is re-armed and starts a new spool. The age check is what keeps an
// instance that shares the directory with another one from removing a spool
// that is still being written.
func CleanStaleSpools(dir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, spoolPrefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed++
		}
	}
	return removed, nil
}

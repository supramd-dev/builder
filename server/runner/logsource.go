package runner

import (
	"context"
	"log"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// LogSource reads one run's log. Both log endpoints work through it: the
// viewer follows a stage by byte offset, the download streams the whole log
// from the beginning.
//
// Its bytes come from the shortest route that has them — the writer's buffer
// while this process is running the stage (the newest output, and the reason a
// live view is immediate), the run's stored parts otherwise — and it never
// holds more than one page plus one part in memory.
//
// One source belongs to one request: it caches the listing it needs, so it is
// not safe to share.
type LogSource struct {
	// live is the writer producing this log in this process, or nil when the
	// stage is not running here (it is finished, or another process has it).
	live *LogWriter
	// store lists the stored parts, objs reads them (nil when the store has
	// no object storage at all).
	store *store.Store
	objs  storage.Store
	// prefix is where the run's parts are, "" when the run has no log yet.
	prefix string
	// page is the largest read a caller should ask for: one part, so a reader
	// catching up fetches each part once instead of once per smaller page.
	page int64

	// listed/parts cache the stored parts for a source without a live writer;
	// a live writer answers from its own bookkeeping instead.
	listed bool
	parts  []store.LogPart
}

// OpenLogSource returns a reader for one attempt's log. It never fails: a run
// with no log yet, or a store without object storage, yields a source that
// reads nothing.
func (s *Service) OpenLogSource(task *store.Task, attempt int) *LogSource {
	src := &LogSource{store: s.Store, page: s.LogLimits.partBytes()}
	if s.Store != nil {
		src.objs = s.Store.Objects()
	}
	s.liveMu.Lock()
	src.live = s.live[liveLogKey{taskID: task.ID, attempt: attempt}]
	s.liveMu.Unlock()
	if src.live != nil {
		// The writer knows its own parts; there is nothing to look up, and
		// the run row lags behind it by design.
		return src
	}
	if s.Store != nil && src.objs != nil {
		if run, err := s.Store.FindTaskRun(task.ID, attempt); err == nil {
			src.prefix = run.LogPrefix
		}
	}
	return src
}

// PageBytes is the largest read this source should be asked for.
func (src *LogSource) PageBytes() int64 { return src.page }

// End is the log's length so far: everything stored plus, while this process
// runs the stage, what its buffer holds. It is what a caller needs to place a
// tail read, or to size a download; a read that ends at it has caught up.
func (src *LogSource) End(ctx context.Context) (int64, error) {
	end, _, err := src.endAndParts(ctx)
	return end, err
}

// endAndParts is End together with the parts the log is stored in, read in one
// look: a tail needs both to place itself.
func (src *LogSource) endAndParts(ctx context.Context) (int64, []store.LogPart, error) {
	if lw := src.live; lw != nil {
		lw.mu.Lock()
		defer lw.mu.Unlock()
		return lw.base + int64(len(lw.buf)), append([]store.LogPart(nil), lw.parts...), nil
	}
	parts, err := src.storedParts(ctx)
	if err != nil {
		return 0, nil, err
	}
	if len(parts) == 0 {
		return 0, nil, nil
	}
	return parts[len(parts)-1].End(), parts, nil
}

// ReadFrom returns up to max bytes of the log starting at byte offset off, in
// stream order. A shorter slice means the log ends there.
//
// A page never mixes the two stores: one that starts in a stored part stops at
// that part's end, so a reader paging through a long log fetches each part
// exactly once. A page that starts in the buffer (the newest output) is served
// whole, which is the case a viewer following a running stage is in.
func (src *LogSource) ReadFrom(ctx context.Context, off, max int64) ([]byte, error) {
	if off < 0 || max <= 0 {
		return nil, nil
	}
	if lw := src.live; lw != nil {
		lw.mu.Lock()
		base, buf := lw.base, lw.buf
		if off >= base {
			// In the buffer. The writer keeps writing to it, so the reader
			// gets its own copy of the bytes it asked for.
			i := off - base
			if i >= int64(len(buf)) {
				lw.mu.Unlock()
				return nil, nil
			}
			end := i + max
			if end > int64(len(buf)) {
				end = int64(len(buf))
			}
			out := append([]byte(nil), buf[i:end]...)
			lw.mu.Unlock()
			return out, nil
		}
		// Behind the buffer: the stored parts answer it, which the writer
		// already holds in stream order.
		parts := append([]store.LogPart(nil), lw.parts...)
		lw.mu.Unlock()
		return readPart(ctx, src.objs, parts, off, max)
	}
	parts, err := src.storedParts(ctx)
	if err != nil {
		return nil, err
	}
	return readPart(ctx, src.objs, parts, off, max)
}

// Tail returns up to n bytes from the end of the log, with the offset they
// start at — which is where the reader must continue from, and may be later
// than asked for when the bytes just before the end were never stored. It is
// what a reader that opens a long log wants (the newest output rather than the
// whole file): a page load that read from zero would stream a log of any size
// into one response, and the beginning of a long log is the part nobody is
// looking at.
func (src *LogSource) Tail(ctx context.Context, n int64) ([]byte, int64, error) {
	if n <= 0 {
		return nil, 0, nil
	}
	end, parts, err := src.endAndParts(ctx)
	if err != nil {
		return nil, 0, err
	}
	if end == 0 {
		return nil, 0, nil
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	// A tail may start inside a part, which reads from there — but not in the
	// gap a stage that restarted leaves between two parts, where nothing
	// covers the offset. There it starts at the part that follows the gap.
	if start > 0 {
		for _, p := range parts {
			if p.End() > start {
				if p.Start > start {
					start = p.Start
				}
				break
			}
		}
	}
	data, err := src.ReadFrom(ctx, start, end-start)
	if err != nil {
		return nil, 0, err
	}
	return data, start, nil
}

// storedParts lists the run's stored parts, once per source.
func (src *LogSource) storedParts(ctx context.Context) ([]store.LogPart, error) {
	if src.listed {
		return src.parts, nil
	}
	src.listed = true
	if src.prefix == "" || src.objs == nil {
		return nil, nil
	}
	parts, err := src.store.ListRunLogParts(ctx, src.prefix)
	if err != nil {
		return nil, err
	}
	src.parts = parts
	return parts, nil
}

// readPart serves a read out of the stored parts: the one holding off, up to
// the end of that part. The bytes are the backend's own copy, so the caller
// may keep them.
func readPart(ctx context.Context, objs storage.Store, parts []store.LogPart, off, max int64) ([]byte, error) {
	if objs == nil {
		return nil, nil
	}
	for _, p := range parts {
		if off < p.Start || off >= p.End() {
			continue
		}
		data, err := objs.Get(ctx, p.Key)
		if err != nil {
			return nil, err
		}
		i := off - p.Start
		if i >= int64(len(data)) {
			// The listing described more than the object holds: a part
			// being overwritten right now (a resumed stage). Nothing to
			// read from it; the next part is the reader's next stop.
			log.Printf("tasklog: part %s holds %d bytes, want %d", p.Key, len(data), p.Size)
			return nil, nil
		}
		end := i + max
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return data[i:end], nil
	}
	return nil, nil
}

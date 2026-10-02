import { Fragment, useEffect, useRef, useState } from 'react'
import { Download } from 'lucide-react'
import { getTaskLogs, isTerminalStatus, taskLogDownloadUrl } from './api'

// One read returns at most this many chunks (the store caps the query), so a
// full page means there may be more and the viewer asks again.
const LOG_PAGE = 1000
// Pages a live tick fetches back to back before yielding to its interval: the
// log of a long stage catches up quickly, but one tick cannot monopolise the
// connection for ever. A finished attempt has no next tick, so it is read to
// the end in one go (the log itself is capped server-side).
const LOG_PAGES_PER_TICK = 20
// How long a live view waits after a read finishes before reading again.
const LOG_POLL_MS = 2000

// TaskLogView shows one attempt's incremental log, polling with
// after=lastSeq while the task may still write to it. Shared by the task
// pipeline page (following a running stage) and the run detail page (a
// finished stage's stdout, or an earlier attempt's).
//
// Exactly one read runs at a time: the next one is scheduled only once the
// previous has finished, never by a bare interval that can fire a second
// reader while the first is still working — two readers starting from the same
// sequence would each append the same page, which is how the same output used
// to show up twice. The cursor only ever moves forward, and a chunk is
// appended only if it is past the last one rendered, so even a repeated read
// cannot double up. A new taskId or
// attempt is a new stream and resets what is rendered; the task finishing only
// stops the timer, after one last read, so the lines written between the final
// tick and the outcome still show up.
//
// Nothing read is ever dropped from the view: output that appeared stays
// where it is, and the page grows downward as the attempt writes more. (A
// view that trimmed its head to bound the text would make the first lines
// vanish under the reader mid-follow.) The stored log is capped server-side,
// which is what bounds this — the bar above links to that copy, whole or not.
export default function TaskLogView({
  taskId,
  attempt,
  status,
}: {
  taskId: number
  // The attempt to show; omit it for the task's current one.
  attempt?: number
  // The task's status, which decides whether the view follows its output.
  status: string
}) {
  // A task that may still write: one that is running, or one in a status this
  // build does not know (an older server's row) — anything but a queued task,
  // which has written nothing yet and will not until it runs, and a finished
  // one, which has nothing left to say. The page around this view polls the
  // status itself, so a queued task's follow starts the moment it does.
  const live = status !== 'pending' && !isTerminalStatus(status)
  // A stage that has not been dispatched yet reports attempt 0, which the API
  // reads as a bad request. Omitting the parameter asks for the current
  // attempt — which is the only one such a node can have.
  const showAttempt = attempt !== undefined && attempt > 0 ? attempt : undefined
  // One entry per read, kept as it arrived: appending to the text would relayout
  // the whole log on every tick, and the array lets React touch only the new
  // nodes.
  const [chunks, setChunks] = useState<string[]>([])
  const lastSeq = useRef(0)
  const preRef = useRef<HTMLPreElement>(null)

  // A new task or attempt is a new stream: drop the old text and read from the
  // start. This runs before the reading effect (declaration order), so the
  // reader below never sees a sequence from the previous stream.
  useEffect(() => {
    setChunks([])
    lastSeq.current = 0
  }, [taskId, showAttempt])

  useEffect(() => {
    let stop = false
    let timer: number | undefined

    // Reads one page at a time until the log is caught up (or a live tick has
    // had its share of the connection).
    async function read() {
      const maxPages = live ? LOG_PAGES_PER_TICK : Number.POSITIVE_INFINITY
      for (let page = 0; page < maxPages && !stop; page++) {
        let res
        try {
          res = await getTaskLogs(taskId, lastSeq.current, showAttempt)
        } catch {
          return // transient; the next tick retries
        }
        if (stop) return
        // Only what is past the last line rendered. The cursor moves forward
        // only, so a read that raced another cannot pull it back — which is
        // what made the next tick fetch, and append, a page all over again.
        const fresh = res.chunks.filter((c) => c.seq > lastSeq.current)
        if (res.lastSeq > lastSeq.current) lastSeq.current = res.lastSeq
        if (fresh.length > 0) {
          const joined = fresh.map((c) => c.content).join('')
          setChunks((cur) => [...cur, joined])
        }
        if (res.chunks.length < LOG_PAGE) return
      }
    }

    async function tick() {
      await read()
      if (stop || !live) return
      timer = window.setTimeout(tick, LOG_POLL_MS)
    }
    tick()

    return () => {
      stop = true
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [taskId, showAttempt, live])

  // Auto-scroll to the bottom on new output while following.
  const stick = useRef(true)
  useEffect(() => {
    const pre = preRef.current
    if (pre && stick.current) {
      pre.scrollTop = pre.scrollHeight
    }
  }, [chunks])

  return (
    <div className="task-log-box">
      <div className="task-log-bar">
        <a
          className="task-log-download"
          href={taskLogDownloadUrl(taskId, showAttempt)}
          download={`task-${taskId}${showAttempt === undefined ? '' : `-attempt-${showAttempt}`}.log`}
          title="Download the full log as a file"
        >
          <Download size={13} /> Download log
        </a>
      </div>
      <pre
        ref={preRef}
        className="task-log"
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
      >
        {chunks.length > 0
          ? chunks.map((chunk, i) => <Fragment key={i}>{chunk}</Fragment>)
          : live
            ? 'waiting for output…'
            : '(no output)'}
      </pre>
    </div>
  )
}

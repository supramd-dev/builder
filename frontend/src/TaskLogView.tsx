import { useEffect, useRef, useState } from 'react'
import { Download } from 'lucide-react'
import { getTaskLogs, taskLogDownloadUrl } from './api'

// One read returns at most this many chunks (the store caps the query), so a
// full page means there may be more and the viewer asks again.
const LOG_PAGE = 1000
// Pages a live tick fetches back to back before yielding to its interval: the
// log of a long stage catches up quickly, but one tick cannot monopolise the
// connection for ever. A finished attempt has no next tick, so it is read to
// the end in one go (the log itself is capped server-side).
const LOG_PAGES_PER_TICK = 20
// Rendered text is capped; the server keeps the whole log.
const MAX_TEXT = 512 * 1024

// TaskLogView shows one attempt's incremental log, polling with
// after=lastSeq while live is set. Shared by the task pipeline page
// (following a running stage) and the run detail page (a finished stage's
// stdout, or an earlier attempt's).
//
// Exactly one reader runs at a time — the effect below is the only caller of
// getTaskLogs — so a chunk is appended once: two pollers starting together
// from the same sequence would each append the same page. A new taskId or
// attempt is a new stream and resets what is rendered; `live` flipping false
// only stops the timer, after one last read, so the lines written between the
// final tick and the outcome still show up.
//
// The rendered text is capped (only the tail is kept), so the bar above it
// links to the server's copy of the whole log — the button is how a user
// gets the full file, whatever the viewer is showing.
export default function TaskLogView({
  taskId,
  attempt,
  live,
}: {
  taskId: number
  // The attempt to show; omit it for the task's current one.
  attempt?: number
  live: boolean
}) {
  const [text, setText] = useState('')
  const [truncated, setTruncated] = useState(false)
  const lastSeq = useRef(0)
  // Everything ever appended, i.e. the length of the untrimmed text.
  const totalRef = useRef(0)
  const preRef = useRef<HTMLPreElement>(null)

  // A new task or attempt is a new stream: drop the old text and read from the
  // start. This runs before the reading effect (declaration order), so the
  // reader below never sees a sequence from the previous stream.
  useEffect(() => {
    setText('')
    setTruncated(false)
    lastSeq.current = 0
    totalRef.current = 0
  }, [taskId, attempt])

  useEffect(() => {
    let stop = false
    async function poll() {
      const maxPages = live ? LOG_PAGES_PER_TICK : Number.POSITIVE_INFINITY
      for (let page = 0; page < maxPages; page++) {
        let res
        try {
          res = await getTaskLogs(taskId, lastSeq.current, attempt)
        } catch {
          return // transient; the next tick retries
        }
        if (stop) return
        lastSeq.current = res.lastSeq
        if (res.chunks.length === 0) return
        const joined = res.chunks.map((c) => c.content).join('')
        totalRef.current += joined.length
        if (totalRef.current > MAX_TEXT) setTruncated(true)
        setText((cur) => {
          const next = cur + joined
          return next.length > MAX_TEXT ? next.slice(next.length - MAX_TEXT) : next
        })
        if (res.chunks.length < LOG_PAGE) return
      }
    }
    poll()
    if (!live) return () => { stop = true }
    const timer = setInterval(poll, 2000)
    return () => {
      stop = true
      clearInterval(timer)
    }
  }, [taskId, attempt, live])

  // Auto-scroll to the bottom on new output while following.
  const stick = useRef(true)
  useEffect(() => {
    const pre = preRef.current
    if (pre && stick.current) {
      pre.scrollTop = pre.scrollHeight
    }
  }, [text])

  return (
    <div className="task-log-box">
      <div className="task-log-bar">
        <a
          className="task-log-download"
          href={taskLogDownloadUrl(taskId, attempt)}
          download={`task-${taskId}${attempt === undefined ? '' : `-attempt-${attempt}`}.log`}
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
        {truncated && <span className="text-muted">… earlier output trimmed (full log on the server) …{'\n'}</span>}
        {text || (live ? 'waiting for output…' : '(no output)')}
      </pre>
    </div>
  )
}

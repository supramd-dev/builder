import { useEffect, useRef, useState } from 'react'
import { getTaskLogs } from './api'

// TaskLogView shows one task's incremental log, polling with after=lastSeq
// while live is set. Shared by the task pipeline page (following a running
// stage) and the run detail page (a finished stage's stdout).
//
// The poll interval is derived from `live` but taskId alone resets the
// stream: flipping live (a task finishing while being watched) only stops
// the timer — the already-streamed text stays put, no re-fetch from 0.
export default function TaskLogView({
  taskId,
  live,
}: {
  taskId: number
  live: boolean
}) {
  const [text, setText] = useState('')
  const [truncated, setTruncated] = useState(false)
  const lastSeq = useRef(0)
  const preRef = useRef<HTMLPreElement>(null)

  // Reset + fetch whenever the log's task changes.
  useEffect(() => {
    setText('')
    setTruncated(false)
    lastSeq.current = 0

    let stop = false
    async function poll() {
      try {
        const res = await getTaskLogs(taskId, lastSeq.current)
        if (stop) return
        lastSeq.current = res.lastSeq
        if (res.chunks.length > 0) {
          setText((cur) => {
            const next = cur + res.chunks.map((c) => c.content).join('')
            // Cap the rendered tail (full log lives server-side).
            if (next.length > 512 * 1024) {
              setTruncated(true)
              return next.slice(next.length - 512 * 1024)
            }
            return next
          })
        }
      } catch {
        // Transient poll errors are ignored; the next tick retries.
      }
    }
    poll()
    return () => {
      stop = true
    }
  }, [taskId])

  // Follow a live task: poll until live flips false (the task finished) —
  // the streamed content above is kept, only the timer stops.
  useEffect(() => {
    if (!live) return
    let stop = false
    async function poll() {
      try {
        const res = await getTaskLogs(taskId, lastSeq.current)
        if (stop) return
        lastSeq.current = res.lastSeq
        if (res.chunks.length > 0) {
          setText((cur) => {
            const next = cur + res.chunks.map((c) => c.content).join('')
            if (next.length > 512 * 1024) {
              setTruncated(true)
              return next.slice(next.length - 512 * 1024)
            }
            return next
          })
        }
      } catch {
        // Transient poll errors are ignored; the next tick retries.
      }
    }
    poll()
    const timer = setInterval(poll, 2000)
    return () => {
      stop = true
      clearInterval(timer)
    }
  }, [taskId, live])

  // Auto-scroll to the bottom on new output while following.
  const stick = useRef(true)
  useEffect(() => {
    const pre = preRef.current
    if (pre && stick.current) {
      pre.scrollTop = pre.scrollHeight
    }
  }, [text])

  return (
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
  )
}

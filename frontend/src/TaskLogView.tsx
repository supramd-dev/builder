import { useEffect, useRef, useState } from 'react'
import { getTaskLogs } from './api'

// TaskLogView shows one task's incremental log, polling with after=lastSeq
// while the parent task is live. Shared by the task detail page (following a
// running task) and the run detail page (a finished stage's stdout).
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

  useEffect(() => {
    // New selection: reset the stream.
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
    const timer = live ? setInterval(poll, 2000) : undefined
    return () => {
      stop = true
      if (timer) clearInterval(timer)
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

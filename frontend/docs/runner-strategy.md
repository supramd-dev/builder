# Runner and tasks

The server embeds the runner component: a task graph model, a scheduler and
the SSH transport to the test environments. The former sshcheck package
(SSH transport) is part of it.

## Dispatch

A webhook push (or a manual re-run via `POST /api/jobs`, see
[Webhooks](#/docs/webhooks)) reads the matrix at the pushed commit, matches
entries to enabled environments by tags and creates one **task graph** per
entry.

A graph is a root task plus a small DAG of sub-tasks:

```
root (test <sha> on <environment>)
 └─ clone repositories          # server clones, uploads a tar over SSH
     └─ build                   # cmake or custom script, in the code dir
         ├─ unit tests          # per-stage command, own timeout
         └─ regression tests    # per-stage command, own timeout
```

- Every node is a row in the same `tasks` table; the `kind` column
  distinguishes root / clone / build / unit / regression (the list is open —
  future kinds, e.g. performance tests, only add a constant and an
  executor). Dependencies are stored as JSON task IDs.
- Each node stores a **snapshot** of its config, so later YAML edits do not
  affect already-dispatched graphs.
- Graphs are keyed by (commit, environment): re-pushing the same commit
  requeues its graph (old sub-tasks and logs dropped, snapshot refreshed,
  attempts counter bumped) rather than duplicating it.
- When a sub-task fails, everything that (transitively) depends on it is
  marked **skipped**; the dashboard still shows a ✗ cell for the skipped
  test stages.

## Execution pool

- By default 2 worker goroutines claim ready sub-tasks atomically and run
  them, polling every 2 seconds. A sub-task is ready when all its
  dependencies are done. Tune with the `MD_BUILDER_WORKERS` environment
  variable; `MD_BUILDER_DISABLE_WORKER=1` disables the pool entirely
  (dispatch still records tasks).
- Tasks left in `running` after a server crash are reset to pending at
  startup and re-executed.

## Per sub-task

- **clone**: the *server* clones the code repository at the pushed commit
  and the test input repository, packs both into a tarball and streams it
  to the environment over SSH (`tar -xzf -` into
  `~/.md-builder/tasks/<sha12>`). The remote host needs **no git and no
  repository access**.
- **build**: a bash script generated from the config snapshot is streamed
  to the environment over SSH (`bash -s`) and runs cmake (or the custom
  build command) in the code directory, bounded by the configured timeout
  via the remote `timeout` command (details in
  [The test matrix](#/docs/test-matrix)). The outcome is recorded as a
  "build" test run, shown on the dashboard's build matrix.
- **unit / regression**: the same, running the stage command in the code
  directory. The stage's full output streams into the task log; a stage
  may print a line `MD-BUILDER-SUMMARY: <text>` to declare its own
  one-paragraph conclusion, otherwise the exit code plus the log tail is
  stored. The outcome is recorded as a test run (see
  [Dashboard and reporting](#/docs/dashboard)).

The script exports `MD_COMMIT`, `MD_ENV_NAME`, `MD_ENV_TAGS`,
`MD_CODE_DIR` (`…/tasks/<sha12>/code`), `MD_TEST_INPUT_DIR`
(`…/tasks/<sha12>/tests`) and the entry's `env` map.

## Logs

Every sub-task's output is stored incrementally in the `task_logs` table
(one row per chunk: at least one per 2 seconds or 32 KiB). The frontend
polls `GET /api/tasks/{id}/log?after=<seq>` for new chunks, so a running
task can be followed live. Logs are capped at 8 MiB per task; the full log
lives server-side, the UI shows the tail.

## Task API

- `GET /api/tasks/{id}` — one task; a root carries its sub-task list plus
  commit/environment context.
- `GET /api/tasks/{id}/log?after=<seq>` — log chunks after the given
  sequence (incremental, for live-following).

The dashboard matrix cells link to the task detail for graphs that have
not reported a run yet (queued / running / failed-before-report).

## Report handling

Each test stage records its run (status + summary + per-case results when
the stage command reports them) for the dashboard. A graph's root is done
when all sub-tasks are done, failed otherwise.

- A sub-task lands in **failed** (with the error on the task and in the
  task detail) when the SSH connection fails, the build fails or the stage
  command exits non-zero — the pass/fail of the *tests themselves* is
  visible on the dashboard, not in the task status.
- There is no automatic retry: re-push the commit or re-run the dispatch
  to retry.

## Prerequisites

- The **server** needs `git` on PATH and read access to both repositories
  (to read the YAML and clone) — public repos work as-is, private ones use
  the deploy key / deploy token configured in the site settings (see
  [Site configuration](#/docs/site-configuration)).
- Each **remote environment** needs `bash`, `tar`, `gzip` and `timeout`,
  plus the toolchain the build and test commands use. It needs no git and
  no repository access.

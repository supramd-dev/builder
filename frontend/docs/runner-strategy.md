# Runner strategy (worker)

The server embeds a job scheduler:

## Dispatch

A webhook push (or a manual re-run via `POST /api/jobs`, see
[Webhooks](#/docs/webhooks)) reads the matrix at the pushed commit,
matches entries to enabled environments by tags and creates one
**pending job** per entry.

- Each job stores a **snapshot** of its config, so later YAML edits do
  not affect already-dispatched jobs.
- Jobs are keyed by (commit, environment): re-pushing the same commit
  requeues its jobs (refreshed snapshot, attempts counter bumped) rather
  than duplicating them.

## Execution pool

- By default 2 worker goroutines claim pending jobs atomically and run
  them, polling every 2 seconds. Tune with the `MD_BUILDER_WORKERS`
  environment variable; `MD_BUILDER_DISABLE_WORKER=1` disables the pool
  entirely (dispatch still records jobs).
- Jobs left in `running` after a server crash are reset to pending at
  startup.

## Per job

A bash script is generated from the config snapshot and streamed to the
environment over SSH (`bash -s`). The script clones the **test input
repository** and the **code repository** at the pushed commit into
~/.md-builder/jobs/<sha> on the remote host, builds (cmake or the script
generator) and runs the unit/regression commands, each bounded by the
configured timeout via the remote `timeout` command (details in
[The test matrix](#/docs/test-matrix)).

## Report handling

The script prints a line protocol
(`===MD-BUILDER-REPORT-BEGIN===` … `unit-status passed`, `unit-summary …`,
`regression-status …` … `===MD-BUILDER-REPORT-END===`) which the worker
parses and stores as test runs — status plus a one-paragraph summary. The
dashboard cells overlay live job state (queued / running / ✗) until a run
lands (see [Dashboard and reporting](#/docs/dashboard)).

- A job lands in **failed** (with the error in the jobs list) when the
  SSH connection fails or no report is produced; a job whose build or
  stages fail but which reports properly is **done** — the pass/fail is
  visible on the dashboard, not in the job status.
- There is no automatic retry: re-push the commit or re-run the dispatch
  to retry. Clone failures of either repository mark the affected stages
  failed with the clone log tail in the summary.

## Prerequisites

- The **server** needs `git` on PATH and read access to the code
  repository (to read the YAML).
- Each **remote environment** needs `git`, `bash` and `timeout`, plus
  access to both repositories — public repos work as-is, private ones
  use the deploy key / deploy token configured in the site settings
  (see [Site configuration](#/docs/site-configuration)).

# Runner and tasks

The server embeds the runner component: a task graph model, a scheduler and
the SSH transport to the test environments. The former sshcheck package
(SSH transport) is part of it.

## Dispatch

A **task graph** is created by one of three triggers — a webhook push, a
manual yaml-matrix dispatch, or a manual custom-commands dispatch. All
share the same graph model, scheduler and environment execution; they
differ in how the commit and the stage commands are determined, and in
how repeated triggers are recorded.

- **Webhook dispatch** (automatic): a push to the configured code
  repository (see [Webhooks](#/docs/webhooks)) reads `md-builder.yaml`
  **at the pushed commit** and matches entries to enabled environments by
  tags, creating one graph per entry. Graphs are keyed by
  (commit, environment): pushing the same commit again — or re-running
  the dispatch via `POST /api/jobs` after changing environment tags or
  the YAML — requeues the existing graph: sub-tasks and logs are rebuilt
  from the fresh snapshot, and test runs of stages the new graph no
  longer contains are dropped.
- **Manual yaml dispatch** (the **Run command** page's *Manual test*
  tab, first section — one ref input and one button — or
  `POST /api/jobs/manual-yaml`): the webhook flow, started by hand for a
  ref (branch / tag / commit id, empty = HEAD) of the site-configured
  code repository. The ref is resolved, the commit recorded
  **deduplicated like a webhook push**, and the yaml matrix at it is
  dispatched exactly like a push: same graphs keyed by
  (commit, environment), so re-triggering the same ref requeues them
  with fresh snapshots (picking up yaml or environment-tag changes)
  instead of adding matrix rows. Graphs are marked `trigger: 2`.
- **Manual dispatch** (the **Run command** page's *Manual test* tab, or
  `POST /api/jobs/manual`, see
  [Dashboard & API](#/docs/dashboard)): a user-configured test — no YAML,
  no push. The repository defaults to the site-configured code repository
  (any other address can be given), an optional branch / tag / commit is
  resolved to a concrete commit before dispatch (empty = HEAD), and the
  build / unit / regression commands come from the form: an empty stage
  is simply not part of the graph (its matrix cells show "—"), and each
  stage runs with the default timeout (1 h). Any subset of the enabled environments can be
  selected (all are preselected); one graph is created per environment.
  Unlike webhook pushes, every manual dispatch records a **fresh commit
  row**: re-running the same ref gives each attempt its own matrix row
  (the commit message carries the dispatch time), and older rows of the
  same (repo, sha) are marked **superseded** — still listed on the
  dashboard, but greyed out.

The `trigger` column of a task records how its graph was created —
`0` = webhook, `1` = manual (custom commands), `2` = manual yaml — and is
surfaced on the task pages and the
matrix (a small "M" / "manual" badge marks manually created graphs —
`1` or `2`).

A graph is a root task plus a small DAG of sub-tasks:

```
root (test <sha> on <environment>)
 └─ clone repositories          # server clones, uploads a tar over SSH
     └─ build                   # the build command, in its workdir
         ├─ unit tests          # per-stage command, own timeout
         ├─ regression: heat    # one sub-task per selected preset
         └─ regression: poisson
```

- Every node is a row in the same `tasks` table; the `kind` column
  distinguishes root / clone / build / unit / regression (the list is open —
  future kinds, e.g. performance tests, only add a constant and an
  executor). Dependencies are stored as JSON task IDs.
- The regression stage is expanded at graph build time: each preset the
  entry selects (`regression.use`, minus `disable`; see
  [the test matrix](#/docs/test-matrix)) becomes its own sub-task named
  `regression: <preset>`, depending on build, with its own command,
  workdir, timeout and artifact files. All cases of an entry share **one**
  regression run per (environment, commit): each case records its own row
  (status, message, duration) and its artifact files link to that row.
- Each node stores a **snapshot** of its config, so later YAML edits or
  manual re-dispatches do not affect already-running graphs.
- When a sub-task fails, everything that (transitively) depends on it is
  marked **skipped**; the dashboard still shows a ✗ cell for the skipped
  test stages. Stages that are not part of the graph at all (an empty
  manual stage command) show "—" instead — they were never requested.

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
  (test inputs live inside it, or the code fetches them itself), packs it
  into a tarball and streams it to the environment over SSH
  (`tar -xzf -` into `~/.md-builder/tasks/<sha12>/code`), then writes the
  environment's env setup script into the task dir (if configured). The
  remote host needs **no git and no repository access**.
- **build**: a bash script generated from the config snapshot is streamed
  to the environment over SSH (`bash -s`) and runs the build command in
  the stage's working directory, bounded by the configured timeout via the
  remote `timeout` command (details in
  [The test matrix](#/docs/test-matrix)). The outcome is recorded as a
  "build" test run, shown on the dashboard's build matrix.
- **unit**: the same, running the stage command in its working directory.
  The stage's full output streams into the task log; a stage may print a
  line `MD-BUILDER-SUMMARY: <text>` to declare its own one-paragraph
  conclusion, otherwise the exit code plus the log tail is stored. The
  outcome is recorded as a test run (see
  [Dashboard and reporting](#/docs/dashboard)).
- **regression: <preset>**: one sub-task per case — the preset's command
  runs in the preset's working directory with `MD_CASE` exported; the
  case's artifact files are collected and linked to the case row. Each case
  records its row into the shared regression run; the cell aggregates.

Every stage script runs through the same preamble:

```bash
export MD_COMMIT=... MD_ENV_NAME=... MD_ENV_TAGS=...
export MD_TASK_DIR=~/.md-builder/tasks/<sha12> MD_CODE_DIR=$MD_TASK_DIR/code
export MD_CASE=...                    # regression case sub-tasks only
export <entry env map>
ENV_SCRIPT="$MD_TASK_DIR/md-builder-env-<hash>.sh"
if [ -f "$ENV_SCRIPT" ]; then . "$ENV_SCRIPT"
else echo "warning: env script $ENV_SCRIPT not found; continuing without it" >&2; fi
cd "$MD_CODE_DIR/<workdir>"           # empty workdir = the code dir
<stage command under timeout>
```

The env script is sourced **last**, so it can override the exported
variables (see [Test environments](#/docs/environments)). Each stage is a
separate SSH session, which is why the preamble re-runs for every one.

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

Each test stage records its run for the dashboard: the status, a summary,
the aggregate counts and — when the stage configured `artifacts` files —
each raw artifact file stored as its own artifact. A graph's root is done
when all sub-tasks are done, failed otherwise.

- A sub-task lands in **failed** (with the error on the task and in the
  task detail) when the SSH connection fails, the build fails or the stage
  command exits non-zero — the pass/fail of the *tests themselves* is
  visible on the dashboard, not in the task status.
- **Unit runs** fail when the command exited non-zero **or** the parsed
  artifact files report failed cases (ctest-style wrappers can swallow the
  test binary's exit code).
- **Regression cases** are judged by their command's exit status alone
  (exit 0 → passed, anything else — timeout, SSH failure, non-zero — →
  failed); their `artifacts` files are stored for display and never flip
  the verdict. The run aggregates its cases: any failed case → the cell
  shows ✗ (see [the test matrix](#/docs/test-matrix)).
- Unit runs carry aggregate counts only (total / passed / failed /
  skipped), summed across all configured artifact files. The per-case list
  is parsed in the browser from the stored artifact files (see [the test
  matrix](#/docs/test-matrix)); the run's `taskId` links back to the
  stage's task log (stdout).
- Regression runs aggregate **incrementally**: each case sub-task upserts
  its own row (re-runs of the same case replace it), and the run's
  counts/status/summary are recomputed over all rows seen so far ("3/4
  cases passed; failed: heat"). A re-dispatch resets the run before the
  new cases land.
- There is no automatic retry: re-push the commit or re-run the dispatch
  to retry.

### Artifacts and the regression extension

Result files live in one `test_artifacts` table keyed by run — a run (or a
single case) can have several — with a `case_id` column that is 0 for
run-level files and set for the artifact files a regression case collects.
This is the extension point for regression tests: their per-case logs and
series/plot data will be stored as `log` / `series` artifacts behind the
same table, fetched by an "analyze" view in the browser, while per-case
outcomes (status, error value, duration) go through the regular
case-result rows.

## Prerequisites

- The **server** needs read access to the code repository (to read the
  YAML and clone) — public repos work as-is, private ones use the Project
  Access Token configured in the site settings (see
  [Site configuration](#/docs/site-configuration)). Cloning happens
  in-process via go-git: no `git` binary is required.
- Each **remote environment** needs `bash`, `tar`, `gzip` and `timeout`,
  plus the toolchain the build and test commands use. It needs no git and
  no repository access.

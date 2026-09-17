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
  parent regression run per (environment, commit); each case is recorded
  as a **child test run** under it (status, message, duration) and the
  case's artifact files attach to that child run.
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
  case's artifact files are collected onto the case's child run. Each case
  records its child run under the shared parent regression run; the parent
  aggregates.

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

The dashboard matrix cells link to the run detail whenever a run exists
(placeholder runs cover every queued/running stage — see
[Live runs](#live-runs-and-the-placeholder-lifecycle)); only a cell with
no run at all falls back to the task detail.

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
  failed); their `artifacts` files are stored on the case's child run for
  display and never flip the verdict. The parent run aggregates its
  cases: any failed case → the cell shows ✗ (see
  [the test matrix](#/docs/test-matrix)).
- Unit runs carry aggregate counts only (total / passed / failed /
  skipped), summed across all configured artifact files. The per-case list
  is parsed in the browser from the stored artifact files (see [the test
  matrix](#/docs/test-matrix)); the run's `taskId` links back to the
  stage's task log (stdout).
- Regression runs aggregate **incrementally**: each case sub-task upserts
  its child run (re-runs of the same case replace it), and the parent
  run's counts/status/summary are recomputed over all child runs seen so
  far ("3/4 cases passed; failed: heat"). A re-dispatch resets the run
  before the new cases land.
- There is no automatic retry: re-push the commit or re-run the dispatch
  to retry.

### Artifacts and the regression extension

Result files live in one `test_artifacts` table keyed by run — a run can
have several. Artifacts belong to the run that produced them: a unit
stage's results files attach to the unit run; a regression case's files
attach to the case's own child run. Regression cases are nested runs, so
this is the natural extension point: their per-case logs and series/plot
data will be stored as `log` / `series` artifacts behind the same table
and fetched by an "analyze" view in the browser.

### Runs with and without a task link

A run's `taskId` (and the derived `rootTaskId`) decides what its detail
page can show. Two kinds of runs exist:

- **Graph-linked runs** (`taskId ≠ 0`): reported by the runner, they carry
  the stage sub-task's ID. Their detail page shows the stage's stdout log
  (`GET /api/tasks/{taskId}/log`) and a breadcrumb link to the task graph;
  the dashboard cell links to the run detail. Regression case **children**
  carry their own case sub-task's ID, so each case row opens a run detail
  with that case's log.
- **External reports** (`taskId = 0`, `rootTaskId = 0`): a run recorded
  without a task graph behind it — e.g. a CI system pushing its results
  through the report API directly. There is no server-side task, hence no
  stdout log and no graph page; the detail page shows only the run's own
  summary, counts, artifacts and (for regression) the case list. The
  dashboard cell links to the run detail as well, and falls back to the
  task detail only when no run exists.

## Live runs and the placeholder lifecycle

A graph-linked run exists **before** its stage executes. At dispatch time
the runner seeds a **placeholder run** per stage kind (status `pending`,
linked to the stage's first sub-task via `taskId`; regression additionally
gets one `pending` child run per preset, so the detail page lists every
case from the start):

```
dispatch    claim          outcome
pending  →  running    →   passed/failed (stage report)
                         ↘ skipped      (an upstream stage failed)
```

- When the scheduler claims the stage sub-task, its placeholder flips
  `pending → running` — the matrix cell shows the spinner and the run
  detail page follows live (3 s run polling, 2 s log polling).
- When the stage finishes, its report **replaces** the placeholder in
  place (same row, every field overwritten): the status becomes terminal
  and the log stops growing.
- When an upstream stage fails, the skipped stage's placeholder is
  replaced by a failed run whose summary starts with `skipped: …` (and the
  regression children by skipped child runs). A placeholder that fails to
  even build its stage script is closed out the same way.
- The dashboard cell therefore links to the **run detail in every state**
  (queued, running, passed, failed, skipped); the task graph is reachable
  from there via the breadcrumb. Only cells without any run at all (a
  stage not part of the graph, "—") have nothing to link to.
- `pending`/`running` are the only non-terminal run statuses; anything
  else is final until a re-dispatch resets it (the placeholder lifecycle
  above starts over).

## Demo seeds and frozen live graphs

The `seed` subcommand populates a demo database with two kinds of graphs,
which are also the reference for how the two scheduling modes behave:

- **Finished graphs** (older commits): terminal tasks with log chunks, and
  their runs reported with the matching `taskId` links. Nothing here is
  claimable.
- **Live graphs** (the newest commit): an in-flight snapshot — one graph
  mid-build (clone done, build `running` with partial output, tests
  queued), one fully queued. Their placeholder runs carry real task IDs,
  so the matrix cells, run detail pages and log polling all behave exactly
  like a genuine dispatch.

The scheduler treats the live graphs like any other graph; the difference
is entirely in how the demo is served:

- With `MD_BUILDER_DISABLE_WORKER=1` the pool is off: the snapshot is
  frozen forever — no task is ever claimed, placeholder runs stay
  pending/running, logs stop growing. Use this for a stable demo.
- With workers enabled, the pending stages of a live graph **are claimed
  and genuinely executed** (and fail on the unreachable demo SSH host):
  the placeholders flip through running to failed, dependents are skipped
  and reported, and the root becomes failed. Note that a restart also
  resets `running` tasks to pending (crash recovery), so a frozen
  "running" demo becomes schedulable again whenever the pool is on.

## Prerequisites

- The **server** needs read access to the code repository (to read the
  YAML and clone) — public repos work as-is, private ones use the Project
  Access Token configured in the site settings (see
  [Site configuration](#/docs/site-configuration)). Cloning happens
  in-process via go-git: no `git` binary is required.
- Each **remote environment** needs `bash`, `tar`, `gzip` and `timeout`,
  plus the toolchain the build and test commands use. It needs no git and
  no repository access.

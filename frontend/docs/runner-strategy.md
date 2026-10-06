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
  the YAML — **requeues** the existing graph rather than replacing it.
  Nodes are matched by their node key (the stage kind, or
  `regression:<case>`), so a re-dispatch opens a fresh attempt on the
  nodes that are still defined with the fresh config snapshot, leaves the
  history of the earlier attempts where it is, and **retires** — never
  deletes — the nodes the new YAML no longer defines: a dropped case keeps
  its runs, logs and artifacts, stops being scheduled and drops out of
  the aggregate. One revision can be requeued this way without anyone
  triggering anything by hand: pushing a branch and then opening an MR
  for it dispatches the same (commit, environment) graph twice on one
  commit row, and the second dispatch restarts whatever was still
  running (see [Webhooks](#/docs/webhooks)). The YAML read itself is one
  request for one file through
  the code host, with the full clone as a fallback, and is capped by a
  timeout — so a dispatch never scales with the repository, and a silent
  repository server cannot hold it open (see
  [Webhooks](#/docs/webhooks)).
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
  stage runs with the default timeout (1 h). Any subset of the environments
  you may use — your own, plus every one for an administrator — can be
  selected (all are preselected); one graph is created per environment.
  Unlike the yaml flow this is not a tag match: the environment is named by
  the caller, and the endpoint refuses an environment the caller may not
  manage (`403`), because the stage commands run there with its owner's
  private key. Unlike webhook pushes, every manual dispatch records a **fresh commit
  row**: re-running the same ref gives each attempt its own matrix row
  (the commit message carries the dispatch time), and older rows of the
  same (repo, sha) are marked **older** (`superseded` in the API) — still
  listed on the dashboard, and greyed out once nothing in them can change
  any more (the same treatment the `fork` policies give a webhook event
  that opens a row of its own — see
  [Site configuration → Repeated commits](#/docs/site-configuration)).

The `trigger` column of a task records how its graph was created —
`0` = webhook, `1` = manual (custom commands), `2` = manual yaml — and is
surfaced on the task pages and the
matrix (a small "M" / "manual" badge marks manually created graphs —
`1` or `2`).

**Which environments each entry point draws on.** Environments are a shared,
site-wide pool (see
[Test environments](#/docs/environments)), so the yaml flow and the two
pickers of the **Run command** page do not cover the same set:

| Entry point | Environments |
|---|---|
| Webhook push, **manual yaml dispatch** | every **enabled** environment of the site, one per yaml entry, matched by tags — a run may land on a machine another account registered |
| **Exec / script** tab (**Run command**) | the environments you may use: your own, plus every one for an administrator — the command logs in with the environment's private key |
| *Manual test* tab, **manual dispatch** | the same set (the picker, and the endpoint refuses a foreign id); the environment is chosen explicitly, not matched |

The Runner Envs list and the dashboard matrix always show the whole pool,
labelled with each environment's owner.

A graph is a root task plus a small tree of sub-tasks:

```
root (test <sha> on <environment>)   # virtual: the whole pipeline
 ├─ clone repositories               # server clones, uploads a tar over SSH
 ├─ build                            # the build command, in its workdir
 ├─ unit tests                       # per-stage command, own timeout
 └─ regression       (virtual)       # the stage container: aggregates its cases
     ├─ regression: heat             # one sub-task per selected preset
     └─ regression: poisson
```

- Every node is a row in the same `tasks` table; the `kind` column
  distinguishes root / clone / build / unit / regression (the container) /
  regression_case (the list is open — future kinds, e.g. performance
  tests, only add a constant and an executor).
- Two relations are stored, and they mean different things. `parent_id` is
  the **tree**: where a node nests, which is what the graph page draws and
  what the rollup walks. `depends_on` is the **scheduling DAG**: a JSON
  array of task ids that must be finished before the node is ready. A
  container is never a dependency (its state is derived, so an edge to it
  could never become ready) and neither is the root: `UpsertTaskGraph`
  rejects both. In practice a case depends on build (or on clone when the
  entry has no build stage), and everything else depends on the node
  before it.
- The regression stage is expanded at graph build time: each preset the
  entry selects (`regression.use`, minus `disable`; see
  [the test matrix](#/docs/test-matrix)) becomes a **real task of its own**
  named `regression: <preset>`, nested under the virtual container,
  depending on build, with its own command, workdir, timeout and artifact
  files. Cases are tasks rather than rows of a parent's result, which is
  what lets them run in parallel, keep a log and an attempt history of
  their own, and survive a re-dispatch.
- The virtual nodes — the root and the regression container — run nothing
  and record nothing: their status, counts, summary and timestamps are
  always **rolled up** from their children, so they cannot drift from what
  the stages did. A container counts real children as one unit each and a
  virtual child by its own children's tally, so the regression stage's
  numbers are the cases' and the root's are the whole pipeline's.
- Each node stores a **snapshot** of its config, so later YAML edits or
  manual re-dispatches do not affect already-running graphs.
- When a sub-task ends without passing — it failed, it timed out, or it was
  itself reported `skipped` — everything that (transitively) depends on it
  is marked **skipped**, with the reason in its summary and a matching line
  in its log (`skipped: upstream task build failed`, or `upstream task
  build timed out`). A dependency must
  pass before the node behind it is ready, so a node that will never pass
  leaves nothing to wait for: the skip happens in the same transaction
  that records the outcome, and no queue is left that cannot drain.
  `skipped` is a real status of the same vocabulary as `passed`, `failed`
  and `timeout`, not a display convention: the node never ran, and the
  dashboard says so. Stages that are not part of the graph at all (an
  empty manual stage command, or a case a later dispatch dropped) are not
  "skipped" — they are simply not there, and their cells show "—".
- A node can also end as **cancelled**, which is a decision about the *run*,
  not about the code: nothing about a cancelled node was judged. There are
  two reasons for one, and the summary names which: the site's
  repeated-commit policy dropped the work because a newer dispatch of the
  same revision replaced it (`fork_cancel`, see
  [Site configuration → Repeated commits](#/docs/site-configuration)) —
  summary `cancelled by a newer dispatch of this commit` — or somebody
  stopped it by hand (`POST /api/tasks/{id}/cancel`, from the run page or
  the task page) with the summary `cancelled by <username>`. Only
  unfinished work is ever cancelled: a node that had already passed keeps
  its status, its result and its log. Cancelling is **absorbing**: a report
  that arrives afterwards (the aborted stage's own outcome, or a late write
  from another worker) is refused by the store instead of reopening the node
  on a fresh attempt, so cancelled work cannot come back to life.
- What is waiting on a cancelled node cannot run any more — the queue only
  hands out nodes whose dependencies all passed — so it is **skipped** in
  the same transaction, exactly as it is behind a failed node, with the
  reason naming the cancelled task (`upstream task build was cancelled`).
  Nothing is left queued that could never be claimed, so a graph with a
  cancelled stage still settles. A hand cancellation is otherwise narrow:
  stopping the regression container drops its unfinished cases (running
  ones included) and leaves the build and unit stages beside it alone;
  stopping a root drops that graph.

## Execution pool

- By default 2 worker goroutines claim ready sub-tasks atomically and run
  them, polling every 2 seconds. A sub-task is ready when all its
  dependencies are done. The pool size is `worker.count` in the server
  config file (or `MD_BUILDER_WORKERS`); `worker.enabled: false` (or
  `MD_BUILDER_DISABLE_WORKER=1`) disables the pool entirely (dispatch still
  records tasks).
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
- **regression: <preset>**: the preset's command runs in the preset's
  working directory with `MD_CASE` exported, and the case's artifact files
  are collected onto the case's **own** run. Nothing is written to a parent
  run: the regression container above is virtual, and its "N/M cases
  passed; failed: heat" summary is the rollup of the case nodes under it.
- A stage that was **skipped** never opens an SSH session at all: the
  scheduler closes its pending attempt with the reason, which is one log
  line and a summary — so a stage stopped by an upstream failure is
  distinguishable from one that ran and failed.

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

Every sub-task's output is stored incrementally in the `task_logs` table,
**keyed by (task, attempt, seq)**: one row per chunk, at least one per 2
seconds or 32 KiB. Keying on the attempt is what keeps a retry's output
apart from the try before it — following the current attempt and reading
an earlier one are the same query with a different `attempt`. The frontend
polls `GET /api/tasks/{id}/log?after=<seq>` for new chunks, so a running
task can be followed live; `GET /api/tasks/{id}/log/download` streams the
whole attempt as one text file, built from the stored chunks a batch at a
time. A read returns one page (1000 chunks) and the viewer asks for the
next one while pages come back full, so a long log is read to its end
rather than up to wherever the first page stopped; when a stage finishes
the viewer reads once more before the timer stops, which is what shows
the last lines and the summary the stage ended with. Logs are capped at
8 MiB per attempt; the full log lives server-side, the UI shows the tail.

## Task API

- `GET /api/tasks/{id}` — one task; a root carries its node list (the
  current graph plus the retired nodes a later dispatch dropped) and the
  commit/environment context.
- `GET /api/tasks/{id}/runs` — the task's attempts, newest first; the log
  viewer's attempt switcher. Virtual tasks answer with an empty list.
- `GET /api/tasks/{id}/log?after=<seq>&attempt=<n>` — log chunks after the
  given sequence (incremental, for live-following). Without `attempt` the
  current one is read.
- `GET /api/tasks/{id}/log/download?attempt=<n>` — the attempt's whole log
  as a `text/plain` attachment.
- `GET /api/tasks/{id}/artifacts/zip` — the subtree's latest attempt at the
  archive root, every descendant's files under a directory named after it;
  a regression stage downloads as one bundle of its cases (see
  [Dashboard and reporting](#/docs/dashboard)).
- `POST /api/tasks/{id}/cancel` — stop the task's unfinished work and
  everything below it (one stage, a container's cases, or the whole graph):
  the one **write** here, and the one write in the API that any signed-in
  user may perform. It closes the in-flight attempt's run as `cancelled`
  and aborts the stage a worker is executing for it; a task with nothing
  left to stop is a `409` (see
  [API → Cancelling running work](#/docs/api)).

Reading any of these is open to every signed-in user: the matrix is
site-wide, so its drill-down pages are too. A virtual node's cell in the
matrix carries `runId` 0 (it has no run), so those link to the task page
instead — and every real node has a run from the moment it is dispatched,
see below.

## Report handling

A stage's outcome is written in exactly one place: `store.FinishAttempt`,
which records the attempt's run (status, summary, error, counts, artifacts,
timings), refreshes the node's cached values from the same numbers and
rolls the virtual containers back up. The runner calls it in process when a
stage ends; `POST /api/test-runs` calls it for a report that arrives over
HTTP. One write path means the node, its run and the containers above it
can never disagree.

- A sub-task lands in **failed** (with the error on the task and in the
  task detail) when the SSH connection fails, the build fails or the stage
  command exits non-zero — the pass/fail of the *tests themselves* is
  visible on the dashboard, not in the task status.
- A stage command that outlives its timeout lands in **timeout** instead:
  the same gate, its own status, with the timeout and the command in the
  error column, a `task timed out: …` line in the log, and the log tail as
  the summary's tail. A container whose failures are all timeouts takes
  the status too, so a pipeline stopped by the clock reads as one.
- **Unit runs** fail when the command exited non-zero **or** the parsed
  artifact files report failed cases (ctest-style wrappers can swallow the
  test binary's exit code). They carry aggregate counts only (total /
  passed / failed / skipped, with `total` counting every case once,
  skipped included), summed across all configured artifact files; the
  per-test list is parsed in the browser from the stored artifact file.
- **Regression cases** are judged by their command's exit status alone
  (exit 0 → passed, anything else — SSH failure, non-zero → failed, and a
  timeout → `timeout`); their `artifacts` files are stored on the case's
  own run for display and never flip the verdict. The container above them
  rolls up: any failed case fails the stage, and its summary reads
  "3/4 cases passed; failed: heat" — or "1/4 cases passed; timed out:
  poisson" when every failure under it was a timeout.
- A container's summary always describes its rollup rather than a report:
  "3/4 cases passed; 1 in progress", "2/4 cases passed; 2 queued",
  "4/4 cases skipped (upstream failure)", "4/4 cases passed".
- There is no automatic retry: re-push the commit or re-run the dispatch
  to start a new attempt of every node.

### Artifacts and the regression extension

Result files live in one `test_artifacts` table keyed by run — a run can
have several. Artifacts belong to the run that produced them: a unit
stage's results files attach to the unit run; a regression case's files
attach to the case's own run. Because a case is a task with a run of its
own, this is where the regression extension lands without a new concept:
per-case logs and series/plot data will be stored as `log` / `series`
artifacts behind the same table, hanging off the case's run, and fetched by
an "analyze" view in the browser.

## The attempt lifecycle

A run is created **before** its stage executes, because the node it belongs
to is created with it. At dispatch time every real node opens an attempt
(`BeginAttempt`): the node goes back to `pending` with empty counts, and an
attempt-N run in status `pending` is created for it:

```
dispatch          claim                          outcome
pending (run)  →  running            →        passed/failed/timeout/skipped
                  (ClaimReadyTask)            (FinishAttempt)
                                                cancelled (the policy, or a
                                                hand cancellation)
```

- The scheduler claims ready nodes atomically (`ClaimReadyTask`), flipping
  the node and its in-flight run to `running` in one transaction, and rolls
  the containers back up in the same one — a graph whose stage runs reads
  `running`, not queued. The matrix cell shows the spinner and the run page
  follows live (3 s run polling, 2 s log polling). The scheduler looks at
  the whole queue rather than a fixed window of it, so a ready node is
  handed out however many unclaimable ones sit in front of it.
- When the stage ends, `FinishAttempt` closes that run with the outcome:
  the status becomes terminal, the log stops growing, and the artifacts the
  runner fetched back are attached to the run — the attempt that produced
  them, not the task as a whole.
- When an upstream stage fails or is reported `skipped`, the dependents'
  pending attempts are closed as **skipped** with the reason (one summary,
  one log line) in the same transaction, and the containers roll up to
  `skipped` or `failed` accordingly.
- A report for a task whose attempt already ended does **not** overwrite it:
  the store opens the **next attempt** — a new run, with the task's counter
  bumped — and both stay readable. That is the retry path, and it is why a
  run's `attempt` number is part of its identity: `(task, attempt)` is
  unique, logs are keyed by it, and the run page can offer the earlier
  tries.
- A **re-dispatch** while a stage is in flight re-arms the node on a new
  attempt and closes the displaced attempt's run (status `skipped`,
  summary "superseded by a new dispatch of this task" — the word names the
  attempt, not the greyed-out commit row of a re-dispatched SHA): nothing
  else would ever close it. The runner reports the attempt it actually ran
  — the report names its attempt number — so the old attempt keeps the
  real outcome and the new one is left for the scheduler to run for real.
- A **cancellation** (`fork_cancel`, or the hand cancellation of
  `POST /api/tasks/{id}/cancel`) closes the in-flight attempt's run as
  `cancelled` with the summary and cancels the node — in one transaction, so
  a reader never sees a cancelled node with a live run — and skips whatever
  was waiting on it, in that same transaction. The stage the runner was
  executing is aborted at the same time (`Service.CancelSubtree` asks the
  store first and then closes the SSH session under the stages it dropped
  locally, which is the only thing a store cannot do). When that stage's
  result arrives afterwards, `FinishAttempt` refuses it
  (`the task was cancelled`) rather than letting a stage verdict overwrite
  the cancellation. Cancelled nodes are terminal, so nothing claims them
  again and no queue is left holding them.
- `pending`/`running` are the only non-terminal statuses; anything else is
  final. Re-running a dispatch opens fresh attempts; nothing is edited in
  place. Absorbing is what `cancelled` does to a *report*, not to a
  dispatch: while the policy that dropped the work is in force, that row is
  never dispatched again — the newer dispatch has a row of its own, which is
  what "run this revision again" means under `fork` and `fork_cancel` — but
  a row that does get dispatched a second time (the site went back to
  `requeue`, so a further push of that revision deduplicates onto it) runs
  its whole graph afresh on new attempts, cancelled nodes included, with the
  cancelled attempt kept as history.

## Demo seeds and frozen live graphs

The `seed` subcommand populates a demo database with two kinds of graphs,
which are also the reference for how the two scheduling modes behave. It
builds them with the production graph builder and finishes them through
the same store calls the runner uses (`FinishAttempt`, and the skip path
for stages an upstream failure stopped), so a seeded matrix is
indistinguishable from a dispatched one — including the attempts, the
per-attempt logs and the artifacts.

- **Finished graphs** (older commits): every real node on a terminal
  attempt, with its log chunks and (for the unit and build stages) the
  files the runner would have fetched back. Nothing here is claimable.
- **Live graphs** (the newest commit): an in-flight snapshot — one graph
  mid-build (clone finished, build `running` with partial output, the
  test stages still queued), one not claimed at all. Their attempts are
  real runs in `pending`/`running`, so the matrix cells, the run pages and
  log polling behave exactly like a genuine dispatch.

The scheduler treats the live graphs like any other graph; the difference
is entirely in how the demo is served:

- With `worker.enabled: false` (or `MD_BUILDER_DISABLE_WORKER=1`) the pool
  is off: the snapshot is frozen forever — no node is ever claimed, the
  pending/running attempts stay where they are, logs stop growing. Use
  this for a stable demo.
- With workers enabled, the pending stages of a live graph **are claimed
  and genuinely executed** (and fail on the unreachable demo SSH host):
  the attempts flip through running to failed, dependents are skipped and
  reported, and the root becomes failed. Note that a restart also resets
  `running` tasks to pending (crash recovery), so a frozen "running" demo
  becomes schedulable again whenever the pool is on.
- Re-seeding is idempotent: commits are deduplicated by (repo, sha) and a
  cell whose graph is already finished is left alone (`-force` deletes the
  demo environments' tasks, with their runs, logs and artifacts, and
  rebuilds every graph from scratch).

## Prerequisites

- The **server** needs read access to the code repository (to read the
  YAML and clone) — public repos work as-is, private ones use the Project
  Access Token configured in the site settings (see
  [Site configuration](#/docs/site-configuration)). Cloning happens
  in-process via go-git: no `git` binary is required.
- Each **remote environment** needs `bash`, `tar`, `gzip` and `timeout`,
  plus the toolchain the build and test commands use. It needs no git and
  no repository access.

# Dashboard, reporting and API

## The test dashboard

The dashboard (first tab after login) shows a build.golang.org-style
matrix: one **row per recent git push** (default 10, capped at 50 via
`?commits=`, newest first), one **column per test environment**
(site-wide — all environments configured by any user; disabled ones are
greyed out). Four views are available via the tabs at the top:

- **All** — the full pipeline matrix (`GET /api/dashboard/full`): each
  cell shows the three stages in display order (build, unit tests,
  regression) for that (commit, environment), each with its own status
  and detail link.
- **Build** — whether the code compiles per environment (click through
  for the build log).
- **Unit tests** / **Regression tests** — the per-case matrices.

Every cell is one **stage node of that (commit, environment)'s task
graph** — not a row of a results table. Its status, pass/fail counts and
timestamps are those of the node's latest attempt, and clicking it opens
that attempt's run detail: the per-case results (name, status, error
value, short note), the one-paragraph summary the runner wrote, the log
and the artifacts. The vocabulary is the task vocabulary throughout —
`pending`, `running`, `passed`, `failed`, `timeout`, `skipped` — so a
stage whose
upstream failed (or was itself reported `skipped`) reads `skipped` (the
reason is in its summary, and its log holds the single line
`skipped: <reason>`), a stage that outlived the timeout the yaml gave it
reads `timeout` (its summary and log name the bound; the stages behind it
are skipped as if it had failed), and a stage the workers
have not claimed yet reads `pending` (shown as queued). A cell is
**null** when the commit has no graph on that environment, or when the
graph does not define that stage at all; the UI renders both as "—",
because from the matrix's point of view a stage that was never requested
and a commit that was never dispatched look the same.

The **regression column is the graph's virtual container**: it runs
nothing, so it has no attempt and no run of its own — its cell carries
`runId` 0, its counts are the cases' aggregate, and clicking it opens the
task page, where every case is a node of its own with its own status,
log, attempt and artifacts. That is what makes a case's history survive a
re-dispatch instead of being rewritten by whichever case reported last.

Every commit row also carries a **graph** link: the dependency graph of
that commit's task pipeline (clone → build → unit/regression),
GitHub-Actions style — clicking a stage node jumps to its run detail or
the live task log (see [Runner and tasks](#/docs/runner-strategy)).

When a commit has **no graph at all** because the dispatch failed — an
unreadable `md-builder.yaml`, an invalid one, no entry matching an
environment, no code repository configured — the graph column shows a
warning triangle in place of the link: hovering it reveals the reason and
clicking opens the full text. The message is stored on the commit row
(`dispatchError`, set at dispatch time and cleared by a later dispatch
that works), so it outlives the webhook response that carried it.

Manually dispatched graphs carry a small **M** badge in their cells and
a "manual" label on the task pages. Rows of manual dispatches that were
re-run (a newer attempt of the same commit exists) are kept but greyed
out with a **superseded** tag — only the newest attempt of a commit is
live.

The full matrix response shape:

```json
{
  "environments": [{"id": 1, "name": "cpu-node-1", "...": "..."}],
  "rows": [
    {
      "commit": {"sha": "abc123", "...": "..."},
      "taskIds": {"1": 42},
      "triggers": {"1": 1},
      "stages": {
        "1": [
          {"kind": "build", "runId": 7, "taskId": 43, "status": "passed"},
          {"kind": "unit", "taskId": 44, "status": "running"},
          {"kind": "regression", "taskId": 45, "status": "pending",
           "summary": "0/4 cases passed; 4 queued"}
        ]
      }
    }
  ]
}
```

Each stage always carries the `taskId` of the node it shows — that is
what makes the cell clickable — and the `runId` of that node's latest
attempt when it has one; the virtual regression container has none, so
its `runId` is 0 and its `summary` counts the cases. `taskIds` maps the
environment to the root task id for the graph link, `triggers` to that
root's trigger (0 = webhook, 1 = manual, 2 = manual yaml), and
`commit.dispatchError` carries the recorded reason when the dispatch
produced no graph at all.

## Reporting results

Results are reported with `POST /api/test-runs`, against the **task whose
attempt the report closes**:

```json
{
  "taskId": 44,
  "status": "failed",
  "summary": "max relative error above tolerance on 2 of 12 cases",
  "total": 12, "passed": 9, "failed": 2, "skipped": 1,
  "durationMillis": 4200,
  "startedAt": "2026-09-08T03:00:00Z",
  "finishedAt": "2026-09-08T03:04:00Z"
}
```

- `taskId` is the only required field. Environment, commit and run kind
  are read from the task, so a report cannot land anywhere else — and,
  because the task is what the caller has to be entitled to, the
  endpoint can tell a legitimate report from somebody else's.
- Everything else is optional. An empty `status` is derived from the
  counts (`failed` when anything failed; `skipped` when the total is
  non-zero and every one of them was skipped; `passed` otherwise) and an
  empty `startedAt` is derived from `finishedAt` and `durationMillis`.
  A `status` outside `passed` / `failed` / `timeout` / `skipped` is a
  `400`. A reporter that can tell a timeout from a failure says so
  itself — the derived status never guesses `timeout`, because a clock
  running out and a command exiting non-zero look the same in the counts.
- The report must come from somebody entitled to that test: the **owner
  of the environment the task runs on, or an administrator**. Any other
  account gets `403`, because otherwise any signed-in user could
  overwrite any test's result by guessing a task id. An unknown task is
  `404`, and a **virtual task** is `400` — the root and the regression
  container record no runs, so a report has to name the child that
  actually ran ("virtual tasks do not record runs; report against their
  children"). There is no "external report" row any more: every run
  belongs to a task.
- The report closes the attempt the store has **in flight**. A second
  report after that attempt ended does not overwrite it — it opens the
  **next attempt** of the same task, which is the retry path: a re-run
  keeps both records and both stay readable. The response is `201` with
  the new run, whose `attempt` is the attempt that was opened.
- There is no per-case list and no artifact field in the body. A
  regression case is a task of its own, so a case's outcome is that
  node's report; its files are fetched back by the runner, which is the
  process that has the connection to the machine.
- The runner does not go through HTTP at all: it runs in the server
  process and closes its attempts through the store (`FinishAttempt`),
  with the same result values. This endpoint is the entry point for
  testers outside the server (see
  [Runner and tasks](#/docs/runner-strategy)).
- Deleting an environment deletes its tasks, runs and artifacts.

## Runs, tasks, logs and artifacts

`GET /api/test-runs/{id}` is the detail view of **one attempt of one
task**: the run itself — `id`, `taskId`, `attempt`, `kind` (the stage
kind, `build` / `unit` / `regression`; a case's run carries the stage's
kind, which is what puts every case in the one regression column),
`status`, `summary`, the
counts, `durationMillis`, `environmentId`, `commitId`, `startedAt`,
`finishedAt` — plus the identity of the task it belongs to (`taskName`,
`taskDescription`, `taskKey`, `taskKind`, `rootTaskId` — the link back to
the pipeline page), its commit and environment context, every other
attempt of the same task in `attempts` (newest first, same shape as the
run) and the attempt's `artifacts`:

```json
{
  "id": 12, "taskId": 44, "attempt": 2, "kind": "unit",
  "status": "failed", "summary": "9/12 passed; failed: models",
  "total": 12, "passed": 9, "failed": 2, "skipped": 1,
  "durationMillis": 4200, "environmentId": 1, "commitId": 7,
  "startedAt": "2026-09-08T03:00:00Z",
  "finishedAt": "2026-09-08T03:04:00Z",
  "taskName": "unit tests", "taskDescription": null,
  "taskKey": "unit", "taskKind": "unit", "rootTaskId": 42,
  "environmentName": "cpu-node-1", "commitSha": "abc123…",
  "commitShortSha": "abc123", "commitMessage": "fix …",
  "commitAuthor": "…", "commitRepo": "group/code",
  "commitRepoUrl": "https://gitlab.example.com/group/code",
  "attempts": [
    {"id": 12, "attempt": 2, "status": "failed", "...": "..."},
    {"id": 9,  "attempt": 1, "status": "passed", "...": "..."}
  ],
  "artifacts": [
    {"id": 3, "kind": "results",
     "name": "build/test_detail.xml", "size": 15832}
  ]
}
```

The task-identity and commit/environment fields are `null` when the row
they name was deleted — a run outlives the environment it ran on, so an
old run page stays readable. Reading is like the matrix itself, which is
site-wide: any signed-in user may open any task, run, log or artifact.
Only the writes are restricted — reporting to a task (the environment's
owner or an administrator) and dispatching onto an environment. Reading
is where the ownership model does not apply, so the pages stay useful
for a colleague asking why a case failed. Artifacts belong to the attempt that
produced them: a unit run's results files attach to the unit run, a
regression case's files to that case's own run — the regression
container, which produces nothing, only aggregates its children's
counts.

`GET /api/tasks/{id}` is the test-side view of a node. It carries the
task's identity (`id`, `rootId`, `parentId`, `kind`, `nodeKey`, `name`,
`description`, `virtual`, `retired`), its state (`status`, `summary`,
`error`, counts, `attempts`, `startedAt`, `finishedAt`), where it ran
(`commitId`, `environmentId`, `tags`, `trigger`, plus the resolved
`commit` and `environment`) and, for a **real** task, every attempt in
`runs` (newest first). `GET /api/tasks/{id}/runs` returns that list on
its own as `{"runs": […]}` — it is what the log viewer's attempt
switcher reads — and answers `{"runs": []}` for a virtual task.

A **root** answers with the graph instead of attempts:

- `subTasks` — the nodes of the current graph: `id`, `parentId` (the
  nesting: the tree the UI draws), `kind`, `nodeKey` (the stable identity
  a re-dispatch matches on), `name`, `description`, `virtual`, `status`,
  `summary`, `error`, `dependsOn` (the scheduling DAG, always a list,
  never null), the counts, `attempts`, `runId` (the latest attempt's run,
  the detail link; absent for a container) and timestamps.
- `retiredTasks` — the nodes an earlier dispatch defined and a later one
  dropped. They are kept, never rescheduled and excluded from the rollup:
  when a case disappears from `md-builder.yaml` its history does not
  vanish from the page, and re-adding the case does not resurrect the old
  node.

Logs are stored per attempt as ordered chunks and read incrementally:
`GET /api/tasks/{id}/log?after=<seq>` answers
`{"attempt": 2, "chunks": [{"seq": 4, "content": "…"}], "lastSeq": 9}` —
the chunks written after `after`, which is what a viewer following a
running task asks for. Both log endpoints take `?attempt=<n>`, defaulting
to the current attempt, so an earlier try stays readable after a retry. A
negative `after` or a non-positive `attempt` is a `400`, not a silent
default. One read returns at most 1000 chunks: a reader continues from
`lastSeq` until a page comes back short, which is how the log viewer
shows a long attempt and how the download walks it.
`GET /api/tasks/{id}/log/download` streams the whole attempt as
one `text/plain` file attachment — `task-<id>.log`, with an
`-attempt-<n>` suffix for anything but the current attempt — built from
the stored chunks batch by batch, so a long build's output never has to
fit in memory.

`GET /api/test-artifacts/{id}` returns one artifact's raw `content`
alongside its `id`, `runId`, `kind` and `name` — the browser-side results
parsing and the regression "analyze" view fetch through it.
`GET /api/test-artifacts/{id}/download` streams the same bytes as a file
download (Content-Disposition attachment, named from the source path's
basename). `GET /api/test-artifacts/{id}/raw` streams them for *viewing*
instead of saving: an `.html`/`.htm` artifact comes back as a page — the
run page frames it and its "open in a new tab" link points at it — under
`Content-Security-Policy: sandbox …` without `allow-same-origin`, so the
page's scripts run from an opaque origin that can reach neither this
site's cookies or storage nor its API with credentials. The readable text
formats (`.json`, `.xml`, `.txt`, `.log`, `.csv`, `.md`, `.yaml`) come
back as `text/plain`; anything else — archives, binaries, and SVG, which
is script-capable too — falls back to the plain download. The bytes live
in object storage: a read whose object is gone is a `404`, and one where
the backend itself failed is a `502` — the row is still there and the
request was fine (see [Object storage](#/docs/object-storage)).

Two zips bundle artifacts, and neither is ever an empty archive:

- `GET /api/test-runs/{id}/artifacts/zip` — that **attempt's own** files,
  flat, as `run-<id>-artifacts.zip`.
- `GET /api/tasks/{id}/artifacts/zip` — the **whole subtree's** latest
  attempts, the named task's files at the archive root and every
  descendant's under a directory named after it, as
  `task-<id>-artifacts.zip`. A regression stage therefore downloads as
  one bundle of its cases, each under a directory built from its node key
  with `:` replaced by `-` (`regression-heat/file.json`), so the layout
  survives a re-dispatch. Two files that would land on the same path get
  a ` (2)` suffix rather than overwriting each other, and retired nodes
  are left out: their files belong to an earlier graph shape.
- A request whose bundle has no files at all is a `404`
  (`{"error":"no artifacts"}`) — an empty zip would look like a
  successful download.

## Script execution (interactive)

Besides the automatic task graphs, environments accept ad-hoc commands
and scripts from the **Run command** page:

- `POST /api/environments/{id}/exec` runs a raw shell command.
- `POST /api/environments/{id}/script` accepts a bash or Python script
  (`"language": "bash"` or `"python"`) and streams it to the remote
  interpreter over stdin.

The interpreter is taken from the script's first-line comment, which
overrides the declared language:

- `#!/usr/bin/env bash`, `#!/bin/bash`, or `# bash` → `bash -`
- `#!/usr/bin/env python3`, or `# python3` → `python3 -`

Only bash, sh, python and python3 are accepted. Commands time out after
60s, scripts after 10 minutes. Disabled environments reject both.

Both endpoints log in with the environment's stored private key, so — like
`POST /api/environments/{id}/test` and every write to an environment — they
are limited to the environment's owner and the administrators (`403`
otherwise). Reading the list is not limited: see
[Test environments](#/docs/environments) for the ownership model.

## Manual test dispatch

The **Run command** page also dispatches user-configured tests as
scheduled task graphs (the *Manual test* tab) — the same mechanism the
webhooks use, but with the stage commands taken from the form instead of
`md-builder.yaml` (details in [Runner and tasks](#/docs/runner-strategy)):

```
POST /api/jobs/manual
{
  "repo": "https://gitlab.example.com/group/code",   // optional: site default
  "ref": "master",                                    // optional: HEAD
  "buildCommand": "cmake . && cmake --build . -j8",   // optional; empty = no build stage
  "unitCommand": "ctest -L unit",                     // optional: stage skipped
  "unitArtifacts": "build/test_detail.xml",           // optional: artifact file(s)
  "regressionCommand": "python3 run.py",              // optional: stage skipped
  "regressionArtifacts": "reg/results.json",          // optional: artifact file(s)
  "environmentIds": [1, 2]
}
```

The `unitArtifacts` / `regressionArtifacts` fields accept a single path
or a list of paths — a run can produce several artifact files.

- At least one stage command and one environment are required.
- The ref is resolved to a concrete commit (a remote ref listing, the
  equivalent of `git ls-remote`) with the site's Project Access Token;
  the response is
  `{"roots": [{"taskId": 42, "environmentId": 1}, …]}` — one root task
  per environment, ordered like the request.
- The environments are dispatched one after another, so a failure part
  way down the list (a disabled environment, a graph that cannot be
  built) is a `422` whose body still carries the `roots` created before
  it: those tasks are queued and running, and the Run page lists them
  next to the error instead of reporting that nothing happened.
- Graphs are marked `trigger: 1` (manual); every dispatch records a
  fresh commit row, so re-running the same ref adds a new matrix row and
  supersedes the older ones.
- `environmentIds` names the environments explicitly — there is no tag
  matching here — and every id must be one the caller **may manage**:
  the environment's owner, or an administrator. A stage command runs over
  SSH with the environment owner's private key, so letting anybody target
  anybody's machine would be a way around the rule
  `/api/environments/{id}/exec` and `/script` already apply. A foreign
  id is `403` (naming the environment), an unknown one `422`. The Run
  page's picker offers exactly this set, so the check is the endpoint
  catching up with the UI rather than a new constraint on it.
- The **webhook** path is deliberately not narrowed: a push matches yaml
  entries against every enabled environment on the site, whoever
  registered it (see [Runner and tasks](#/docs/runner-strategy)).

### YAML matrix dispatch

`POST /api/jobs/manual-yaml` runs the **webhook flow on demand** for a
ref of the site-configured code repository — the form has one input and
one button on the *Manual test* tab:

```
POST /api/jobs/manual-yaml
{
  "ref": "main"   // branch, tag, short/full SHA; empty = HEAD
}
```

The ref is resolved (the same `git ls-remote` as the manual dispatch),
the commit recorded **deduplicated like a webhook push**, the
md-builder.yaml at that commit read and parsed, and one graph per
matching environment created:

```
{
  "commitId": 7, "commitSha": "abc123…", "commitCreated": true,
  "jobsCreated": 2, "entriesSkipped": 0
}
```

- Graphs are marked `trigger: 2` (manual yaml). Re-triggering the same
  ref requeues the **same** graphs (keyed by commit+environment) with
  fresh snapshots — yaml or environment-tag changes are picked up, and
  no extra matrix row appears.
- There is no environment list in the request: each yaml entry is matched
  by tags against **every enabled environment on the site**, whoever
  registered it — the same matching a webhook push does, so a run may
  land on another account's machine.
- Errors (unresolvable ref, bad YAML, no matching environment) come back
  as 422 with a `dispatchError` field, mirroring the webhook response;
  the commit row stays recorded when one was resolved.

## First-run setup

A database with no account at all has nothing to log in with, so the API
answers the first-run guide page instead (see
[Getting started](#/docs/getting-started)). Both endpoints are
**unauthenticated** — there is no session to use yet, and no administrator to
create one — and both stop doing anything as soon as an account exists:

```
GET /api/setup
{"required": true}
```

```
POST /api/setup
{
  "codeRepo": "https://gitlab.example.com/group/code",
  "accessToken": "glpat-…",       // optional: empty = public repository
  "username": "root",             // the site's first administrator
  "email": "root@example.com",
  "password": "…"                 // at least 8 characters
}
```

- The account and the repository are stored **in one transaction**: a
  rejected field (`400`, using the same rules as `adduser` and the account
  API) writes nothing, and so does a request that arrives after the site is
  set up (`409`).
- The created account is an **administrator**, and it is the only account
  this endpoint can create: the role is fixed here, which is the one place a
  role is not decided by the CLI.
- The response is the same shape `/api/login` returns, and the session
  cookie is set the same way — the guide page signs the new administrator
  straight in.
- `accessToken` is write-only like everywhere else: it is stored, never
  echoed, and never logged.

## API endpoints

All endpoints require a session (cookie) unless noted. Apart from the first
administrator, created by the setup page above, users are created with the
`adduser` CLI (see [Getting started](#/docs/getting-started)); further
administrators are created there too, with `adduser -admin`.

| Method | Path                            | Description                                   |
|--------|---------------------------------|-----------------------------------------------|
| GET    | `/api/health`                   | Health check (no session)                     |
| GET    | `/api/setup`                    | Whether the site still needs its first-run setup (no session) |
| POST   | `/api/setup`                    | Create the first administrator and store the code repository, then sign it in (no session; `409` once set up) |
| POST   | `/api/login`                    | Authenticate, sets session cookie             |
| POST   | `/api/logout`                   | Destroy the current session                   |
| GET    | `/api/me`                       | Current user (`id`, `username`, `email`, `role`) |
| GET    | `/api/auth/gitlab/enabled`      | Whether the site offers GitLab sign-in (no session) |
| GET    | `/api/auth/gitlab/start`        | Begin a GitLab sign-in (redirects to GitLab)  |
| GET    | `/api/auth/gitlab/callback`     | Finish it (GitLab redirects back here)        |
| GET    | `/api/users`                    | List every account (administrator only)       |
| PUT    | `/api/users/{id}`               | Edit an account: your own, or anyone's as an administrator |
| GET    | `/api/environments`             | List every environment on the site, each with `owner` and `canEdit` |
| POST   | `/api/environments`             | Create a test environment (you become its owner) |
| GET    | `/api/environments/{id}`        | Get one environment (any signed-in user)      |
| PUT    | `/api/environments/{id}`        | Update one environment (owner or administrator) |
| DELETE | `/api/environments/{id}`        | Delete one environment (owner or administrator; also its test runs) |
| POST   | `/api/environments/{id}/test`   | SSH connectivity check                        |
| PUT    | `/api/environments/{id}/enabled`| Enable/disable (`{"enabled": bool}`)          |
| POST   | `/api/environments/{id}/exec`   | Run a shell command (`{"command": string}`)   |
| POST   | `/api/environments/{id}/script` | Run a script (`{"language", "script"}`)       |
| GET    | `/api/site-config`              | Site repository configuration (`codeRepo`, `accessTokenSet`, `timezone`, `webhookToken` — administrators only) |
| PUT    | `/api/site-config`              | Update site configuration (access token: empty = keep, `clearAccessToken` = remove; `timezone`: IANA name, empty = browser-local; the `gitlab*` fields are administrator-only) |
| POST   | `/api/site-config/webhook-token`| Rotate the webhook secret and return the configuration (administrators only) |
| GET    | `/api/dashboard/{kind}`         | Test result matrix, `kind` = `regression` \| `unit` \| `build` |
| GET    | `/api/dashboard/full`           | Full pipeline matrix: per commit and environment the build/unit/regression stages plus the task-graph link |
| POST   | `/api/test-runs`                | Close a task's current attempt: report a result against `taskId` (owner or administrator of the task's environment) |
| GET    | `/api/test-runs/{id}`           | One attempt of one task: counts, the task's identity, every attempt, artifacts |
| GET    | `/api/test-runs/{id}/artifacts/zip` | That attempt's own artifacts as a zip (`404` when there are none) |
| GET    | `/api/test-artifacts/{id}`      | One stored artifact's raw content             |
| GET    | `/api/test-artifacts/{id}/download` | One artifact as a file download          |
| GET    | `/api/test-artifacts/{id}/raw`  | One artifact for viewing: HTML as a sandboxed page, text as text |
| POST   | `/api/jobs`                     | Manually re-dispatch the task graphs for a commit (webhook-style, reads the YAML) |
| POST   | `/api/jobs/manual`              | Dispatch a user-configured test (repo, ref, stage commands, environments; no YAML) |
| POST   | `/api/jobs/manual-yaml`         | Dispatch the md-builder.yaml matrix at a ref (webhook flow on demand) |
| GET    | `/api/jobs`                     | Recent task graphs (`?limit=`, monitoring; legacy job shape) |
| GET    | `/api/tasks/{id}`               | One task; a root carries its node list (current plus retired) and commit/environment context |
| GET    | `/api/tasks/{id}/runs`          | The task's attempts, newest first (one run per attempt) |
| GET    | `/api/tasks/{id}/log?after=<seq>&attempt=<n>` | The task's log chunks after the given sequence (incremental, live-following) |
| GET    | `/api/tasks/{id}/log/download?attempt=<n>` | The attempt's full log as a `text/plain` file attachment (`Content-Disposition`) |
| GET    | `/api/tasks/{id}/artifacts/zip` | The subtree's latest artifacts as one zip, descendants under a directory named after them (`404` when there are none) |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook receiver (no session: authenticated by the `X-Gitlab-Token` header, see [Webhooks](#/docs/webhooks)) |

The environment list is **not** owner-scoped: dispatch matches a yaml entry
against every enabled environment, so the pool is site-wide and every row is
visible to everyone. Each row carries `owner` (the managing account's
username) and `canEdit` (true for the owner and for administrators), which is
what the UI uses to render a foreign row read-only. Mutating a row you do not
own is `403`; an id that does not exist is `404`. See
[Test environments](#/docs/environments).

Environment create/update bodies carry `name`, `host`, `username`,
`privateKey`, `tags`, `description`, `enabled`, `envScript` (the
environment setup script sourced before every stage) and `allowedEnvVars`
(the host environment variable whitelist a md-builder.yaml `variables:`
value may expand **on this machine** — see
[Test environments](#/docs/environments)). Unlike the private key (empty
on update = keep), an omitted/empty `envScript` clears the script.
Both fields round-trip: the private key is never echoed back, the env
script is (it is not a secret).

`allowedEnvVars` is the one field here that is neither a secret nor a
boolean: it is sent as one block of text (names separated by commas, spaces
or newlines) and an empty string is a decision — allow nothing, which is why
it is a plain string rather than a pointer-shaped "absent". Absent = keep
the stored list, so saving the form without touching that box never rewrites
it; on create, where there is nothing to keep, absent means the built-in
default list (`allowedEnvVarsDefault` in the response reports it, and a new
environment is created with it stored, not unset). The list is **per
environment**, not per site: two hosts of the same site may expose different
names. It holds names and no values, so it is reported to everybody, with
the row's own permissions deciding who may change it — its owner or an
administrator. Names are validated on save: one that is not a shell
identifier, or one starting with `MD_`, is refused with a `400` naming the
offender.

The site-configuration tokens differ in what they reveal. `accessToken` and
`secretToken` are write-only: the API reports `accessTokenSet` /
`secretTokenSet` and never the values, and an update keeps the stored token
unless a value or the matching `clear…` flag is sent. `webhookToken` is the
one that comes back in full, because it has to be copied into GitLab by
hand — and only to an administrator: for anybody else the field is empty.
It is generated with the site configuration, so it is never unset; there is
no way to clear it, only to rotate it with
`POST /api/site-config/webhook-token` (administrators only, answers with the
whole configuration). The webhook endpoint compares `X-Gitlab-Token` against
it in constant time and answers `401` on a mismatch, before parsing the body.
See [Site configuration → Webhook secret](#/docs/site-configuration).

The GitLab sign-in configuration lives on the same endpoint but is
administrator-only: `gitlabUrl`, `gitlabClientId`, `gitlabClientSecret` and
`gitlabLoginEnabled`. A request from anybody else that carries any of them is
`403`. `gitlabLoginEnabled` and `gitlabClientSecretSet` are reported to
everyone — the login page needs the first — while the instance address, the
application id and `gitlabRedirectUri` come back for administrators only. The
secret follows the same write-only rule as the two tokens above
(`clearGitLabClientSecret` removes it). `gitlabUrl` and `gitlabClientId` are
optional in a different way: leaving one out keeps the stored value, so a
request that only flips `gitlabLoginEnabled` does not disturb the
credentials. Sending an explicit `""` clears it, which is refused with `400`
while the integration would be left switched on — as is switching it on
without all three pieces. Both `gitlabUrl` and `server.publicURL` must be
full URLs including `http://` or `https://`; a bare host is refused, because
it would build a relative `redirect_uri` that GitLab rejects with a message
that says nothing about the cause. See
[Site configuration → GitLab sign-in](#/docs/site-configuration).

The three `/api/auth/gitlab/*` routes are unauthenticated — they are how a
visitor becomes an account. `start` sets a short-lived `md_gitlab_state`
cookie and redirects to the instance's authorization page; `callback`
compares the returned `state` against that cookie in constant time, clears it,
and answers with a redirect: to the dashboard on success, or to the login page
with a status word (`pending`, `disabled`, `email_taken`, `no_email`,
`denied`, `unavailable`, `error`) when the sign-in was refused. Every outcome
is a redirect carrying a fixed word — never a token, a code, or anything a
remote server said. The redirect URI is built from the configured
`server.publicURL`, never from the request's `Host` header.

The account endpoints are where the two roles differ. `/api/users` requires an
administrator. `/api/users/{id}` accepts your own account, or any account when
you are an administrator; its body carries `username`, `email`, `password`
(empty = keep the stored one), `disabled` and `approved` — the last two are
administrator-only, and both are refused on an administrator's account and on
your own. `role` and `source` are not part of the body, and sending either
changes nothing: only `adduser -admin` creates an administrator, and where an
account came from is a fact about it. An account row also reports `source`
(`local` or `gitlab`), `approved`, and `gitlabId` (0 for a local account). A
new password ends that account's other sessions; disabling ends all of them,
and so does withdrawing an approval — an account loses access when the
decision is made, not when its session happens to expire.
See [Site configuration → User accounts](#/docs/site-configuration).

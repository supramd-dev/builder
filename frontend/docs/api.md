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

Cells with a recorded run show pass/fail counts; clicking one opens the
run detail with the per-case results (name, status, error value, short
note) and the one-paragraph summary reported by the worker. Cells
without a run but with a live task graph show **queued** / **running…**
(or ✗ when the task failed before reporting); a stage that is not part
of the graph at all shows "—" (it was never requested). Every commit row
also carries a **graph** link: the dependency graph of that commit's
task pipeline (clone → build → unit/regression), GitHub-Actions style —
clicking a stage node jumps to its run detail or the live task log (see
[Runner and tasks](#/docs/runner-strategy)).

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
      "stages": {
        "1": [
          {"kind": "build", "runId": 7, "status": "passed"},
          {"kind": "unit", "taskId": 42, "status": "running"}
        ]
      }
    }
  ]
}
```

Each stage either carries the recorded `runId` (opens the run detail) or
the live `taskId` of the task graph while the run has not landed
(`status` one of `pending`/`running`/`failed`/`done`). `taskIds` maps the
environment to the root task id for the graph link.

## Reporting results

Results are reported with `POST /api/test-runs`:

```json
{
  "environmentId": 1,
  "commitId": 7,
  "kind": "regression",
  "summary": "max relative error 3e-7 within tolerance",
  "startedAt": "2026-09-08T03:00:00Z",
  "finishedAt": "2026-09-08T03:04:00Z",
  "cases": [
    {"name": "water-tip4p-npt", "status": "failed",
     "message": "drift above threshold", "durationMillis": 4200}
  ]
}
```

- `commitId` may be replaced by `"commitSha"` + `"commitRepo"`.
- `startedAt` / `finishedAt` are optional (RFC 3339).
- Each regression case becomes a **child test run** of the reported run:
  the case list in the run detail is a list of run summaries, and a case
  row's `id` doubles as the child run id its detail page opens. Case
  statuses are `passed` / `failed` / `skipped` (skipped marks a case whose
  sub-task never ran because an upstream stage failed).
- When `cases` are present the run status and counts are derived from
  them. With no cases, an explicit `"status"` (`passed` | `failed`), a
  `"summary"` and optional aggregate counts (`"total"` / `"passed"` /
  `"failed"` / `"skipped"`) are stored directly — the simplified report
  path (build runs and unit runs, whose per-case detail lives in the
  results-file artifact, not in the database).
- Reporting again for the same (environment, commit, kind) replaces the
  stored result — the API is idempotent, so a flaky reporter can retry
  safely.
- Deleting an environment also deletes its test runs.

`GET /api/test-runs/{id}` returns the run with `taskId` (the stage
sub-task whose log holds the stage's stdout; 0 for external reports),
`rootTaskId` (the graph's root task — the link back to the pipeline
page; 0 for external reports), `name`/`message` (child runs only: the
preset name and its note), `parentRunId`/`parentName` (child runs only —
the parent regression run for the breadcrumb link back up), `cases` (a
child-run summary list: `id` = child run id, plus `name`, `status`,
`message`, `durationMillis`) and `artifacts` — references to stored
files, e.g. the googletest results files the runner fetched back (a run
can produce several):

```json
{
  "id": 12, "kind": "unit", "status": "failed", "taskId": 77,
  "rootTaskId": 70,
  "name": "", "message": "", "parentRunId": 0, "parentName": null,
  "total": 12, "passed": 9, "failed": 2, "skipped": 1,
  "artifacts": [
    {"id": 3, "kind": "results",
     "name": "build/test_detail.xml", "size": 15832},
    {"id": 4, "kind": "results",
     "name": "build/extra.json", "size": 2101}
  ]
}
```

Artifacts belong to the run they were produced by: a unit run's results
files attach to the unit run; a regression case's artifacts attach to the
case's own child run (the parent aggregates counts only).

`GET /api/test-artifacts/{id}` returns one artifact's raw `content` —
the browser-side results parsing and the upcoming regression "analyze"
view fetch through it.

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
- Graphs are marked `trigger: 1` (manual); every dispatch records a
  fresh commit row, so re-running the same ref adds a new matrix row and
  supersedes the older ones.

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
- Errors (unresolvable ref, bad YAML, no matching environment) come back
  as 422 with a `dispatchError` field, mirroring the webhook response;
  the commit row stays recorded when one was resolved.

## API endpoints

All endpoints require a session (cookie) unless noted. Users are
created with the `adduser` CLI (see
[Getting started](#/docs/getting-started)).

| Method | Path                            | Description                                   |
|--------|---------------------------------|-----------------------------------------------|
| GET    | `/api/health`                   | Health check (no session)                     |
| POST   | `/api/login`                    | Authenticate, sets session cookie             |
| POST   | `/api/logout`                   | Destroy the current session                   |
| GET    | `/api/me`                       | Current user                                  |
| GET    | `/api/environments`             | List the user's test environments             |
| POST   | `/api/environments`             | Create a test environment                     |
| GET    | `/api/environments/{id}`        | Get one environment                           |
| PUT    | `/api/environments/{id}`        | Update one environment                        |
| DELETE | `/api/environments/{id}`        | Delete one environment (and its test runs)    |

Environment create/update bodies carry `name`, `host`, `username`,
`privateKey`, `tags`, `description`, `enabled` and `envScript` (the
environment setup script sourced before every stage — see
[Test environments](#/docs/environments)). Unlike the private key (empty
on update = keep), an omitted/empty `envScript` clears the script.
Both fields round-trip: the private key is never echoed back, the env
script is (it is not a secret).
| POST   | `/api/environments/{id}/test`   | SSH connectivity check                        |
| PUT    | `/api/environments/{id}/enabled`| Enable/disable (`{"enabled": bool}`)          |
| POST   | `/api/environments/{id}/exec`   | Run a shell command (`{"command": string}`)   |
| POST   | `/api/environments/{id}/script` | Run a script (`{"language", "script"}`)       |
| GET    | `/api/site-config`              | Site repository configuration (`codeRepo`, `accessTokenSet`, `timezone`) |
| PUT    | `/api/site-config`              | Update site configuration (access token: empty = keep, `clearAccessToken` = remove; `timezone`: IANA name, empty = browser-local) |
| GET    | `/api/dashboard/{kind}`         | Test result matrix, `kind` = `regression` \| `unit` \| `build` |
| GET    | `/api/dashboard/full`           | Full pipeline matrix: per commit and environment the build/unit/regression stages plus the task-graph link |
| POST   | `/api/test-runs`                | Report a test run result                      |
| GET    | `/api/test-runs/{id}`           | One run's detail: cases, counts, artifacts    |
| GET    | `/api/test-artifacts/{id}`      | One stored artifact's raw content             |
| POST   | `/api/jobs`                     | Manually re-dispatch the task graphs for a commit (webhook-style, reads the YAML) |
| POST   | `/api/jobs/manual`              | Dispatch a user-configured test (repo, ref, stage commands, environments; no YAML) |
| POST   | `/api/jobs/manual-yaml`         | Dispatch the md-builder.yaml matrix at a ref (webhook flow on demand) |
| GET    | `/api/jobs`                     | Recent task graphs (`?limit=`, monitoring; legacy job shape) |
| GET    | `/api/tasks/{id}`               | One task; a root carries its sub-task list and commit/environment context |
| GET    | `/api/tasks/{id}/log?after=<seq>` | The task's log chunks after the given sequence (incremental, live-following) |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook receiver (no session; see [Webhooks](#/docs/webhooks)) |

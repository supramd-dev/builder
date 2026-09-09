# Dashboard, reporting and API

## The test dashboard

The dashboard (first tab after login) shows a build.golang.org-style
matrix: one **row per recent git push** (default 10, capped at 50 via
`?commits=`, newest first), one **column per test environment**
(site-wide — all environments configured by any user; disabled ones are
greyed out). Two kinds are available: **regression** and **unit** tests.

Cells with a recorded run show pass/fail counts; clicking one opens the
run detail with the per-case results (name, status, error value, short
note) and the one-paragraph summary reported by the worker. Cells
without a run but with a live job show **queued** / **running…** (or ✗
when the job failed before reporting).

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
    {"name": "water-tip4p-npt", "status": "failed", "errorValue": 0.02,
     "message": "drift above threshold"}
  ]
}
```

- `commitId` may be replaced by `"commitSha"` + `"commitRepo"`.
- `startedAt` / `finishedAt` are optional (RFC 3339).
- When `cases` are present the run status and counts are derived from
  them. With no cases, an explicit `"status"` (`passed` | `failed`) and
  a `"summary"` are stored directly — this is the worker's simplified
  report path.
- Reporting again for the same (environment, commit, kind) replaces the
  stored result — the API is idempotent, so a flaky reporter can retry
  safely.
- Deleting an environment also deletes its test runs.

## Script execution (interactive)

Besides the automatic jobs, environments accept ad-hoc commands and
scripts from the **Run command** page:

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
| POST   | `/api/environments/{id}/test`   | SSH connectivity check                        |
| PUT    | `/api/environments/{id}/enabled`| Enable/disable (`{"enabled": bool}`)          |
| POST   | `/api/environments/{id}/exec`   | Run a shell command (`{"command": string}`)   |
| POST   | `/api/environments/{id}/script` | Run a script (`{"language", "script"}`)       |
| GET    | `/api/site-config`              | Site repository configuration (`codeRepo`, `testInputRepo`, `testRepoRef`, credential set-flags) |
| PUT    | `/api/site-config`              | Update site configuration (deploy key/token: empty = keep, `clearDeploy*` = remove) |
| GET    | `/api/dashboard/{kind}`         | Test result matrix, `kind` = `regression` \| `unit` |
| POST   | `/api/test-runs`                | Report a test run result                      |
| GET    | `/api/test-runs/{id}`           | One run's detail incl. per-case results       |
| POST   | `/api/jobs`                     | Manually re-dispatch test jobs for a commit   |
| GET    | `/api/jobs`                     | Recent test jobs (`?limit=`, monitoring)      |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook receiver (no session; see [Webhooks](#/docs/webhooks)) |

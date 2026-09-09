# md-builder

A platform for running and displaying tests of scientific computing software
(e.g. molecular dynamics), inspired by [build.golang.org](https://build.golang.org).

## Tech stack

- **Backend**: Go, `net/http`, [GORM](https://gorm.io) ORM
- **Database**: SQLite (default, pure-Go driver, no CGO) or PostgreSQL
- **Frontend**: Vite + React 19 + TypeScript
- **Styling**: Tailwind CSS v4 with a custom sourcehut-style minimal theme
  (near-monochrome, thin borders, no shadows, no rounded corners)

## Project structure

```
md-builder/
├── server/                 # Go backend (single binary: server + CLI)
│   ├── main.go            # entrypoint; subcommand dispatch + HTTP server
│   ├── adduser.go         # CLI: create a user
│   ├── terminal.go         # read password from TTY without echo
│   ├── store/             # GORM models + queries (users, sessions,
│   │                      #   environments, site config, commits, test runs)
│   ├── auth/               # bcrypt hashing + session tokens
│   ├── api/                # HTTP handlers (auth, environments, dashboard)
│   ├── sshcheck/           # SSH connectivity test package
│   └── go.mod
└── frontend/              # Vite + React + TS frontend
    ├── src/App.tsx        # shell: login + page routing (incl. dashboard)
    ├── src/LoginPage.tsx  # static login page
    ├── src/DashboardPage.tsx    # test result matrix (regression / unit)
    ├── src/TestRunDetailPage.tsx # per-run case results
    ├── src/CaseDetailPage.tsx   # per-case detail (placeholder)
    ├── src/UserCenter.tsx # environment management dashboard
    ├── src/RunPage.tsx    # remote command/script execution (Monaco editor)
    ├── src/SettingsPage.tsx # site config: repos, branch/commit, GitLab notice
    ├── src/EnvironmentForm.tsx # create/edit environment form
    ├── src/api.ts         # typed API client
    ├── src/index.css      # hand-written sourcehut-style CSS
    └── vite.config.ts     # dev proxy /api -> :8080
scripts/                  # end-to-end API smoke test + demo data seeder
```

## Development

Frontend dev server (hot reload, on :5173, proxies `/api` to :8080):

```sh
make dev-frontend
```

Backend dev server (builds nothing, uses Go source directly, on :8080):

```sh
make dev-backend
```

## Build & run

```sh
make serve   # builds the frontend, then serves via Go on :8080
```

Visit http://localhost:8080

## Authentication

There is **no registration UI**. Users are created via the `adduser` CLI
subcommand; the web UI only handles login.

### Create a user

```sh
# Interactive password (hidden, read from the terminal):
go run ./server adduser -username alice -email alice@example.com

# Or pass the password directly:
go run ./server adduser -username alice -email alice@example.com -password 's3cret!'

# Select a different database:
go run ./server adduser -username alice -email alice@example.com -dsn 'postgres://...'
```

### Database selection

The DSN is taken from the `MD_BUILDER_DSN` environment variable if set, otherwise
it defaults to a local SQLite file `md-builder.db`.

```sh
export MD_BUILDER_DSN='postgres://user:pass@localhost:5432/mdbuilder?sslmode=disable'
```

### API endpoints

| Method | Path                            | Description                                   |
|--------|---------------------------------|-----------------------------------------------|
| GET    | `/api/health`                   | Health check                                  |
| POST   | `/api/login`                    | Authenticate, sets session cookie              |
| POST   | `/api/logout`                   | Destroy the current session                   |
| GET    | `/api/me`                       | Current user (requires session)               |
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
| GET    | `/api/dashboard/{kind}`         | Test result matrix, `kind` = `regression` \| `unit` (requires session) |
| POST   | `/api/test-runs`                | Report a test run result (requires session)   |
| GET    | `/api/test-runs/{id}`           | One run's detail incl. per-case results       |
| POST   | `/api/jobs`                     | Manually re-dispatch test jobs for a commit (requires session) |
| GET    | `/api/jobs`                     | Recent test jobs (`?limit=`, monitoring)      |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook receiver (push events, triggers job dispatch) |

### Site configuration

Logged-in users configure the two repositories the platform tests against:

- **Code repository** — the code under test (not tied to a specific domain)
- **Test input repository** — the inputs used to exercise the code, tested
  at the configured **branch or commit id**

Both repository locations are expected to be **GitLab** repositories
(gitlab.com or a self-hosted instance) — no other platform is supported yet.
The settings page in the UI states this prominently. Locations are not
host-validated at the API level, since self-hosted GitLab instances live on
arbitrary hosts.

#### Private repositories: deploy key / deploy token

For private repositories the settings page also accepts a **GitLab deploy
token** or a **deploy key** (SSH private key). Either credential is used in
two places: by the server itself (to fetch `md-builder.yaml` from the code
repository) and by the generated job scripts on the test environments (to
clone both repositories).

- **Deploy token** — a GitLab deploy token or personal/group access token
  with `read_repository` scope, plus the username GitLab shows next to it
  (`gitlab+deploy-token-42`; empty means `oauth2`). It applies to https
  repository URLs: the server passes it via an inline git credential
  helper, the remote scripts via `GIT_CONFIG_*` environment variables —
  the token never lands in a `.git/config` or a process command line.
- **Deploy key** — a PEM-encoded SSH private key whose public counterpart
  is registered as a deploy key with read access to both repositories.
  https repository URLs are converted to their `ssh://git@host/...` form;
  the key is used via `GIT_SSH_COMMAND` (a temp file on the server, a
  file under the job directory on the environment, removed after the job).

Both fields are write-only: the API reports only whether one is set
(`deployKeySet` / `deployTokenSet`). An update with an empty value keeps
the stored secret; the *Remove …* checkboxes clear it. When both are set,
the token is preferred for https URLs and the key for SSH ones. Token
material is redacted from dispatch/execution error messages before they
are stored.

### GitLab webhooks

Point a GitLab project webhook at `POST /api/webhooks/gitlab` with the
*Push events* trigger. Push events are recorded in the `commits` table —
each becomes a column of the test dashboard (deduplicated by
repo + sha). Other event types are acknowledged with `status: ignored`.
The endpoint is unauthenticated (called by the GitLab server); verify the
`X-Gitlab-Token` header once a secret is configured.

When the site config's `codeRepo` is set and a push to that repository
arrives, the server dispatches test jobs automatically: it reads the
`md-builder.yaml` test matrix at the pushed commit (see below), matches
matrix entries to **enabled** environments by tags, and creates one job
per entry. The response carries `jobsCreated` / `entriesSkipped`, plus a
`dispatchError` when the YAML cannot be fetched or parsed (the commit is
still recorded). `POST /api/jobs` re-runs the dispatch for a commit
(`{"commitId": N}` or `{"commitSha", "commitRepo"}`) — useful after
changing environment tags or the YAML. `GET /api/jobs?limit=20` lists
recent jobs for monitoring.

### Test dashboard

The dashboard (first tab after login) shows a build.golang.org-style
matrix: one **row per recent git push** (default 10, capped at 50 via
`?commits=`, newest first), one **column per test environment**
(site-wide — all environments configured by any user; disabled ones are
greyed out). Two kinds are available: **regression** and **unit** tests.
Cells with a recorded run show pass/fail counts; clicking one opens the
run detail with the per-case results (name, status, error value, short
note) and the one-paragraph summary reported by the worker. Clicking a
case opens a placeholder detail page — result visualization is future
work. Cells without a run but with a live job show **queued** /
**running…** (or ✗ when the job failed before reporting).

Results are reported with `POST /api/test-runs`:

```json
{
  "environmentId": 1,
  "commitId": 7,              // or "commitSha" + "commitRepo" instead
  "kind": "regression",       // or "unit"
  "summary": "max relative error 3e-7 within tolerance",  // optional
  "startedAt": "2026-09-08T03:00:00Z",   // optional
  "finishedAt": "2026-09-08T03:04:00Z",  // optional
  "cases": [
    {"name": "water-tip4p-npt", "status": "failed", "errorValue": 0.02,
     "message": "drift above threshold"}
  ]
}
```

When `cases` are present the run status and counts are derived from them.
With no cases, an explicit `"status"` ("passed" \| "failed") and
`"summary"` are stored directly — the worker's simplified report path.
Reporting again for the same (environment, commit, kind) replaces the
stored result — the API is idempotent, so a flaky reporter can retry
safely.

Deleting an environment also deletes its test runs (the dashboard is a
site-wide view, so dangling rows would otherwise survive the environment).

## Test matrix configuration (md-builder.yaml)

The test matrix is defined in **`md-builder.yaml` at the root of the code
repository** — it changes with the code, and a push that changes it
changes the dispatch. Every environment carries **tags** (set at creation
in the user center, editable later); matrix entries select environments
by these tags:

```yaml
version: 1

# Optional defaults, merged into every matrix entry (maps merge key-wise,
# scalars are overridden per entry).
defaults:
  timeout: 3600                 # per-command timeout seconds (hard cap 4h)
  env:
    OMP_NUM_THREADS: "4"
  build:
    generator: cmake            # cmake (default) | script
    cmake_flags: "-DCMAKE_BUILD_TYPE=Release"
    threads: 8                  # cmake --build -j

# Required: the matrix. Each entry names the tags an environment must
# carry; dispatch picks one matching enabled environment per entry.
matrix:
  - tags: [cpu]
    description: Generic CPU build
    env:
      CC: gcc
      CXX: g++
    build:
      cmake_flags: "-DENABLE_MPI=OFF"
    unit:
      command: "ctest --test-dir build -L unit --output-on-failure"
      timeout: 600
    regression:
      command: "python3 run_regression.py --suite full"
      timeout: 1800

  - tags: [gpu, cuda]
    env:
      CC: clang
    build:
      generator: script         # non-cmake projects
      command: "./build.sh --cuda"
    unit:
      command: "ctest --test-dir build -L unit"
```

Rules:

- `version` must be 1. `matrix` must be non-empty; each entry needs
  non-empty `tags` and at least one of `unit` / `regression` (each with a
  `command`). Duplicate tag sets across entries are rejected.
- Tag matching is subset semantics: an environment matches when its tag
  set contains all entry tags (extra environment tags are fine). Tags are
  compared case-insensitively (normalized to lowercase).
- When several enabled environments match, the tightest match wins
  (fewest unrelated tags), ties broken by environment name. Exactly one
  environment runs each entry. No match → the entry is skipped (counted
  in `entriesSkipped`), not an error.

## Worker

The server embeds a job scheduler (`server/worker`) started from main:

- **Dispatch** (webhook or `POST /api/jobs`): fetch the code repository,
  read `md-builder.yaml` at the pushed commit via `git show`, parse it,
  match entries to enabled environments, create pending jobs (one per
  entry, requeueing an existing (commit, environment) job on re-push).
  Each job stores a config snapshot, so later YAML changes do not affect
  already-dispatched jobs. When a deploy key or deploy token is
  configured, the fetch uses it (see
  [Private repositories](#private-repositories-deploy-key--deploy-token)).
- **Execution pool**: 2 goroutines by default (`MD_BUILDER_WORKERS`,
  `MD_BUILDER_DISABLE_WORKER=1` disables), polling every 2s, claiming
  jobs atomically. Jobs left in `running` after a crash are reset at
  startup.
- **Per job**: a bash script is generated from the config snapshot and
  streamed to the environment over SSH (`bash -s`). The script clones
  the **test input repository** (test cases) and the **code repository**
  at the pushed commit into `~/.md-builder/jobs/<sha>` on the remote
  host, exports `MD_COMMIT`, `MD_ENV_NAME`, `MD_ENV_TAGS`, `MD_CODE_DIR`,
  `MD_TEST_INPUT_DIR` plus the YAML `env`, builds (cmake or the script
  generator) and runs the unit/regression commands, each bounded by the
  configured timeout via the remote `timeout` command.
- **Report**: the script prints a line protocol
  (`===MD-BUILDER-REPORT-BEGIN===` … `unit-status passed`,
  `unit-summary …`, `regression-status …` … `===MD-BUILDER-REPORT-END===`)
  which the worker parses and stores as test runs — status plus a
  one-paragraph summary (the simplified report; per-case results are
  future work). Test commands may emit `MD-BUILDER-SUMMARY: <text>` on
  stdout to provide the summary text; otherwise the exit code plus the
  log tail is used.
- The overall SSH session timeout is the sum of the stage timeouts plus
  15 minutes slack.

Prerequisites: the server needs `git` on PATH and read access to the code
repository (to read the YAML); each remote environment needs `git`, `bash`
and `timeout`, plus access to both repositories — public repos work
as-is, private ones use the deploy key / deploy token configured in the
site settings (see above).

### Demo data

To see the dashboard populated without a real GitLab or test runner:

```sh
make seed-demo    # pushes 3 fake commits, reports regression + unit runs
```

It needs at least one environment (create one in the user center) and the
smoke user (`make adduser USER=smoke-user EMAIL=smoke@example.com`).

Sessions are stored in the database as random 64-char hex tokens and expire
after 7 days. Passwords are hashed with bcrypt (cost 12).

### API smoke test

[scripts/api-smoke.sh](scripts/api-smoke.sh) exercises the full API surface
against a running server: auth gates, login/logout, environment CRUD
(incl. tags), connectivity test, command exec, script execution
(bash/python), enable/disable gating, webhook push recording (incl. job
dispatch error surfacing), job trigger/list endpoints, test-run
reporting, the dashboard matrix and run details, and deletion (incl. run
cleanup). It is idempotent — safe to run repeatedly.

```sh
make smoke-test          # against the default server on :8080
# or directly, with options:
BASE_URL=http://localhost:9000 USERNAME=alice PASSWORD=secret scripts/api-smoke.sh
SKIP_SETUP=1 scripts/api-smoke.sh   # user already exists, skip adduser
```

It exits non-zero if any check fails, so it can double as a CI gate.

### Script execution

`/exec` runs a raw shell command. `/script` accepts a bash or Python script
(`"language": "bash"` or `"python"`) and streams it to the remote interpreter
over stdin. The interpreter is taken from the script's first-line comment:

- `#!/usr/bin/env bash`, `#!/bin/bash`, or `# bash` → `bash -`
- `#!/usr/bin/env python3`, or `# python3` → `python3 -`

The comment overrides the declared language; only bash, sh, python and
python3 are accepted. Commands time out after 60s, scripts after 10 minutes.

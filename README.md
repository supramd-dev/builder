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
| GET    | `/api/site-config`              | Site repository configuration (`codeRepo`, `testInputRepo`, `testRepoRef`) |
| PUT    | `/api/site-config`              | Update site configuration                     |
| GET    | `/api/dashboard/{kind}`         | Test result matrix, `kind` = `regression` \| `unit` (requires session) |
| POST   | `/api/test-runs`                | Report a test run result (requires session)   |
| GET    | `/api/test-runs/{id}`           | One run's detail incl. per-case results       |
| POST   | `/api/webhooks/gitlab`          | GitLab webhook receiver (push events)         |

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

### GitLab webhooks

Point a GitLab project webhook at `POST /api/webhooks/gitlab` with the
*Push events* trigger. Push events are recorded in the `commits` table —
each becomes a column of the test dashboard (deduplicated by
repo + sha). Other event types are acknowledged with `status: ignored`.
Automatic test runs from webhooks are future work. The endpoint is
unauthenticated (called by the GitLab server); verify the
`X-Gitlab-Token` header once a secret is configured.

When the site config's `codeRepo` is set, only pushes to that repository
are shown as dashboard columns (the repo path is extracted from the URL
and compared to the webhook's `path_with_namespace`).

### Test dashboard

The dashboard (first tab after login) shows a build.golang.org-style
matrix: one **row per recent git push** (default 10, capped at 50 via
`?commits=`, newest first), one **column per test environment**
(site-wide — all environments configured by any user; disabled ones are
greyed out). Two kinds are available: **regression** and **unit** tests.
Cells with a recorded run show pass/fail counts; clicking one opens the
run detail with the per-case results (name, status, error value, short
note). Clicking a case opens a placeholder detail page — result
visualization is future work.

Results are reported with `POST /api/test-runs`:

```json
{
  "environmentId": 1,
  "commitId": 7,              // or "commitSha" + "commitRepo" instead
  "kind": "regression",       // or "unit"
  "startedAt": "2026-09-08T03:00:00Z",   // optional
  "finishedAt": "2026-09-08T03:04:00Z",  // optional
  "cases": [
    {"name": "water-tip4p-npt", "status": "failed", "errorValue": 0.02,
     "message": "drift above threshold"}
  ]
}
```

The run status and counts are derived from the cases. Reporting again for
the same (environment, commit, kind) replaces the stored result — the API
is idempotent, so a flaky reporter can retry safely. Reporting currently
requires a logged-in session; a machine token for automated runners is
future work.

Deleting an environment also deletes its test runs (the dashboard is a
site-wide view, so dangling rows would otherwise survive the environment).

#### Demo data

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
against a running server: auth gates, login/logout, environment CRUD,
connectivity test, command exec, script execution (bash/python), enable/disable
gating, webhook push recording, test-run reporting, the dashboard matrix
and run details, and deletion (incl. run cleanup). It is idempotent — safe
to run repeatedly.

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

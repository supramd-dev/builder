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
├── frontend/              # Vite + React + TS frontend
│   ├── docs/              # user documentation (.md), embedded via ?raw
│   ├── src/App.tsx        # shell: login + page routing (incl. dashboard)
│   ├── src/LoginPage.tsx  # static login page
│   ├── src/DashboardPage.tsx    # test result matrix (regression / unit)
│   ├── src/TestRunDetailPage.tsx # per-run case results
│   ├── src/CaseDetailPage.tsx   # per-case detail (placeholder)
│   ├── src/UserCenter.tsx # environment management dashboard
│   ├── src/RunPage.tsx    # remote command/script execution (Monaco editor)
│   ├── src/SettingsPage.tsx # site config: repos, credentials, webhook notice
│   ├── src/EnvironmentForm.tsx # create/edit environment form
│   ├── src/DocsPage.tsx   # user documentation viewer (markdown.tsx renderer)
│   ├── src/api.ts         # typed API client
│   ├── src/index.css      # hand-written sourcehut-style CSS
│   └── vite.config.ts     # dev proxy /api -> :8080
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
## User documentation

End-user documentation — site configuration, environment tags, the
md-builder.yaml test matrix, GitLab webhook usage, the worker/runner
strategy, the HTTP API and reporting — is built into the web UI: run the
server and open **Documentation** from the footer (or
http://localhost:8080/#/docs). The Markdown sources live in
[frontend/docs/](frontend/docs/) and are embedded into the frontend
bundle at build time.

## Authentication

There is **no registration UI**. Users are created via the `adduser` CLI
subcommand; the web UI only handles login. Passwords are hashed with
bcrypt (cost 12); sessions are random 64-char hex tokens stored in the
database and expire after 7 days.

```sh
# Interactive password (hidden, read from the terminal):
go run ./server adduser -username alice -email alice@example.com

# Or pass the password directly:
go run ./server adduser -username alice -email alice@example.com -password 's3cret!'

# Select a different database:
go run ./server adduser -username alice -email alice@example.com -dsn 'postgres://...'
```

The DSN is taken from the `MD_BUILDER_DSN` environment variable if set,
otherwise it defaults to a local SQLite file `md-builder.db` (both
SQLite — pure-Go driver, no CGO — and PostgreSQL are supported):

```sh
export MD_BUILDER_DSN='postgres://user:pass@localhost:5432/mdbuilder?sslmode=disable'
```

## Demo data

To see the dashboard populated without a real GitLab or test runner:

```sh
make seed-demo    # pushes 3 fake commits, reports regression + unit runs
```

It needs at least one environment (create one in the user center) and the
smoke user (`make adduser USER=smoke-user EMAIL=smoke@example.com`).

## API smoke test

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


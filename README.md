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
│   ├── src/TestRunDetailPage.tsx # per-run detail: case list (child runs), parsed results
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

The server needs object storage for the artifacts it records (see
[md-builder-server.example.yaml](md-builder-server.example.yaml) and
[Object storage](frontend/docs/object-storage.md)); it refuses to start
without it. `-config` names the config file, and everything else — the
listen address, the database, the worker pool — is read from it or from the
matching `MD_BUILDER_*` environment variable, which wins.

### Containers

[Dockerfile](Dockerfile) builds the frontend and the server into one image,
`genshen/md-builder:1.0`; [docker-compose.yml](docker-compose.yml) runs it
next to a MinIO instance. Compose only runs images — it never builds one —
so build (or pull) the image first. The tag is the `MD_BUILDER_TAG`
variable, so building `genshen/md-builder:1.1` and setting
`MD_BUILDER_TAG=1.1` runs that one instead. No configuration file is
needed — the MinIO password is the one value with no default, and it comes
from the environment. Mount one at `/app/md-builder-server.yaml` when the
settings outgrow the compose variables:

```sh
docker build -t genshen/md-builder:1.0 .   # or: podman build ...
export MINIO_ROOT_PASSWORD='pick-something-long'
mkdir -p data/md-builder data/minio   # the database and the buckets
sudo chown 10001:10001 data/md-builder   # the server runs as uid 10001
docker compose up -d                  # or: podman compose up -d
docker compose --profile tools run --rm cli adduser -username alice -email alice@example.com
```

The `chown` is the one step that fails quietly if you skip it: a bind mount
takes its ownership from the host directory, so the container's uid 10001
cannot create the SQLite file and the server exits at startup with
`open store: open db: unable to open database file`. Run the container as
yourself instead (`MD_BUILDER_UID=$(id -u) MD_BUILDER_GID=$(id -g)`) if you
would rather the files stay yours — see
[the deployment docs](frontend/docs/getting-started.md).

## User documentation

End-user documentation — site configuration, environment tags, the
md-builder.yaml test matrix, GitLab webhook usage, the worker/runner
strategy, the HTTP API and reporting — is built into the web UI: run the
server and open **Documentation** from the footer (or
http://localhost:8080/#/docs). The Markdown sources live in
[frontend/docs/](frontend/docs/) and are embedded into the frontend
bundle at build time.

A fully commented example of the md-builder.yaml test matrix (schema
version 2) lives at [md-builder.example.yaml](md-builder.example.yaml).

## Authentication

There is **no registration UI**. Users are created via the `adduser` CLI
subcommand; the web UI only handles login. Passwords are hashed with
bcrypt (cost 12); sessions are random 64-char hex tokens stored in the
database and expire after 7 days.

There are two roles. A regular user signs in and uses md-builder; an
**administrator** additionally manages the accounts from **Settings → Users**
(disable a regular user, edit anyone's username, email and password). An
administrator can only be created here, with `-admin` — no request the web UI
can make sets a role:

```sh
# Interactive password (hidden, read from the terminal):
go run ./server adduser -username alice -email alice@example.com

# An administrator, who can then manage the other accounts in the web UI:
go run ./server adduser -admin -username root -email root@example.com

# Or pass the password directly:
go run ./server adduser -username alice -email alice@example.com -password 's3cret!'

# Select a different database:
go run ./server adduser -username alice -email alice@example.com -dsn 'postgres://...'
```

Usernames and email addresses must be unique and passwords at least 8
characters long — `adduser` and the account API apply the same rules. See
[Site configuration](frontend/docs/site-configuration.md) for what the panel
does.

The DSN is the `database.dsn` key of the server config file, defaulting to a
local SQLite file `md-builder.db`; the `MD_BUILDER_DSN` environment variable
overrides both (SQLite — pure-Go driver, no CGO — and PostgreSQL are
supported):

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


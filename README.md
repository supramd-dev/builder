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
│   ├── store/             # GORM models + queries (users, sessions, environments)
│   ├── auth/               # bcrypt hashing + session tokens
│   ├── api/                # HTTP handlers (auth + environments)
│   ├── sshcheck/           # SSH connectivity test package
│   └── go.mod
└── frontend/              # Vite + React + TS frontend
    ├── src/App.tsx        # shell: login vs user center routing
    ├── src/LoginPage.tsx  # static login page
    ├── src/UserCenter.tsx # environment management dashboard
    ├── src/EnvironmentForm.tsx # create/edit environment form
    ├── src/api.ts         # typed API client
    ├── src/index.css      # Tailwind theme tokens
    └── vite.config.ts     # dev proxy /api -> :8080
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
| DELETE | `/api/environments/{id}`        | Delete one environment                        |
| POST   | `/api/environments/{id}/test`   | SSH connectivity check                        |
| PUT    | `/api/environments/{id}/enabled`| Enable/disable (`{"enabled": bool}`)          |
| POST   | `/api/environments/{id}/exec`   | Run a shell command (`{"command": string}`)   |
| POST   | `/api/environments/{id}/script` | Run a script (`{"language", "script"}`)       |

Sessions are stored in the database as random 64-char hex tokens and expire
after 7 days. Passwords are hashed with bcrypt (cost 12).

### Script execution

`/exec` runs a raw shell command. `/script` accepts a bash or Python script
(`"language": "bash"` or `"python"`) and streams it to the remote interpreter
over stdin. The interpreter is taken from the script's first-line comment:

- `#!/usr/bin/env bash`, `#!/bin/bash`, or `# bash` → `bash -`
- `#!/usr/bin/env python3`, or `# python3` → `python3 -`

The comment overrides the declared language; only bash, sh, python and
python3 are accepted. Commands time out after 60s, scripts after 10 minutes.

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
│   ├── store/             # GORM models + queries (users, sessions)
│   ├── auth/               # bcrypt hashing + session tokens
│   ├── api/                # HTTP handlers (login/logout/me/health)
│   └── go.mod
└── frontend/              # Vite + React + TS frontend
    ├── src/App.tsx        # login page + auth state
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

| Method | Path          | Description                          |
|--------|---------------|--------------------------------------|
| GET    | `/api/health` | Health check                         |
| POST   | `/api/login`  | Authenticate, sets session cookie     |
| POST   | `/api/logout` | Destroy the current session          |
| GET    | `/api/me`     | Current user (requires session)      |

Sessions are stored in the database as random 64-char hex tokens and expire
after 7 days. Passwords are hashed with bcrypt (cost 12).

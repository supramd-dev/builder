# Getting started

## Accounts

There is **no registration UI**. Users are created via the `adduser` CLI
subcommand on the server; the web UI only handles login.

```sh
# Interactive password (hidden, read from the terminal):
go run ./server adduser -username alice -email alice@example.com

# Or pass the password directly:
go run ./server adduser -username alice -email alice@example.com -password 's3cret!'

# Select a different database:
go run ./server adduser -username alice -email alice@example.com -dsn 'postgres://...'
```

Sessions are stored in the database as random 64-char hex tokens and
expire after 7 days. Passwords are hashed with bcrypt (cost 12).

## Demo data

To explore the dashboards without a real git host or SSH nodes, seed the
database with demo data (user `demo` / `demo-pass-123`, three fake
environments, five pushes, regression/unit/build runs and two finished
task graphs with logs):

```sh
make seed-demo          # or: go run ./server seed
make seed-demo FORCE=1  # rebuild the demo task graphs
```

Then (re)start the server and log in as `demo`.

## Database selection

The DSN is taken from the `MD_BUILDER_DSN` environment variable if set,
otherwise it defaults to a local SQLite file `md-builder.db`.

```sh
export MD_BUILDER_DSN='postgres://user:pass@localhost:5432/mdbuilder?sslmode=disable'
```

## First-run checklist

1. Create a user with `adduser`, log in.
2. **Settings**: configure the code repository under test (see
   [Site configuration](#/docs/site-configuration)); add a Project
   Access Token if the repository is private.
3. **Runner Envs**: register at least one test environment and give it
   tags (see [Test environments](#/docs/environments)).
4. **Code repository**: add a md-builder.yaml test matrix (see
   [The test matrix](#/docs/test-matrix)).
5. **GitLab**: add a push-events webhook pointing at the server (see
   [GitLab webhooks](#/docs/webhooks)).
6. Push a commit — the dashboard (see
   [Dashboard and reporting](#/docs/dashboard)) fills in as the task
   graphs run (see [Runner and tasks](#/docs/runner-strategy)).

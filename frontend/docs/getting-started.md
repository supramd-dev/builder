# Getting started

## First start: the setup page

A site whose database holds **no account at all** opens on a guide page
instead of the login form. It has three blocks:

1. **Code repository** — the GitLab repository under test, and an optional
   Project Access Token (`read_repository`). An empty token means a public
   repository.
2. **Administrator account** — username, email and password. The first
   account of a site is always an administrator.
3. **GitLab webhook** — nothing to fill in: the webhook URL to paste into
   GitLab (project → Settings → Webhooks), the triggers to enable, and a note
   that the secret token is in **Settings → Webhook** once you are signed in.

Confirming stores the repository and the account in one transaction and signs
the new administrator in, so the page leads straight to the dashboard. A
rejected field writes nothing.

The page appears **exactly once**: the server offers it only while no account
exists, and closes it the moment one does — created there, by `adduser`, or by
`seed`. It also means the first account is the one thing that cannot be made
any other way: an administrator needs an administrator to create one, so on an
empty site the setup page is the only way in.

The endpoints behind it are unauthenticated (there is no session to use yet),
which is why the window is kept as narrow as possible. Do not leave a
**fresh, empty** database reachable from the internet before it is set up: the
first visitor to the setup page claims the administrator account. Once any
account exists, the endpoints answer `409` and do nothing.

## Accounts

Apart from that first administrator, there is **no registration UI**. Users
are created via the `adduser` CLI subcommand on the server; the web UI only
handles login.

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

`seed` creates the `demo` account, so the setup page no longer appears on that
database — create the administrator it needs with
`adduser -admin -username root -email root@example.com`.

## Database selection

The DSN is the `database.dsn` key of the server config file, and defaults
to a local SQLite file `md-builder.db` when the file does not set it. The
`MD_BUILDER_DSN` environment variable overrides both.

```sh
export MD_BUILDER_DSN='postgres://user:pass@localhost:5432/mdbuilder?sslmode=disable'
```

## Running the server

```sh
go run ./server                       # http://localhost:8080
go run ./server -config /etc/md-builder/server.yaml
```

`-config` names the server config file, and is the only flag the server
takes: where to listen, which database to open and how many workers to run
all come from that file. `-h` lists the flags of the binary and of each
subcommand.

```yaml
server:
  addr: 127.0.0.1   # host or host:port; empty = every interface
  port: 9000        # 0 = take the port from addr, else 8080
```

The file also holds the object storage section the server needs (see
[Object storage (MinIO)](#/docs/object-storage)) and the `worker` pool.
Copy `md-builder-server.example.yaml` to start one.

Every key has an environment override, which wins over the file and is how
containers are configured: `MD_BUILDER_ADDR` and `MD_BUILDER_PORT` for the
listen address, `MD_BUILDER_DSN` for the database, `MD_BUILDER_DIST` for
the frontend build, `MD_BUILDER_WORKERS` and `MD_BUILDER_DISABLE_WORKER`
for the worker pool, and `MD_BUILDER_S3_*` for the object store.

## Deployment with Docker or Podman

The repository ships a `Dockerfile` and a `docker-compose.yml` that run the
server together with a MinIO instance. Compose only runs images — it never
builds one — so build the server image first (or pull it from a registry):

```sh
docker build -t genshen/md-builder:1.0 . # or: podman build ...
export MINIO_ROOT_PASSWORD='pick-something-long'
mkdir -p data/md-builder data/minio      # before the first `up`
sudo chown 10001:10001 data/md-builder   # the server runs as uid 10001
docker compose up -d                     # or: podman compose up -d
```

No configuration file is involved: the compose file carries a default for
everything except the MinIO password, which it reads from the environment
(and refuses to start without). Any of those defaults can be overridden the
same way — `MD_BUILDER_PORT=9000 docker compose up -d` moves both the
listening port and the published one.

Both services run the image `genshen/md-builder:1.0`, where `1.0` is the
`MD_BUILDER_TAG` variable. To run another tag, build it under that name and
set the variable to match:

```sh
docker build -t genshen/md-builder:1.1 .
MD_BUILDER_TAG=1.1 docker compose up -d
```

A deployment with more settings than compose variables is easier to write
as a file: mount it at `/app/md-builder-server.yaml`, which is the default
location the server looks in (the image's working directory is `/app`), and
drop the corresponding `MD_BUILDER_*` variables. The variables win over the
file, so the two can be mixed.

The UI is then on <http://localhost:8080> and MinIO's console on
<http://127.0.0.1:9001>.

Everything the deployment writes stays in two directories next to the
compose file — the database in `data/md-builder`, the buckets in
`data/minio` — so backing it up is copying `data/`. Create them yourself
first: Docker happily creates a missing bind-mount source, but it does so as
root, and a root-owned directory is one the container's user cannot write.

The server runs as uid 10001, and a bind mount takes its ownership from the
host directory, not from the image — the image's `chown` of `/data` only
applies to a named volume. So on Linux the directory has to be writable by
that uid before the first `up`, either by giving it away:

```sh
sudo chown 10001:10001 data/md-builder
```

or by running the container as yourself — `id -u`/`id -g` and set
`MD_BUILDER_UID`/`MD_BUILDER_GID` to those numbers, which also keeps the
files yours. On macOS this is invisible, because file sharing maps ownership.

Skip both and the server cannot create its SQLite file: it exits at startup
with `open store: open db: unable to open database file ... (14)` — 14 is
SQLite's `SQLITE_CANTOPEN`, and the "out of memory" the message carries is
not the real cause — and the restart policy turns that into a loop. MinIO
comes up regardless, because its image runs as root, so the failure looks
like it is the server's alone.

There is no registration UI, so the first account comes from the CLI — the
`cli` service runs against the same database and object store as the
server:

```sh
docker compose --profile tools run --rm cli adduser -username alice -email alice@example.com
```

It prompts for the password (the `-password` flag works too, but the
password then lands in your shell history). `seed` takes the same route
when you want the demo data.

Add `-admin` to create an **administrator** — an account that can manage the
other accounts from **Settings → Users** (see
[Site configuration → User accounts](#/docs/site-configuration)). The CLI is
the only way to create one, so do it on the server:

```sh
docker compose --profile tools run --rm cli adduser -admin -username root -email root@example.com
```

Building without compose works as well — the image is a static binary plus
the built frontend on Alpine, and it can take its whole configuration from
the environment:

```sh
docker build -t genshen/md-builder:1.0 .
mkdir -p data/md-builder
docker run -d --name md-builder -p 8080:8080 \
  --user "$(id -u):$(id -g)" \
  -v "$PWD/data/md-builder:/data" \
  -v "$PWD/md-builder-server.yaml:/app/md-builder-server.yaml:ro" \
  -e MD_BUILDER_S3_ENDPOINT=minio.example.com:9000 \
  -e MD_BUILDER_S3_ACCESS_KEY=... -e MD_BUILDER_S3_SECRET_KEY=... \
  -e MD_BUILDER_S3_BUCKET=md-builder \
  genshen/md-builder:1.0
```

The server needs object storage to start, in a container exactly as on a
host, so it **exits** when the store is unreachable at startup (see
[Object storage (MinIO)](#/docs/object-storage)). The compose file
restarts it, which is how it waits for MinIO to come up; a restart loop
that never settles means the endpoint or the credentials are wrong, and
`docker compose logs md-builder` says which.

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

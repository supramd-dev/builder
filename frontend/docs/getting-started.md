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

## Running the server

```sh
go run ./server                       # http://localhost:8080
go run ./server -port 9000            # a different port
go run ./server -addr 127.0.0.1:9000  # a specific host and port
go run ./server -config /etc/md-builder/server.yaml
```

`-addr` takes a host or a host:port, and `-port` overrides the port inside
it — so `-addr 127.0.0.1 -port 9000` listens on 127.0.0.1:9000. The
server also needs object storage (see
[Object storage (MinIO)](#/docs/object-storage)); `-config` names that
file, and `-h` lists every flag.

Containers are configured through the environment instead:
`MD_BUILDER_ADDR` and `MD_BUILDER_PORT` set the same defaults the flags
have, `MD_BUILDER_DSN` the database, and `MD_BUILDER_S3_*` the object
store. A flag still wins over its variable.

## Deployment with Docker or Podman

The repository ships a `Dockerfile` and a `docker-compose.yml` that run the
server together with a MinIO instance:

```sh
cp .env.example .env    # then set MINIO_ROOT_PASSWORD
docker compose up -d    # or: podman compose up -d
```

The UI is then on <http://localhost:8080> (`MD_BUILDER_PORT` in `.env`
moves it, including the published port). The database is the
`md-builder-data` volume; MinIO's is `minio-data`, and its console is on
<http://127.0.0.1:9001>.

There is no registration UI, so the first account comes from the CLI — the
`cli` service runs against the same database and object store as the
server:

```sh
docker compose --profile tools run --rm cli adduser -username alice -email alice@example.com
```

It prompts for the password (the `-password` flag works too, but the
password then lands in your shell history). `seed` takes the same route
when you want the demo data.

Building without compose works as well — the image is a static binary plus
the built frontend on Alpine, and it takes its whole configuration from the
environment:

```sh
docker build -t md-builder:local .
docker run -d --name md-builder -p 8080:8080 \
  -v md-builder-data:/data \
  -e MD_BUILDER_S3_ENDPOINT=minio.example.com:9000 \
  -e MD_BUILDER_S3_ACCESS_KEY=... -e MD_BUILDER_S3_SECRET_KEY=... \
  -e MD_BUILDER_S3_BUCKET=md-builder \
  md-builder:local
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

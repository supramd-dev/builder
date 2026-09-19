# Object storage (MinIO)

Every test output file the platform produces — the build's `test_detail.xml`,
a unit test's log, a regression run's result series — is stored as an
**object in MinIO**, and the database keeps only a reference to it (the
object key and its size). Nothing large is kept in the database.

Task logs are the exception: they are appended line by line while a stage
runs and are followed live by the browser, so they stay in the database.

## Configuring the server

The server reads its own configuration file, **`md-builder-server.yaml`**,
at startup. It is a deployment file — it lives next to the server binary,
not in the code repository under test:

```yaml
objectStorage:
  endpoint: minio.example.com:9000
  accessKey: md-builder
  secretKey: "…"
  bucket: md-builder
  useSSL: true
  # prefix: md-builder          # optional namespace inside the bucket
  # region: us-east-1           # optional, for AWS-style endpoints
  autoCreateBucket: true        # create the bucket at startup if missing
  gc: true                      # sweep orphaned objects (see below)
  # gcIntervalHours: 6
```

The file is looked up in this order:

1. the `-config` flag:
   `md-builder -config /etc/md-builder/server.yaml`;
2. `$MD_BUILDER_CONFIG`, if set (the file must exist);
3. `./md-builder-server.yaml`;
4. `./server/md-builder-server.yaml` — so the binary also runs from the
   project root.

A pinned path — the flag or the variable — must exist; it is never silently
replaced by another file, so a typo cannot start a differently configured
deployment. `md-builder -h` lists the flag, and `seed` takes it too
(`md-builder seed -config …`).

An unknown key is a startup error, so a typo like `endpont` is reported
instead of silently leaving the endpoint empty.

This file is also where the server's other settings live — `server.addr`
and `server.port`, `database.dsn`, `dist`, `worker` — each with the same
kind of environment override; see
[Getting started](#/docs/getting-started).

### Through the environment

Containers and CI deployments usually inject credentials instead of
mounting a file. Every setting has an environment variable, and environment
values win over the file:

| Variable | Setting |
| --- | --- |
| `MD_BUILDER_CONFIG` | path to the config file (the `-config` flag wins over it) |
| `MD_BUILDER_S3_ENDPOINT` | `endpoint` |
| `MD_BUILDER_S3_ACCESS_KEY` | `accessKey` |
| `MD_BUILDER_S3_SECRET_KEY` | `secretKey` |
| `MD_BUILDER_S3_BUCKET` | `bucket` |
| `MD_BUILDER_S3_REGION` | `region` |
| `MD_BUILDER_S3_PREFIX` | `prefix` |
| `MD_BUILDER_S3_USE_SSL` | `useSSL` (`true`/`false`) |
| `MD_BUILDER_S3_AUTO_CREATE_BUCKET` | `autoCreateBucket` (`true`/`false`) |
| `MD_BUILDER_S3_GC` | `gc` (`true`/`false`) |
| `MD_BUILDER_S3_GC_INTERVAL_HOURS` | `gcIntervalHours` (a positive integer) |

Setting those four required variables (`ENDPOINT`, `ACCESS_KEY`,
`SECRET_KEY`, `BUCKET`) is enough to run without a file at all; the rest
have sensible defaults. A variable that is set but unreadable
(`MD_BUILDER_S3_GC=ture`, `MD_BUILDER_S3_GC_INTERVAL_HOURS=often`) is a
startup error rather than a silently ignored typo.

## Object storage is mandatory

The server **refuses to start** when the object store is not configured or
not reachable: it validates the settings, connects, and checks that the
bucket exists before it opens the database. There is no fallback that
quietly writes artifacts into the database — a deployment that looks
healthy but loses its artifacts is worse than one that fails at startup.

The same rule holds at runtime. If the object store goes down while the
server runs, the artifact endpoints answer **502 Bad Gateway** rather than
serving something stale, and the health board turns red.

## Key layout

Objects are written under a deterministic key:

```
[<prefix>/]runs/<run id>/<kind>/<file name>
```

`<kind>` is the artifact kind (`results`, `log`, `series`, `file`) and
`<file name>` is the name the reporter used, with characters that are
awkward in an object key flattened to `_` — so `build/test_detail.xml`
becomes `build_test_detail.xml`. Names longer than 128 characters are
truncated with a short hash of the full name appended, which keeps two long
names sharing a prefix distinct. A second artifact with the same name in one
run gets a `-2`, `-3`, … suffix.

Because the key is derived from the run and the name, re-reporting a run
**overwrites its artifacts in place** instead of piling up copies.

## Downloading

The frontend never talks to MinIO directly and needs no MinIO credentials.
The server proxies the bytes:

| Endpoint | Returns |
| --- | --- |
| `GET /api/test-artifacts/{id}` | the content as JSON (`{id, runId, kind, name, content}`) |
| `GET /api/test-artifacts/{id}/download` | the raw file, as an attachment |
| `GET /api/test-runs/{id}/artifacts/zip` | every artifact of the run, zipped |

So MinIO only has to be reachable from the server host — it can sit on a
private network with no ingress at all.

## Reclaiming orphaned objects

Deleting a run (or re-reporting it) removes its rows from the database.
The objects behind them are removed by a background **sweep**, not by the
delete itself: deletes run inside transactions, and removing an object
before the transaction commits would lose data if it rolled back.

The sweep lists `runs/` in the bucket and deletes every object that is
older than one hour and referenced by no `test_artifacts` row. The grace
period protects an object that was just uploaded while its row is still
being inserted. It runs every `gcIntervalHours` (6 by default) and can be
switched off with `gc: false`, which is what you want if the bucket is
shared with other tools and cleaned up elsewhere.

## Migrating an existing deployment

Rows written before object storage existed kept their content inline in the
database. At startup the server moves them into the bucket, one batch at a
time, and clears the inline column. The migration is idempotent and its
failure is not fatal — rows it could not move are still readable from the
database, and the next restart retries them.

## Running MinIO locally

For development, a throwaway MinIO in Docker is enough:

```sh
docker run --rm -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=md-builder \
  -e MINIO_ROOT_PASSWORD=md-builder-secret \
  minio/minio server /data --console-address ":9001"
```

with the matching `md-builder-server.yaml`:

```yaml
objectStorage:
  endpoint: 127.0.0.1:9000
  accessKey: md-builder
  secretKey: md-builder-secret
  bucket: md-builder
  useSSL: false
  autoCreateBucket: true
  gc: true
```

`useSSL: false` is for this local, plain-HTTP setup only — use TLS
(`useSSL: true`) for anything reachable from a network.

For a deployment rather than a scratch store, `docker-compose.yml` starts
MinIO and the server together and wires the environment up for you (see
[Deployment with Docker or Podman](#/docs/getting-started)).

## Credentials

`accessKey` and `secretKey` are never logged and never returned by an API:
startup logs the endpoint, bucket and scheme only, and errors name the
missing setting without its value. Keep the config file out of version
control (it is git-ignored; commit
[`md-builder-server.example.yaml`](https://github.com/genshen/md-builder/blob/main/md-builder-server.example.yaml)
instead) and prefer a dedicated MinIO user scoped to this bucket over the
root credentials.

## Health

**Settings → Health** probes the object store alongside the code
repository: the endpoint and bucket in use, and whether a request to it
succeeds. A store that is reachable but missing its bucket counts as a
failure, so a misconfigured deployment shows up there rather than at the
first artifact write. See [Dashboard & API](#/docs/dashboard).

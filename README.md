# multienv

Every local project wants the same infrastructure (a database, object storage, HTTPS on a real hostname). Running it per project means duplicated containers and clashing ports, sharing it becomes a hassle of keeping track of everything.

multienv is a single-binary CLI that reuses one shared set of these containers across all your projects, with no per-project boilerplate.

You bring up your own containers however you like (usually Docker Compose). multienv watches them, and whenever a container carries a `multienv.*` label it plugs that container into shared, centrally-managed **accessories** (HTTPS reverse proxy, PostgreSQL, S3-compatible store) that multienv creates and tears down for you.

There are no per-project config files. Everything multienv does is driven by labels you put on your own containers.

## Contents

- [Install](#install)
- [Core concepts](#core-concepts)
- [Quick start](#quick-start)
- [Accessories](#accessories)
  - [proxy: HTTPS reverse proxy (Caddy)](#proxy-https-reverse-proxy-caddy)
  - [postgres: PostgreSQL 18](#postgres-postgresql-18)
  - [s3: object storage (RustFS)](#s3-object-storage-rustfs)
- [Command reference](#command-reference)
- [Running as a container](#running-as-a-container)
- [Where multienv keeps state](#where-multienv-keeps-state)
- [Environment variables](#environment-variables)
- [Notes](#notes)

## Install

You need Go and a running Docker daemon.

```bash
go build -o ./multienv ./cmd/multienv
```

This produces a `multienv` binary in the repo root. Move it onto your `PATH` if you like.

Or run it as a container (see [Running as a container](#running-as-a-container) below).

## Core concepts

**Services** are containers *you* own. multienv never creates or recreates them (it only observes them and connects them to the shared network). A container becomes a "service" the moment it has at least one `multienv.*` label.

**Accessories** are shared containers *multienv* owns: `proxy`, `postgres`, and `s3`. Your services opt into an accessory by declaring its labels. Accessories are created on demand and shared across every project on your machine.

**Labels** are the only interface. The pattern is always:

```
multienv.<accessory>.<key>=<value>
```

## Quick start

Add labels to a service in your project's `docker-compose.yml`:

```yaml
services:
  api:
    build: .
    labels:
      multienv.proxy.domain: api.localhost   # serve over HTTPS at https://api.localhost
      multienv.proxy.port: "3000"            # your app listens on 3000
      multienv.postgres.dbname: myapp        # create + use the "myapp" database
      multienv.s3.bucket: uploads            # create + use the "uploads" bucket
```

Once you start multienv, it will inspect all docker containers, look for the labels, and create the appropriate accessories/connections.

```bash
multienv daemon          # leave this running; it watches for label changes and applies them
```

If you don't want a process running in the background, you can also run a single update with:

```bash
multienv reconcile       # applies the current set of labels, exits afterwards
```

## Accessories

Each accessory runs as a single shared container named `multienv-<accessory>` (`multienv-proxy`, `multienv-postgres`, `multienv-s3`) on the `multienv` network. It is created the first time a service requests it and shared across every project on your machine.

Every accessory except the proxy (which is always published on 80/443) can be published to a host port, so you can reach it from your machine and not just from other containers (the defaults are listed per accessory below):

```bash
multienv <accessory> publish 5433   # bind 127.0.0.1:5433 (recreates the container; data is kept)
multienv <accessory> publish off    # stop publishing to the host (in-network access still works)
multienv <accessory> publish        # show the current binding
```

> [!IMPORTANT]
> The accessory's data is never deleted (kept in a named volume `multienv-<accessory>-data`). To reclaim the space, remove the container and volume manually. Caution: this will delete the data for all services using that accessory!

### proxy: HTTPS reverse proxy (Caddy)

Fronts your services with locally-trusted HTTPS without any port conflicts.

| Label | Required | Description |
|---|---|---|
| `multienv.proxy.domain` | yes | Hostname(s) to serve. Comma-separate for multiple. |
| `multienv.proxy.port` | no | Container port to proxy to. Defaults to the first exposed port. |

The proxy listens on ports 80 and 443 (fixed). It uses its own internal certificate authority. To make your browser and tools trust it:

```bash
multienv proxy trust-ca   # extracts the CA and prints OS-specific trust instructions
```

> [!NOTE]
> `api.localhost` and other `*.localhost` names resolve to `127.0.0.1` automatically on most systems. For other hostnames, add them to your `/etc/hosts`.

### postgres: PostgreSQL 18

A shared Postgres server with a database per service. Credentials are always `postgres:postgres`.

| Label | Required | Description |
|---|---|---|
| `multienv.postgres.dbname` | yes | Database to create and use. Must match `^[a-z_][a-z0-9_]*$`. |

Connect from your app:

```
# from another container on the multienv network
postgres://postgres:postgres@multienv-postgres:5432/<dbname>

# from your host machine
postgres://postgres:postgres@127.0.0.1:5432/<dbname>
```

Published on `127.0.0.1:5432` by default.

### s3: object storage (RustFS)

A shared S3-compatible store backed by [RustFS](https://rustfs.com), with a bucket per service. Credentials are always `rustfsadmin:rustfsadmin`.

| Label | Required | Description |
|---|---|---|
| `multienv.s3.bucket` | yes | Bucket to create and use. S3 naming rules apply (lowercase, no dots/underscores). |
| `multienv.s3.domain` | no | Serve the S3 API over HTTPS at this hostname (via the proxy). |
| `multienv.s3.public` | no | `true` to allow anonymous read on the bucket. Defaults to `false`. |

Connect from your app:

```
# from another container on the multienv network
http://rustfsadmin:rustfsadmin@multienv-s3:9000

# from your host machine
http://rustfsadmin:rustfsadmin@127.0.0.1:9000
```

Published on `127.0.0.1:9000` by default.

## Command reference

| Command | What it does |
|---|---|
| `multienv daemon [--debounce DUR]` | Reconcile once, then watch Docker events and reconcile on every change. Run this in the background. Ctrl-C to stop. |
| `multienv reconcile [--dry-run]` | Run a single reconcile pass. `--dry-run` prints what *would* happen without touching Docker. |
| `multienv services list` | List every container multienv currently recognizes as a service. |
| `multienv <accessory>` | Show the accessory's help and its label schema. |
| `multienv <accessory> services` | List the services using this accessory (with history, if available). |
| `multienv <accessory> publish [PORT\|off]` | Show or change the host port the accessory binds to. Auto-reconciles. |
| `multienv proxy trust-ca` | Extract the proxy CA and print instructions to trust it. |

The proxy's ports (80/443) are fixed and have no `publish` command.

## Running as a container

multienv can run its daemon inside Docker, controlling the host's daemon over the socket. Build the image, then run it with the Docker socket bind-mounted and a volume for its state:

```bash
docker build -t multienv .
docker run -d --name multienv --restart unless-stopped \
  -e NO_COLOR=1 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v multienv-state:/data \
  multienv
```

## Where multienv keeps state

multienv stores its own small state file (host-port settings and a history of which services used which accessory) at `~/.multienv/state.json`. This file is optional (multienv works fine without it). Override its location with `MULTIENV_STATE_FILE`.

Accessory data (databases, buckets, certificates) lives in named Docker volumes and survives restarts and host-port changes.

## Environment variables

| Variable | Effect |
|---|---|
| `MULTIENV_STATE_FILE` | Override the state-file path. |
| `NO_COLOR` | Disable colored log output. |
| `DOCKER_HOST` (and friends) | Honored for connecting to Docker. |

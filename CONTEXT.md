# multienv — agent context

This file is for AI agents working on multienv. It captures the project's purpose, architecture, and conventions so an agent can pick up work without prior conversation history.

## What multienv is

A single-binary Go CLI that manages a developer's local Docker environment by treating it as two distinct concerns:

- **Services** — containers the developer brings up themselves (typically via Docker Compose). multienv only observes them via Docker labels; it never recreates them.
- **Accessories** — shared, centrally-managed containers (proxy, postgres, etc.) that multienv creates, configures, and tears down. Services *opt in* to accessories by declaring labels on their own containers (e.g. `multienv.proxy.domain=app.example.com`).

The binary's purpose is to converge the local Docker state toward what the union of services' labels currently asks for. A reconcile pass:

1. Ensures the shared `multienv` Docker network exists.
2. Attaches every multienv-labeled service container to that network.
3. For each registered accessory, hands the matching service requests to that accessory, which then owns its own container's lifecycle and config push.

A future daemon mode (event-driven reconciliation) is anticipated but not yet implemented; the reconciler is intentionally written to be one-shot today and reusable in a daemon later.

## Repository layout

```
cmd/multienv/main.go               # binary entrypoint, signal-aware Cobra runner
internal/
  cli/                             # cobra command tree (root, services, reconcile, accessory wiring)
    root.go                        # NewRoot() — builds registry, wires subcommands
    accessory.go                   # framework: per-accessory parent command, reserved-name check
    services_cmd.go                # framework: <accessory> services subcommand
    publish_cmd.go                 # framework: <accessory> publish subcommand
    reconcile.go                   # top-level reconcile command
    services.go                    # top-level `multienv services list`
  docker/                          # the only package that imports the Docker SDK
    client.go                      # Client wrapper, ListContainers
    container.go                   # Container / ContainerPort / ContainerSpec / Mount / PortBinding
    network.go                     # NetworkInspect/Create/Connect, Network type, ErrNotFound
    lifecycle.go                   # ContainerInspect/Create/Start/Remove/Exec/CopyFrom + ImagePull + demuxDockerStream
  labels/labels.go                 # multienv.* prefix, HasMultienv, ForAccessory, Accessories
  service/service.go               # Service domain type + FromContainer(s)
  reconciler/reconciler.go         # Reconciler with three phases + three log channels
  accessory/
    accessory.go                   # Accessory interface, Env, Registry, ConfigOption, Column, HostBindingSpec, DockerAPI
    proxy/                         # Caddy-based HTTPS proxy
    postgres/                      # PostgreSQL 18 accessory
    s3/                            # MinIO-based S3 accessory
  state/state.go                   # ~/.multienv/state.json reader/writer
```

`internal/` only; no `pkg/`. There is intentionally only one binary today, but the layout supports adding more `cmd/<name>/` later.

## Key dependencies

- `github.com/moby/moby/client` v0.4.x — the *split* Docker SDK (new home).
- `github.com/moby/moby/api/types/...` — types live here, not in legacy paths.
- `github.com/containerd/errdefs` — for `IsNotFound` detection on SDK errors.
- `github.com/spf13/cobra` — CLI framework. Cobra is only imported in `internal/cli/`; accessory packages must stay framework-free.

**Do NOT add `github.com/moby/moby` (without subpath) — the legacy `+incompatible` module collides with the split modules and breaks the build.** We intentionally inlined a small stdcopy demuxer (`demuxDockerStream` in `internal/docker/lifecycle.go`) instead of taking that dep.

## Label namespace

- Root prefix: `multienv.`
- Per-accessory namespace: `multienv.<accessory>.<key>=<value>`
- A container is recognized as a multienv service iff it has at least one `multienv.*` label (see `labels.HasMultienv`).
- The accessory's `<key>` segment is what each accessory consumes — proxy reads `domain` and `port`; postgres reads `dbname`; s3 reads `bucket`, `domain`, and `public`.
- **Cross-accessory plumbing via labels.** Accessories never call into each other in Go. When the s3 accessory needs proxy routing for its container, it stamps `multienv.proxy.domain` / `multienv.proxy.port` onto the `multienv-s3` container itself; the proxy then picks it up as just another service. This is the prescribed pattern for any future accessory that needs HTTP fronting.
- `multienv.managed=true` is set on multienv-owned resources (the network, the proxy container, the postgres container).
- Standard compose labels are also read: `com.docker.compose.project`, `com.docker.compose.service`.

## Service identity

Defined by `displayName(service.Service)`:
- With compose labels: `<project>/<service>` (e.g. `myapp/api`)
- Without: container's primary name (e.g. `lonely-thing`)

This is the **stable identity** across `compose down && compose up` because container IDs change but project/service names don't. State-file keys, log lines, and the `services` listing all use it.

## The reconciliation model

Entrypoint: `reconciler.Reconciler.Reconcile(ctx)` in `internal/reconciler/reconciler.go`.

Three phases, in order:

1. **ensureNetwork** — `multienv` bridge network with `attachable=true`, label `multienv.managed=true`. Idempotent: inspect first, create only on `docker.ErrNotFound`.
2. **attachServices** — for every multienv service not yet on the network, `NetworkConnect`. Stopped containers are attached too (a `network connect` on a stopped container makes it live on next start).
3. **reconcileAccessories** — for each registered accessory, collect `ServiceRequest`s (services whose `multienv.<name>.*` labels are non-empty), build a per-accessory `Env`, call `Reconcile`. Per-accessory failures are collected with `errors.Join` so one bad accessory doesn't block others.

After the three phases, the reconciler writes the state file (non-dry-run only; write failures are warning-level, not fatal).

### Dry-run

`r.DryRun == true` short-circuits every operation that would mutate Docker, prints "would …" messages instead. The reconciler does not save state in dry-run.

### Output channels

Three log channels with bracket prefixes (in `internal/reconciler/reconciler.go`):

- `[housekeeping]` cyan — network creation, state-file warnings
- `[service <project>/<name>]` green — per-service attach actions
- `[accessory <name>]` magenta — per-accessory output, plumbed via `Env.Log`

Color is auto-detected by TTY (`os.ModeCharDevice` check) and respects `NO_COLOR` env var. CLI sets `Reconciler.Color`; tests leave it off.

## The accessory framework

Defined in `internal/accessory/accessory.go`.

### Interface

```go
type Accessory interface {
    Name() string
    ConfigSchema() map[string]ConfigOption       // documents labels for help text
    Commands() []Command                          // accessory-specific subcommands (NOT services/publish)
    Reconcile(ctx, env Env, requests []ServiceRequest) error

    HostBinding() *HostBindingSpec               // nil disables the publish subcommand
    ServicesColumns(req ServiceRequest) []Column // table columns in `<accessory> services` output
}
```

`HostBindingSpec{Description, DefaultPort}` — non-nil enables `publish`.

`Command{Name, Short, Long, Run}` — framework-free. The CLI layer (`toCobra` in `internal/cli/accessory.go`) wraps these into cobra commands. Accessories never import cobra.

### Reserved command names

`services` and `publish` are reserved — provided by the framework. If an accessory's `Commands()` returns either name, registration panics at startup. See `reservedAccessoryCommands` in `internal/cli/accessory.go`.

### Env handed to Reconcile

```go
type Env struct {
    Docker        DockerAPI                       // shared interface defined in accessory pkg
    NetworkID     string                          // ID of the multienv network
    Network       string                          // "multienv"
    Log           func(format string, args ...any)// pre-bound; emits [accessory <name>] ...
    DryRun        bool
    HostBoundPort uint16                          // resolved from state (or accessory default); 0 = off
}
```

`Env.Log` is bound to the accessory's name so accessories don't have to prefix their messages.

### Registry

`accessory.NewRegistry()` returns an in-memory registry. `Register` panics on duplicate names. `All()` returns accessories sorted by name. Built in `cli.buildRegistry()`.

### Universal commands per accessory

Added by `internal/cli/accessory.go`:

- `multienv <accessory> services` (always) — see `services_cmd.go`. With state file present: lists every service ever recorded as using this accessory, statuses `running`/`stopped`/`missing`, with last-seen column. Without state file: live Docker only, no STATUS column.
- `multienv <accessory> publish [PORT|off]` (only if `HostBinding() != nil`) — see `publish_cmd.go`. With no args: prints current binding. With a number: writes state and **runs a full reconcile** to apply. With `off`: disables host publication.

The parent command's `Long` help is auto-rendered from `ConfigSchema()` — see `renderAccessoryHelp`.

## State file

Location: `~/.multienv/state.json`. Override via `MULTIENV_STATE_FILE` env var (the `state.PathEnv` constant; used by tests).

Schema (current `state.CurrentVersion` is `1`):

```json
{
  "version": 1,
  "accessories": {
    "postgres": { "host_port": 5432 }
  },
  "services": {
    "myapp/api": {
      "last_seen_at": "2026-05-16T10:00:00Z",
      "accessories": {
        "postgres": { "dbname": "myapp" },
        "proxy":    { "domain": "app.example.com" }
      }
    }
  }
}
```

### Semantics

- **File is optional.** `state.Load()` returns an empty `*State` (no error) when the file is missing. Reconcile and read-only commands MUST keep working without it.
- **`accessories.<name>` absent** → use the accessory's `HostBindingSpec.DefaultPort`. Present with `host_port: 0` → host publication explicitly **off**. Present with non-zero → bind `127.0.0.1:<port>`.
- **`services.<key>` entries are updated by the reconciler** at the end of each (non-dry-run) pass, with the per-accessory configs we observed. The entry's `accessories` map is replaced wholesale — if a service stops using a given accessory, that key disappears from the entry.
- **Write failures**:
  - During reconcile: log one `[housekeeping] could not persist state: ...` line and continue. Reconcile never fails because of state I/O.
  - From `publish`: return an error — the user's intent depended on the write happening.
- **Atomic writes**: `Save` writes to a temp file in the same dir, then `rename`s. No half-written files.
- The package exposes helper methods (do not poke fields directly): `AccessoryHostPort`, `SetAccessoryHostPort`, `RecordService`, `ServicesUsingAccessory`.

## The docker package

`internal/docker/` is the only package allowed to import the Docker SDK. Everything outside it uses our value types:

- `Container` — flat projection of a container summary or inspect (ID, Name(s), Image, State, Status, Labels, Ports)
- `ContainerPort` — `{Private, Public, Protocol}`. `Public` is filled from `NetworkSettings.Ports` (the live host publications), unioned with `Config.ExposedPorts` (image-declared). See `readInspectPorts` in `lifecycle.go`.
- `ContainerSpec`, `PortBinding`, `Mount` — inputs to `ContainerCreate`
- `Network`, `NetworkSpec` — networks
- `ErrNotFound` — sentinel returned by inspect ops when the resource doesn't exist; check with `errors.Is`.

### Exec stream handling

`ContainerExec` takes `stdin io.Reader` and `stdout io.Writer`. Non-TTY exec streams come back multiplexed in Docker's 8-byte-header frame format. `demuxDockerStream` parses those frames in-house — no `pkg/stdcopy` dep. If exit code != 0, stderr is buffered and surfaced in the returned error.

## Existing accessories

### proxy (`internal/accessory/proxy/`)

- Container: `multienv-proxy` running `caddy:2-alpine`, attached to `multienv` network, ports `0.0.0.0:80→80` and `0.0.0.0:443→443`, named volume `multienv-proxy-data` → `/data` (persists CA + autosaved config).
- Cmd: `caddy run --resume` so the previously-applied config is reloaded on container restart.
- Config push: write a Caddyfile inside the container via `docker exec -i sh -c "cat > /tmp/multienv.Caddyfile && caddy reload --adapter caddyfile --config /tmp/multienv.Caddyfile"`. **The admin API is never exposed on the host.**
- Caddyfile contains `{ local_certs }` so all sites use Caddy's internal CA. HTTP→HTTPS redirect stays on (Caddy default).
- Port resolution: `multienv.proxy.port` label, then first container-exposed port (sorted ascending), then per-service skip with an error.
- Upstream target: `<containerName>:<port>` — the container's primary name on the multienv network.
- **Comma-separated `multienv.proxy.domain`** is supported: a single container can serve multiple domains all pointing to the same upstream. `buildRoutes` fans each comma-separated value out to its own `route` entry; downstream Caddyfile generation is unchanged. The s3 accessory relies on this to register every requested s3 domain on one shared `multienv-s3` container.
- `HostBinding() returns nil` — 80/443 are not user-configurable; the `publish` command is intentionally absent from the proxy.
- Per-accessory command: `multienv proxy trust-ca` (extracts CA via `docker cp` to `/tmp/multienv-caddy-ca.crt`, prints OS-appropriate trust instructions, does **not** sudo on its own).

### postgres (`internal/accessory/postgres/`)

- Container: `multienv-postgres` running `postgres:18-alpine`, named volume `multienv-postgres-data` → `/var/lib/postgresql`, default published on `127.0.0.1:5432`. Credentials are blanket `postgres:postgres`.
- `multienv.postgres.dbname=<name>` is the only label. Strict regex `^[a-z_][a-z0-9_]*$` — both to forbid SQL injection in `CREATE DATABASE <name>` and to keep names predictable.
- Idempotent DB creation: exec `psql ... SELECT 1 FROM pg_database WHERE datname = '<name>'`; only `CREATE DATABASE <name>` when absent.
- Readiness: `pg_isready -U postgres -h 127.0.0.1` polled every second up to 30s before issuing CREATE statements.
- **Host-port changes auto-recreate the container** (the named volume preserves data). `ensureContainer` is a three-way switch over `(matches, mismatch, not found)`. The mismatch branch does `ContainerRemove(force=true)` then falls through to create. Triggered end-to-end by `multienv postgres publish 5433`.
- Connection convention (not injected — service authors hardcode):
  - in-network: `postgres://postgres:postgres@multienv-postgres:5432/<db>`
  - from host: `postgres://postgres:postgres@127.0.0.1:<host_port>/<db>`
- `HostBinding() returns {Description: "PostgreSQL", DefaultPort: 5432}` → the `publish` subcommand is enabled.

### s3 (`internal/accessory/s3/`)

- Container: `multienv-s3` running `minio/minio:latest`, cmd `server /data --console-address :9001`, named volume `multienv-s3-data` → `/data`, default published on `127.0.0.1:9000`. Credentials are blanket `minioadmin:minioadmin`. The console port (9001) stays in-network only — `HostBinding` covers the S3 API port (9000) only.
- Labels read on service containers:
  - `multienv.s3.bucket=<name>` (required) — strict regex `^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$` (S3 naming subset, no dots/underscores/uppercase).
  - `multienv.s3.domain=<host>` (optional) — when present, the accessory stamps a corresponding `multienv.proxy.domain` label onto the `multienv-s3` container so the proxy serves that hostname over HTTPS.
  - `multienv.s3.public=<bool>` (optional, default false) — toggles anonymous read on the bucket via `mc anonymous set download|none local/<bucket>`. Parsed with `strconv.ParseBool`.
- **`mc` is exec'd directly inside the MinIO container** (the official image ships with mc since 2022). Container env includes `MC_HOST_local=http://minioadmin:minioadmin@127.0.0.1:9000`, so exec commands don't need a shell wrapper or per-call `mc alias set`.
- Readiness: poll `mc ready local` every 1s up to 30s.
- Idempotent bucket creation: `mc stat local/<name>` (exit code distinguishes exists vs missing); only `mc mb` when missing.
- Anonymous-read policy: applied unconditionally via `mc anonymous set <download|none>` after `ensureBucket` (mc handles idempotency). No read-modify-write.
- **Container drift triggers recreation.** `matchesDesired(c, hostPort, domains)` checks both the host-port binding and the current `multienv.proxy.domain` label; either mismatch forces a `ContainerRemove(force=true)` and a fresh create. Volume preserves data. `logRecreateReason` emits one log line per piece of drift before the recreate. Triggered end-to-end by `multienv s3 publish ...` and by any change to the set of services declaring `multienv.s3.domain`.
- Per-bucket conflict handling: if two services request the same bucket name with different `public` values, the first requester wins and the conflict is logged as a per-service error (does not fail the reconcile).
- `HostBinding() returns {Description: "S3 (MinIO)", DefaultPort: 9000}` → `publish` enabled.
- Connection convention:
  - in-network: `http://minioadmin:minioadmin@multienv-s3:9000`
  - from host: `http://minioadmin:minioadmin@127.0.0.1:<host_port>`

## Development conventions

### Style

- **No comments unless the WHY is non-obvious.** Don't describe what code does; name things well. Comments explain hidden constraints, surprising invariants, intentional non-obvious choices.
- **No premature abstractions.** Add an interface when the second use case lands, not the first.
- **No backwards-compat shims, no "removed-X" stub comments, no unused exports.** If something is unused, delete it.
- **No host config files unless required.** `~/.multienv/state.json` is the only host file and only for multienv's own mutable state (host_port settings, service usage history). Accessories' own runtime data lives in named Docker volumes.
- **Bracket-prefix log channels are the user-facing UX.** Keep them. Adding a fourth channel needs design discussion.

### Testing

- Pure-logic tests for `labels`, `service`, `state`, and per-accessory parsing/lifecycle. The fakes are in-package and small.
- `internal/reconciler/reconciler_test.go` has a `TestMain` that routes `MULTIENV_STATE_FILE` to a temp file so tests never touch the real `~/.multienv/`.
- New tests requiring docker isolate via the same env var.
- No integration tests against a real daemon. Smoke verification is manual: `./multienv reconcile --dry-run`.

### Build & test commands

```bash
go build ./...                      # everything compiles
go test ./...                       # all tests pass
go build -o ./multienv ./cmd/multienv   # produce the binary (gitignored at repo root)
./multienv --help                   # smoke help tree
./multienv reconcile --dry-run      # safe live check
```

The repo's `.gitignore` already excludes `/multienv` and `/bin/`.

### When the user's environment causes a failure

Per the project's `feedback_scope_discipline` memory: when a CLI invocation fails for an obvious environmental reason (missing Docker socket, missing env var, daemon down, missing credential), **state the cause and stop**. Do not probe the user's machine (`docker context ls`, `ls /var/run/...`, etc.) or pre-emptively expand the code with fallbacks unless explicitly asked.

### Reservations / sharp edges

- The proxy container has `multienv.managed=true` and shows up as a `[service multienv-proxy]` line during the attach phase because its labels match the multienv prefix. Harmless but visible. Could be filtered by recognizing `multienv.accessory=*` labels, but no one has asked.
- A failed `compose up` after a label change leaves the old container in `ListContainers(All:true)` because we list stopped containers too. Reconcile happily ignores them; they just appear with `status=stopped` in `services`.
- We never tear down accessory containers when no services request them anymore. By design (dev tool — don't surprise the user). A future `multienv <accessory> prune` is fine.
- We do not parse `docker-compose.yaml`. All knowledge comes from container labels at the daemon.
- Container labels are immutable after create. Any accessory that mutates labels based on per-service requests (s3 does this for proxy routing) must recreate its container to apply changes — design the matching check accordingly and rely on the named volume to preserve data.

## What is explicitly NOT in scope (today)

- Daemon mode / Docker event subscription (reconciler is structured to allow it; just not implemented).
- Injecting env vars into running service containers (impossible without recreate; we chose the by-convention approach: services hardcode connection URLs from documented patterns).
- Auto-trusting CAs / running sudo on the user's behalf in `trust-ca`.
- Per-service postgres users/passwords (everything uses the `postgres` superuser; isolation is zero — dev only).
- Postgres extensions, dump/restore, psql subcommand. Easy to add when needed.
- Configurable proxy host ports (80/443 are fixed).
- State file pruning / TTL on `services` entries.
- Hosts-file management / DNS for proxy domains.

## Operational quick reference

| Command | Behavior |
|---|---|
| `multienv reconcile [--dry-run]` | Full pass: network + attach + accessories + state write |
| `multienv services list` | All current multienv services (top-level — not the accessory subcommand) |
| `multienv <accessory>` | Help with config schema |
| `multienv <accessory> services` | Services using this accessory (history+state if file present, live otherwise) |
| `multienv <accessory> publish [PORT|off]` | Get/set host-port, auto-reconciles |
| `multienv proxy trust-ca` | Extract Caddy CA + print trust instructions |

| File / Env | Purpose |
|---|---|
| `~/.multienv/state.json` | multienv's own mutable state |
| `MULTIENV_STATE_FILE` | Override state-file path (used by tests) |
| `NO_COLOR` | Disable ANSI color in log channels |
| `DOCKER_HOST` etc. | Honored via `client.FromEnv` |

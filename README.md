# hostel

English | [简体中文](README.zh-CN.md)

**hostel is an agent-native sandbox runtime.** It runs many isolated sandboxes
from a single process and exposes an HTTP API to create them, run commands and
shell sessions in them, and read/write their files — built for AI agents that
each need a scratch space to execute in. Each sandbox is called a **bed**. It
runs anywhere: your laptop, a VM, a CI job, or a container.

It **implements the [OpenSandbox](https://github.com/alibaba/opensandbox) execd
HTTP API**, so existing OpenSandbox SDKs work against hostel unchanged.

## Why

If you give each agent (or user, or task) its own full VM or container, it's slow
to start and holds real CPU/RAM even while doing nothing — and agent workloads
sit idle most of the time (the agent spends most of its wall-clock waiting on the
model, not running commands). That's wasteful when you want many of them at once.

hostel takes a lighter approach: pack many isolated **beds** into one process.
A bed is near-instant to create and costs almost nothing while idle, so a single
machine or container can hold a large number of them. Isolation is
filesystem-level (beds share the host kernel) — a good fit for **trusted or
semi-trusted** code; for **untrusted** code you want stronger isolation (a
microVM or a dedicated VM/container).

## Two-layer model

- **A bed is one sandbox**: a workspace directory + a long-running shell (its
  state — cwd, env — persists across commands) + its own mount namespace (under
  bwrap).
- **Default bed**: a request without a bed id lands on `default`, so if you only
  need one sandbox you can ignore beds entirely.
- **Choosing a bed**: send the HTTP header `X-Hostel-Bed` (or `?bed=`); empty
  means the default. Beds are isolated from each other — one bed's shell and
  files are invisible to another.

## Quick start

```bash
make build
./bin/hostel --isolation dorm --workspace-root ./.workspace --addr :8872

curl -s localhost:8872/ping                                   # pong
curl -s localhost:8872/healthz | jq
# foreground command (SSE stream)
curl -sN -XPOST localhost:8872/command \
  -H 'Content-Type: application/json' -d '{"command":"echo hi > /workspace/a.txt; cat /workspace/a.txt"}'
# read the file back
curl -s 'localhost:8872/files/download?path=/workspace/a.txt'
# target another bed (a separate isolation unit; cannot see the default bed's files)
curl -s 'localhost:8872/files/info?path=/workspace/a.txt' -H 'X-Hostel-Bed: conv-1'
```

## API (v1, OpenSandbox-compatible)

| Group | Endpoints |
|---|---|
| Basic | `GET /ping`, `GET /healthz` |
| Files | `GET /files/info`, `DELETE /files`, `POST /files/mv`, `POST /files/permissions`, `GET /files/search`, `POST /files/replace`, `POST /files/upload`, `GET /files/download` |
| Directories | `GET /directories/list`, `POST /directories`, `DELETE /directories` |
| Command | `POST /command` (SSE), `DELETE /command`, `GET /command/status/:id`, `GET /command/:id/logs` |
| Session | `POST /session`, `POST /session/:id/run` (SSE), `DELETE /session/:id` |
| Beds | `GET/POST /v1/beds`, `GET/DELETE /v1/beds/:id`, `POST /v1/beds/:id/checkpoint`, `GET /v1/beds/capabilities` |
| Scheduler | `GET /v1/inventory` — capacity + every local bed (active and luggage) with its persisted generation |

Path semantics: clients address files under the virtual prefix `/workspace`;
hostel rebases that onto the bed's workspace directory. Relative paths are
workspace-relative. Absolute paths outside the prefix are rejected — a bed never
sees the host. Under `bwrap` the workspace is also *really mounted* at
`/workspace` inside the sandbox, so shell paths and file-API paths are the same
string; under `direct` (no mount namespace) the shell cwd is the host dir.
Probe the `workspace_mount` capability to tell the two apart.

## Isolation

Data isolation is graded by **hostel room type**: `--isolation
dorm|room|suite|auto` (default `auto` = the environment ceiling). The effective
level is `min(requested, ceiling)` — an over-ask degrades honestly, a lower ask
is a deliberate downgrade.

- `dorm` (bunk): chdir only, no enforced isolation (= direct, all platforms);
- `room` (private room, shared toilet): Landlock LSM — a bed can't *access*
  other beds' data (EACCES) but siblings stay visible and `/tmp` / system paths
  are shared; **no capability required** (Linux ≥5.13);
- `suite` (fully private): bwrap mount ns — siblings invisible + private `/tmp`
  + canonical `/workspace` mount + env scrub (needs userns or CAP_SYS_ADMIN).

The environment ceiling is probed at boot; healthz/capabilities report
`isolation.{level,mechanism,requested,effective,ceiling}`. See
`docs/data-isolation.md`.

Stronger isolation (real setuid, seccomp, per-bed CPU/memory limits via cgroups,
copy-on-write overlay workspaces, PTY over WebSocket) is on the roadmap.

## Managed services (Chromium / Jupyter / …, planned)

Some tools are heavy to start but can serve many tenants at once — a browser, a
Jupyter server. hostel will run one shared instance and give each bed its own
slice using the tool's native mechanism (a browser context per bed, a kernel per
bed), with outputs saved into that bed's workspace. v1 wires the teardown hook
(a bed's slices are released when the bed is deleted or times out); the actual
Chromium/Jupyter integrations come later.

## Amenities (shared facilities)

Heavyweight, natively multi-tenant tools run **once** per hostel and are sliced
per bed. The first is **Chromium**: one shared browser, an isolated
BrowserContext per bed, artifacts saved into the bed workspace. Enable by
shipping a chromium binary (`--chromium-path`, or it's probed) or attaching to
an existing instance (`--chromium-cdp-url`). Bed-scoped verbs (the raw CDP
socket is never exposed):

```
POST /v1/beds/:id/browser/goto        {url}
POST /v1/beds/:id/browser/screenshot  {path?}   # saved under the bed workspace
POST /v1/beds/:id/browser/text
POST /v1/beds/:id/browser/{click,type,press,scroll,wait}
POST /v1/beds/:id/browser/close
```

The browser starts on first use and stops after an idle grace; capabilities
reports `amenities: {chromium: idle|running}`.

## Configuration

Flags (or `HOSTEL_*` env vars): `--addr` / `--workspace-root` / `--isolation` /
`--default-bed` / `--shell` / `--bed-idle-timeout` / `--max-beds` / `--store` /
`--s3-bucket` / `--s3-prefix` / `--s3-endpoint` / `--persist-interval` /
`--luggage-high-bytes` / `--luggage-low-bytes` /
`--chromium-path` / `--chromium-cdp-url` / `--chromium-idle-stop`.

Persistence: setting `--s3-bucket` (any S3-compatible endpoint) turns it on —
the default `--store auto` resolves to `s3`, an incremental content-addressed
layout: the workspace is CDC-chunked (via desync) and only chunks new since
the bed's previous snapshot are uploaded, so an unchanged workspace
re-persists with zero uploads. Snapshots restore when the bed is created
again and persist on evict (DELETE / idle reap) or explicit checkpoint, plus
an optional `--persist-interval` safety net. A bed's durable identity is the
snapshot; the local dir is just its working copy.
`DELETE /v1/beds/:id` evicts (identity kept); add `?purge=true` to also delete
the snapshot and end the identity. An evict raced by live traffic returns
`409 BED_BUSY` instead of dropping mid-flight writes.

Luggage: an evicted bed leaves its local dir behind as a warm cache — resuming
on the same instance skips the snapshot download (a monotonic generation
counter, carried in bed meta and snapshot metadata, decides freshness; a copy
that fell behind is discarded and re-restored). `--luggage-high-bytes` caps the
disk luggage may hold: past it, cold copies are deleted — stale generation
first, then least recently used — until under `--luggage-low-bytes` (default
80% of high). With the `noop` store luggage is the only copy, so luggage GC
there destroys data — same honesty rule as everywhere: `/healthz` tells you
which world you're in.

Capacity: `--max-beds N` caps concurrent beds (0 = unlimited; the default bed
is neither refused nor counted). A full instance answers new-bed requests with
`429 BED_LIMIT_EXCEEDED` — the backpressure signal for a scheduler to place the
sandbox elsewhere; current and max counts are reported by `/healthz` and the
capabilities endpoint.

## Container image

`build/Dockerfile` is a multi-stage build: a static, pure-Go hostel binary on a
`debian-slim` runtime that bundles the two optional facilities — **bubblewrap**
(the `suite` level) and **chromium** (the browser amenity). Both stay optional:
hostel probes them at boot and degrades honestly, so a locked-down pod without
namespaces still serves.

```bash
make image                     # full image (bwrap + chromium), current arch
make image-lean                # bwrap only (~150MB); browser via --chromium-cdp-url or absent
make image-multiarch IMAGE=repo/hostel:tag   # linux/amd64 + arm64, pushed to a registry
docker run -p 8872:8872 hostel:dev
```

The build is multi-arch (`linux/amd64`, `linux/arm64`): being pure Go, the
builder cross-compiles natively (no QEMU) and only the debian runtime stage runs
per-target so apt pulls the right-arch bwrap/chromium. `make image-multiarch`
needs `docker buildx` and pushes directly (a multi-platform image can't load into
the local docker).

In-container defaults (all overridable via `HOSTEL_*`): `--isolation suite`,
`--workspace-root /workspace` (a declared volume), `--chromium-path
/usr/bin/chromium`. `tini` is PID 1 (reaps shell/chromium children); the
`HEALTHCHECK` calls `hostel --health` (self-GETs `/healthz`, no curl needed).
Whether bwrap actually isolates depends on the pod granting user namespaces /
`CAP_SYS_ADMIN`; without them hostel logs the degrade and runs at `dorm`. The
image runs as root by default (bwrap mount setup + chromium `--no-sandbox`);
harden with a dropped-capability `securityContext` per deployment.

## License & acknowledgements

hostel is licensed under **Apache-2.0** (see [`LICENSE`](LICENSE)), consistent
with its origin. It is **based on / derived from OpenSandbox execd**
(https://github.com/alibaba/opensandbox, Apache-2.0): it began as a
reimplementation of that project's isolated-execution model and is expected to
diverge over time. See [`NOTICE`](NOTICE) for attribution details.

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
./bin/hostel --isolation direct --workspace-root ./.workspace --addr :44772

curl -s localhost:44772/ping                                   # pong
curl -s localhost:44772/healthz | jq
# foreground command (SSE stream)
curl -sN -XPOST localhost:44772/command \
  -H 'Content-Type: application/json' -d '{"command":"echo hi > /workspace/a.txt; cat /workspace/a.txt"}'
# read the file back
curl -s 'localhost:44772/files/download?path=/workspace/a.txt'
# target another bed (a separate isolation unit; cannot see the default bed's files)
curl -s 'localhost:44772/files/info?path=/workspace/a.txt' -H 'X-Hostel-Bed: conv-1'
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

Path semantics: clients address files under the virtual prefix `/workspace`;
hostel rebases that onto the bed's workspace directory. Relative paths are
workspace-relative. Absolute paths outside the prefix are rejected — a bed never
sees the host. Under `bwrap` the workspace is also *really mounted* at
`/workspace` inside the sandbox, so shell paths and file-API paths are the same
string; under `direct` (no mount namespace) the shell cwd is the host dir.
Probe the `workspace_mount` capability to tell the two apart.

## Isolation

- `direct` (default, all platforms): only chdir into the workspace, no isolation
  — for dev / fully-trusted single-tenant use;
- `bwrap` (Linux): mount/pid/uts/ipc namespaces + read-only root; sibling beds
  are masked out of existence (tmpfs over the workspace root), the bed's own
  workspace is mounted rw at `/workspace`, host user data and mounted secrets
  are masked, and secret-looking env vars are stripped. Probed at boot; falls
  back to direct (honestly reported) when unusable.

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
POST /v1/beds/:id/browser/close
```

The browser starts on first use and stops after an idle grace; capabilities
reports `amenities: {chromium: idle|running}`.

## Configuration

Flags (or `HOSTEL_*` env vars): `--addr` / `--workspace-root` / `--isolation` /
`--default-bed` / `--shell` / `--bed-idle-timeout` / `--max-beds` / `--store` /
`--s3-bucket` / `--s3-prefix` / `--s3-endpoint` / `--persist-interval` /
`--chromium-path` / `--chromium-cdp-url` / `--chromium-idle-stop`.

Persistence: with `--store s3` each bed's workspace is snapshotted to
`s3://<bucket>/<prefix>/<bedID>.tar.gz` (any S3-compatible endpoint) — restored
when the bed is created again, persisted on evict (DELETE / idle reap) or
explicit checkpoint, plus an optional `--persist-interval` safety net. A bed's
durable identity is the snapshot; the local dir is just its working copy.
`DELETE /v1/beds/:id` evicts (identity kept); add `?purge=true` to also delete
the snapshot and end the identity. An evict raced by live traffic returns
`409 BED_BUSY` instead of dropping mid-flight writes.

Capacity: `--max-beds N` caps concurrent beds (0 = unlimited; the default bed
is neither refused nor counted). A full instance answers new-bed requests with
`429 BED_LIMIT_EXCEEDED` — the backpressure signal for a scheduler to place the
sandbox elsewhere; current and max counts are reported by `/healthz` and the
capabilities endpoint.

## License & acknowledgements

hostel is licensed under **Apache-2.0** (see [`LICENSE`](LICENSE)), consistent
with its origin. It is **based on / derived from OpenSandbox execd**
(https://github.com/alibaba/opensandbox, Apache-2.0): it began as a
reimplementation of that project's isolated-execution model and is expected to
diverge over time. See [`NOTICE`](NOTICE) for attribution details.

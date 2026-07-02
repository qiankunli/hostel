# hostel

**通用 sandbox 数据面管理程序**：在一台机器 / 一个 K8s pod 内管理多个隔离执行单元（**bed**），对外提供 **OpenSandbox 兼容** 的 HTTP API。可单机跑（laptop / VM / CI），主要跑在 pod 里，作为弱档（共享 pod、仅文件隔离）沙箱的南向 runtime。

设计文档：[`docs/design.md`](docs/design.md)。上层背景见 mono-sandbox `docs/weak-tier.md` 的「hostel」节。

## 两层模型

- **bed = 隔离单元 = 北向一个 sandbox**：一个 workspace 目录 + 常驻 shell（会话状态跨命令保持）+ 自己的 mount namespace（bwrap 下）。
- **默认 bed 兜底**：请求不带 bed 就落到 `default`——单租户调用方可完全无视 bed 概念。
- **bed 路由**：HTTP header `X-Hostel-Bed`（或 `?bed=`），缺省 default。一个 pod 只用一个 bed ≈ 独占；多 bed ≈ 共享，每 bed 仍有私有 ns / workspace / shell。

## 快速开始

```bash
make build
./bin/hostel --isolation direct --workspace-root ./.workspace --addr :44772

curl -s localhost:44772/ping                                   # pong
curl -s localhost:44772/healthz | jq
# 前台命令（SSE 流）
curl -sN -XPOST localhost:44772/command \
  -H 'Content-Type: application/json' -d '{"command":"echo hi > /workspace/a.txt; cat /workspace/a.txt"}'
# 文件读回
curl -s 'localhost:44772/files/download?path=/workspace/a.txt'
# 指定 bed（另一个隔离单元，看不到上面 default 的文件）
curl -s 'localhost:44772/files/info?path=/workspace/a.txt' -H 'X-Hostel-Bed: conv-1'
```

## API（v1，OpenSandbox 兼容）

| 组 | 端点 |
|---|---|
| 基础 | `GET /ping`、`GET /healthz` |
| 文件 | `GET /files/info`、`DELETE /files`、`POST /files/mv`、`POST /files/permissions`、`GET /files/search`、`POST /files/replace`、`POST /files/upload`、`GET /files/download` |
| 目录 | `GET /directories/list`、`POST /directories`、`DELETE /directories` |
| 命令 | `POST /command`(SSE)、`DELETE /command`、`GET /command/status/:id`、`GET /command/:id/logs` |
| 会话 | `POST /session`、`POST /session/:id/run`(SSE)、`DELETE /session/:id` |
| bed 管理 | `GET/POST /v1/beds`、`GET/DELETE /v1/beds/:id`、`GET /v1/beds/capabilities` |

路径语义：客户端用 `/workspace/...` 寻址，hostel rebase 到该 bed 的 workspace 目录；相对路径即 workspace 相对；workspace 之外的绝对路径被拒绝（bed 看不到宿主）。

## 隔离

- `direct`（默认，全平台）：仅 chdir 到 workspace，无隔离——dev / 可信单租户；
- `bwrap`（Linux）：mount/pid/uts/ipc namespace + RO 根 + workspace bind；非 Linux 退化 direct。

更强档（真 setuid / seccomp / per-bed cgroup、overlay CoW + commit/persist、PTY WebSocket）是 v1.1 路线，参考 OpenSandbox OSEP-0013。

## managed-service（Chromium / Jupyter …，v1.1）

重资产、自带多租能力的服务由 hostel 在 bed 外统一管理一份，用应用原生机制切分（Chromium→BrowserContext、Jupyter→kernel），产物落对应 bed 的 workspace。v1 只落 `ReleaseTenant` 钩子（bed 删除/idle 时释放切片），实例后续加入即 drop-in。

## 配置

Flag（或 `HOSTEL_*` 环境变量）：`--addr` / `--workspace-root` / `--isolation` / `--default-bed` / `--shell` / `--bed-idle-timeout`。

## 许可与致谢

hostel 采用 **Apache-2.0**（见 [`LICENSE`](LICENSE)），与其来源保持一致。

hostel **基于 / 派生自 OpenSandbox execd**（https://github.com/alibaba/opensandbox ，Apache-2.0）：起步是对其 OSEP-0013 隔离执行模型的重实现——bubblewrap/overlay 约束、per-session upper 目录管理、marker 分帧常驻 shell、files/command/session 的 API 形状——后续会逐步演化分化。归属细节见 [`NOTICE`](NOTICE)。按 Apache-2.0 §4，任何逐字引入上游源码的文件会保留其 Apache-2.0 头与版权/归属声明。

# hostel

[English](README.md) | 简体中文

**hostel 是一个面向 AI agent 的 sandbox runtime**：用一个进程管理多个互相隔离的 sandbox，对外提供 HTTP API——创建 sandbox、在其中执行命令与 shell 会话、读写它的文件。每个 sandbox 称为一个 **bed**。可跑在任何地方：你的电脑、一台 VM、CI、或一个容器里。

**实现 [OpenSandbox](https://github.com/alibaba/opensandbox) execd 的 HTTP API 规范**，因此现有 OpenSandbox SDK 可直接对接 hostel、无需改动。

## 初衷

给每个 agent（或用户、或任务）配一整个 VM / 容器，启动慢、且空闲时也占着真实的 CPU/内存——而 agent 的负载大部分时间是空闲的（墙钟大头在等模型、不在跑命令）。想同时跑很多个时，这种粒度很浪费。

hostel 走更轻的路：把多个隔离的 **bed** 装进一个进程。bed 创建近乎瞬时、空闲时几乎零成本，于是一台机器 / 一个容器能承载大量 bed，被多个 agent 复用。隔离是文件系统级的（bed 共享宿主内核）——适合**可信 / 半可信**代码；**不可信**代码应使用更强的隔离（microVM 或独立的 VM/容器）。

## 两层模型

- **一个 bed 就是一个 sandbox**：一个 workspace 目录 + 一个常驻 shell（其状态——cwd、env——跨命令保持）+ 自己的 mount namespace（bwrap 下）。
- **默认 bed**：请求不带 bed id 就落到 `default`——只需要一个 sandbox 时可完全无视 bed 概念。
- **选择 bed**：请求带 HTTP header `X-Hostel-Bed`（或 `?bed=`），空即默认。bed 之间互相隔离——一个 bed 的 shell 和文件对另一个不可见。

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
# 指定 bed（另一个隔离单元，看不到 default 的文件）
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

路径语义：客户端用 `/workspace/...` 寻址，hostel rebase 到该 bed 的 workspace 目录；相对路径即 workspace 相对；workspace 之外的绝对路径被拒绝（bed 看不到宿主）。注意：`/workspace` 是 **file API** 的约定；shell 命令的 cwd 是 bed 的真实 workspace 目录，v1 用真实/相对路径。

## 隔离

- `direct`（默认，全平台）：仅 chdir 到 workspace，无隔离——dev / 可信单租户；
- `bwrap`（Linux）：mount/pid/uts/ipc namespace + RO 根 + workspace bind；非 Linux 退化 direct。

更强的隔离（真 setuid、seccomp、每个 bed 的 CPU/内存限制（cgroup）、写时复制 overlay workspace、PTY over WebSocket）在路线图上。

## 共享服务（Chromium / Jupyter …，规划中）

有些工具启动重、但天生支持多租户——浏览器、Jupyter server。hostel 会只跑一份共享实例，用工具自身的机制给每个 bed 一份独立切片（每 bed 一个浏览器 context、一个 kernel），产物存进该 bed 的 workspace。v1 先接好释放钩子（bed 删除或超时时释放它的切片），Chromium/Jupyter 的实际接入后续再加。

## 配置

Flag（或 `HOSTEL_*` 环境变量）：`--addr` / `--workspace-root` / `--isolation` / `--default-bed` / `--shell` / `--bed-idle-timeout`。

## 许可与致谢

hostel 采用 **Apache-2.0**（见 [`LICENSE`](LICENSE)），与其来源保持一致。hostel **基于 / 派生自 OpenSandbox execd**（https://github.com/alibaba/opensandbox ，Apache-2.0）：起步是对其隔离执行模型的重实现，后续会逐步演化分化。归属细节见 [`NOTICE`](NOTICE)。

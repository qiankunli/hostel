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
./bin/hostel --isolation direct --workspace-root ./.workspace --addr :8872

curl -s localhost:8872/ping                                   # pong
curl -s localhost:8872/healthz | jq
# 前台命令（SSE 流）
curl -sN -XPOST localhost:8872/command \
  -H 'Content-Type: application/json' -d '{"command":"echo hi > /workspace/a.txt; cat /workspace/a.txt"}'
# 文件读回
curl -s 'localhost:8872/files/download?path=/workspace/a.txt'
# 指定 bed（另一个隔离单元，看不到 default 的文件）
curl -s 'localhost:8872/files/info?path=/workspace/a.txt' -H 'X-Hostel-Bed: conv-1'
```

## API（v1，OpenSandbox 兼容）

| 组 | 端点 |
|---|---|
| 基础 | `GET /ping`、`GET /healthz` |
| 文件 | `GET /files/info`、`DELETE /files`、`POST /files/mv`、`POST /files/permissions`、`GET /files/search`、`POST /files/replace`、`POST /files/upload`、`GET /files/download` |
| 目录 | `GET /directories/list`、`POST /directories`、`DELETE /directories` |
| 命令 | `POST /command`(SSE)、`DELETE /command`、`GET /command/status/:id`、`GET /command/:id/logs` |
| 会话 | `POST /session`、`POST /session/:id/run`(SSE)、`DELETE /session/:id` |
| bed 管理 | `GET/POST /v1/beds`、`GET/DELETE /v1/beds/:id`、`POST /v1/beds/:id/checkpoint`、`GET /v1/beds/capabilities` |

路径语义：客户端用 `/workspace/...` 寻址，hostel rebase 到该 bed 的 workspace 目录；相对路径即 workspace 相对；workspace 之外的绝对路径被拒绝（bed 看不到宿主）。`bwrap` 下 workspace 还会**真实挂载**在沙箱内 `/workspace`——shell 路径与 file API 路径同名同物；`direct` 下（无 mount namespace）shell cwd 是宿主真实目录。以 capabilities 的 `workspace_mount` 区分两种模式。

## 隔离

- `direct`（默认，全平台）：仅 chdir 到 workspace，无隔离——dev / 可信单租户；
- `bwrap`（Linux）：mount/pid/uts/ipc namespace + RO 根；兄弟 bed 被 tmpfs 遮蔽（视图里不存在），自己的 workspace rw 挂载在 `/workspace`，宿主用户数据与平台挂载凭据被遮蔽，密钥形环境变量被剥除。启动时 probe；不可用则退化 direct 并如实上报。

更强的隔离（真 setuid、seccomp、每个 bed 的 CPU/内存限制（cgroup）、写时复制 overlay workspace、PTY over WebSocket）在路线图上。

## 共享服务（Chromium / Jupyter …，规划中）

有些工具启动重、但天生支持多租户——浏览器、Jupyter server。hostel 会只跑一份共享实例，用工具自身的机制给每个 bed 一份独立切片（每 bed 一个浏览器 context、一个 kernel），产物存进该 bed 的 workspace。v1 先接好释放钩子（bed 删除或超时时释放它的切片），Chromium/Jupyter 的实际接入后续再加。

## amenity(共享设施)

重资产、自带多租能力的工具由 hostel **共享一份**、按 bed 切片。首个是 **Chromium**:一份共享浏览器,每 bed 一个隔离 BrowserContext,产物落 bed workspace。启用方式:镜像带 chromium 二进制(`--chromium-path`,或自动探测)或 attach 既有实例(`--chromium-cdp-url`)。北向只给 bed 级动作(**不透传 CDP socket**):

```
POST /v1/beds/:id/browser/goto        {url}
POST /v1/beds/:id/browser/screenshot  {path?}   # 存进 bed workspace
POST /v1/beds/:id/browser/text
POST /v1/beds/:id/browser/close
```

浏览器首次使用时启动、空闲后自停;capabilities 报 `amenities: {chromium: idle|running}`。

## 配置

Flag（或 `HOSTEL_*` 环境变量）：`--addr` / `--workspace-root` / `--isolation` / `--default-bed` / `--shell` / `--bed-idle-timeout` / `--max-beds` / `--store` / `--s3-bucket` / `--s3-prefix` / `--s3-endpoint` / `--persist-interval` / `--chromium-path` / `--chromium-cdp-url` / `--chromium-idle-stop`。

持久化：`--store s3` 时每个 bed 快照到 `s3://<bucket>/<prefix>/<bedID>.tar.gz`（任意 S3 兼容端点）——同 id 再建时恢复,驱逐（DELETE / idle 回收）或显式 checkpoint 时持久化,另有 `--persist-interval` 周期兜底。bed 的持久身份是快照,本地目录只是工作副本。`DELETE /v1/beds/:id` 是驱逐（身份保留）,`?purge=true` 连快照一起删、终结身份;驱逐撞上并发流量返回 `409 BED_BUSY`,不丢在途写入。

容量：`--max-beds N` 限制并发 bed 数（0 = 不限；default bed 不被拒绝也不计数）。实例满时新建 bed 返回 `429 BED_LIMIT_EXCEEDED`——这是给调度方的背压信号（换个实例放置）；当前/最大数量由 `/healthz` 与 capabilities 上报。

## 容器镜像

`build/Dockerfile` 多阶段构建:静态纯 Go 二进制 + `debian-slim` 运行时,内置两个可选设施——**bubblewrap**(`--isolation bwrap`)与 **chromium**(浏览器 amenity)。两者都是可选的:hostel 启动时 probe、探不到就诚实降级,受限 pod(无 namespace)照常服务。

```bash
make image                     # 完整镜像(bwrap + chromium),当前架构
make image-lean                # 仅 bwrap(~150MB);浏览器走 --chromium-cdp-url 或缺席
make image-multiarch IMAGE=repo/hostel:tag   # linux/amd64 + arm64,推到镜像仓库
docker run -p 8872:8872 hostel:dev
```

镜像多架构(`linux/amd64`、`linux/arm64`):纯 Go,builder 原生交叉编译(不走 QEMU),只有 debian runtime 阶段按目标架构跑、让 apt 拉对应架构的 bwrap/chromium。`make image-multiarch` 需要 `docker buildx` 且直接 push(多平台镜像无法 load 进本地 docker)。

容器内默认值(均可用 `HOSTEL_*` 覆盖):`--isolation bwrap`、`--workspace-root /workspace`(声明为 volume)、`--chromium-path /usr/bin/chromium`。`tini` 作 PID 1(回收 shell/chromium 子进程);`HEALTHCHECK` 用 `hostel --health`(自打 `/healthz`,免 curl)。bwrap 是否真隔离取决于 pod 是否给了 user namespace / `CAP_SYS_ADMIN`,没有则日志记录降级、以 `direct` 运行。

## 许可与致谢

hostel 采用 **Apache-2.0**（见 [`LICENSE`](LICENSE)），与其来源保持一致。hostel **基于 / 派生自 OpenSandbox execd**（https://github.com/alibaba/opensandbox ，Apache-2.0）：起步是对其隔离执行模型的重实现，后续会逐步演化分化。归属细节见 [`NOTICE`](NOTICE)。

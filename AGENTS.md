# hostel

## 项目定位与边界

**通用 sandbox 数据面管理程序**：在一台机器 / 一个 K8s pod 内管理多个隔离执行单元（**bed**），对外提供 **OpenSandbox 兼容** HTTP API。可单机跑（laptop/VM/CI），主要跑在 pod 里，作弱档（共享 pod、仅文件隔离）沙箱的南向 runtime——由控制面（sandctl）经 `sandbox_id → (worker pod, bed)` 路由驱动。

- **做**：bed 生命周期、exec / file、共享多租服务（Chromium/Jupyter…）管理。
- **不做**（留控制面）：调度选 pod、workspace 供给、信任分档路由、计费配额。
- 参考 OpenSandbox execd（Apache-2.0）净重写，非其 fork；归属见 `NOTICE`。设计与 roadmap 见 `docs/design.md`；上层背景见 mono-sandbox `docs/weak-tier.md` 的「hostel」节。

## 代码地图与核心模块

```
cmd/hostel/main.go     组装：config→isolation→service registry→bed manager→gin server；idle GC；优雅关停
internal/
├── config/            flags + HOSTEL_* env
├── isolation/         Isolator 接口（Wrap 一个 exec.Cmd 到 workspace）；direct(全平台) + bwrap(linux build-tag)
├── bed/               ★核心。bed=隔离单元=北向一个 sandbox
│   ├── bed.go         Manager：Resolve(空→default)/Get/List/Delete/CollectIdle；ForegroundShell；StartCommand
│   ├── shell.go       常驻 bash：单 reader goroutine→lines chan，Run 用 marker 分帧、单消费（状态跨 run 保持）
│   └── command.go     一次性命令 registry：前台/后台、status、logs（cursor 增量、环形缓冲）
├── fsops/             bed-workspace-rooted 文件操作；Resolve 做路径 confine + /workspace 虚拟前缀 rebase
├── service/           ManagedService 接口 + Registry（v1 空；bed 删除/idle 调 ReleaseAll）
└── web/               gin 薄适配层：server(路由+bedOf 解析) / errors / sse / files / command / beds
```

**数据流**：请求 →`web` 按 `X-Hostel-Bed`(缺省 default) 解析 bed → 调 `bed`/`fsops` 核心 → 响应（命令走 SSE）。核心层（bed/fsops/isolation/service）**不含任何 HTTP 类型**，换框架只动 `web/`。

## 关键约定

- **bed 是隔离单元 = 北向一个 sandbox**：一 workspace 目录 + 常驻 shell（状态跨命令保持）+ 私有 mount ns（bwrap 下）。**默认 bed 兜底**：不带 bed 的请求落 `default`，单租户调用方可无视 bed 概念；default bed 永不被 idle GC / Delete 清数据。
- **路径两套语义，别混**：`/workspace/x` 是 **file API** 的虚拟前缀（`fsops.Resolve` rebase 到 bed 的真实 workspace 目录，并拒绝逃逸）；**shell 命令**的 cwd 是 bed 的真实 host 目录，用相对/真实路径——`/workspace` v1 不映射给 shell（bwrap 也 bind 在 host path）。把 `/workspace` 做成 shell 内规范挂载是 v1.1。
- **API 对齐 execd**：响应 JSON 结构、错误码、SSE 帧（`<json>\n\n`，事件 shape = execd `ServerStreamEvent`）都对齐 OpenSandbox，SDK 不改。加/改端点先对 `OpenSandbox/specs/execd-api.yaml`。
- **isolation 是可换后端**：`direct` 无隔离（dev/可信）；`bwrap` linux ns。更强档（真 setuid/seccomp/per-bed cgroup、overlay CoW+commit/persist、PTY WS）按 OSEP-0013 增补，全走 `Isolator` 接口，不散在业务层。
- **managed-service 通则**：重资产、自带多租的服务由 hostel 在 bed 外管一份，用应用原生机制切租（Chromium→BrowserContext、Jupyter→kernel），产物落对应 bed 的 workspace。新增实例 = 实现 `ManagedService` + 注册，bed 生命周期已备 `ReleaseTenant` 钩子。
- **常驻 shell 的坑**：一个 Shell 只能有**一个** stdout reader（否则 run 间串输出——v1 踩过）；Run 之间串行；`exit` 会杀死 session，非零退出码用子 shell（`sh -c "exit N"`）。
- Go 项目常规：改完 `go build ./...` + `go test ./...` + `go vet ./...` 三件套过再提交（见 `Makefile`）。这是私人 GitHub 仓（`github.com/qiankunli/hostel`），非 code.byted.org。

## References

- 设计文档（定位、bed 模型、managed-service 框架、决策表、v1 范围与 roadmap）：`docs/design.md`
- 快速上手 / API 一览 / 配置：`README.md`
- 归属（execd 参考的具体设计点）：`NOTICE`
- 弱档整体背景与 hostel 价值点：mono-sandbox `docs/weak-tier.md`「hostel」节
- API 契约来源：`OpenSandbox/specs/execd-api.yaml`（本地 clone 在 mono-sandbox）

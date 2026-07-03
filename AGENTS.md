# hostel

## 项目定位与边界

**面向 AI agent 的 sandbox runtime**：在一台机器 / 一个容器内管理多个隔离执行单元（**bed**），对外提供 **OpenSandbox 兼容** HTTP API。形态上 hostel = **web server + bed manager + amenity manager + store** 的组合（后续可扩充更多 manager）。可单机跑（laptop/VM/CI），也可作为多租户共享实例的 in-process runtime，由上层调度系统按 `sandbox_id → (实例, bed)` 路由驱动。

- **做**：bed 生命周期、exec / file、共享多租服务（Chromium/Jupyter…）管理。
- **不做**（留给上层调度系统）：实例调度、跨实例路由、计费配额。
- 参考 OpenSandbox execd（Apache-2.0）净重写，非其 fork；归属见 `NOTICE`。设计与 roadmap 见 `docs/design.md`。

## 代码地图与核心模块

```
build/Dockerfile       多阶段多架构镜像(amd64/arm64,builder 原生交叉编译免 QEMU)：静态 hostel + debian-slim（内置可选 bwrap + chromium）；tini PID1；hostel --health 做 HEALTHCHECK
cmd/hostel/main.go     组装：config→isolation→amenity registry→store→bed manager→gin server；idle GC/luggage GC/持久兜底；--version/--health/__confine(landlock confiner 自 re-exec) 前置子命令；优雅关停
internal/
├── config/            flags + HOSTEL_* env
├── isolation/         数据隔离房型档：New 按 env ceiling 路由；direct(dorm/全平台) + landlock(room/linux) + bwrap(suite/linux)
├── bed/               ★核心。bed=隔离单元=对外一个 sandbox
│   ├── bed.go         Manager：Resolve(空→default，按 generation 判 luggage 新鲜)/Get/List/Evict/Purge/CollectIdle；ForegroundShell；StartCommand
│   ├── luggage.go     luggage（evict 留下的现场缓存）：磁盘水位 GC（stale 优先→LRU）、Inventory（调度器视图）
│   ├── shell.go       常驻 bash：单 reader goroutine→lines chan，Run 用 marker 分帧、单消费（状态跨 run 保持）
│   └── command.go     一次性命令 registry：前台/后台、status、logs（cursor 增量、环形缓冲）
├── fsops/             bed-workspace-rooted 文件操作；Resolve 做路径 confine + /workspace 虚拟前缀 rebase
├── store/             workspace 持久化：Store 接口 + noop/s3；tar 打包（zip-slip 防护）；见 docs/persistence.md
├── amenity/           Amenity 接口(生命周期 State)+ Registry；chromium 实例(共享浏览器/每 bed BrowserContext)；见 docs/amenity.md
└── web/               gin 薄适配层：server(路由+bedOf 解析) / errors / sse / files / command / beds
```

**数据流**：请求 →`web` 按 `X-Hostel-Bed`(缺省 default) 解析 bed → 调 `bed`/`fsops` 核心 → 响应（命令走 SSE）。核心层（bed/fsops/isolation/service）**不含任何 HTTP 类型**，换框架只动 `web/`。

## 关键约定

- **bed = 客人单元 = 对外一个 sandbox**（workspace + 常驻 shell，状态跨命令保持）；**房型(dorm/room/suite)是这张床的隔离档、与 bed 正交**——bed 是跨档不变的基本单位，房型只描述"床周围的墙"有多严，不替代 bed 命名（见 `docs/data-isolation.md`）。**默认 bed 兜底**：不带 bed 的请求落 `default`，单租户调用方可无视 bed 概念；default bed 永不被清数据、不可 purge、不占 `--max-beds` 名额。**生命周期**：ACTIVE→EVICTING→DORMANT（快照即身份，`store.Stat` 判定，无第二注册表）；EVICTING 期间新活动取消驱逐（防 persist 窗口写丢）；`DELETE`=evict、`?purge=true`=终结身份；bed 目录分层 `{root}/{bedID}/{meta.json,data/}`，快照打包 bed 目录（含可移植 meta，顶层 `*.local` 除外），沙箱只见 `data/`。**luggage**：快照是唯一事实、其余皆缓存——evict 留现场，同机 resume 按 generation（meta 里单调 persist 计数；判序不用时间戳，跨机时钟会反转）判新鲜：够新免下载 warm start，落后则整目录丢弃重拉、只换不合；磁盘走 `--luggage-high/low-bytes` 独立水位 GC（stale 优先→LRU）；`GET /v1/inventory` 给调度器一次拉全容量+本机全部 bed（stale-tolerant hint，正确性靠激活时复查兜底）。详见 `docs/persistence.md` §四。**bed 数量上限**：`--max-beds`（0=不限）只拦新建，满时 429 `BED_LIMIT_EXCEEDED` 作为调度背压；容量经 healthz/capabilities 上报。
- **路径语义按模式**：`/workspace/x` 永远是 **file API** 的虚拟前缀（`fsops.Resolve` rebase + 拒绝逃逸）。**bwrap 下** workspace 同时真实挂载在 bed 内 `/workspace`（shell 路径 == file API 路径，cwd 由 `web` 层 `resolveCwd` 按 `Isolator.MountPoint()` 映射）；**direct 下**无挂载能力，shell cwd 是宿主真实目录。调用方以 capabilities 的 `workspace_mount` 探测。
- **API 对齐 execd**：响应 JSON 结构、错误码、SSE 帧（`<json>\n\n`，事件 shape = execd `ServerStreamEvent`）都对齐 OpenSandbox，SDK 不改。加/改端点先对 `OpenSandbox/specs/execd-api.yaml`。
- **isolation 按「青年旅社房型」分档**（对外保证，非机制名）：`dorm`（通铺，无屏障=direct）/ `room`（单间锁门、厕所公用，数据 EACCES 但兄弟可见、系统路径共享=landlock，自 re-exec `hostel __confine`）/ `suite`（套房全私有，兄弟不可见+私有 mount 视图+`/workspace` 规范挂载+env 剥除=bwrap）/ `auto`（顶格取 env 上限）。`effective=min(requested,ceiling)`，请求超上限诚实降级。机制（direct/bwrap/landlock/uid）是内部细节，全走 `Isolator` 接口。**三档均已实装**（room=landlock 自 re-exec `hostel __confine`，见 `docs/data-isolation.md`）。威胁模型：bed 越狱/串门去动别的 bed。
- **amenity 通则**：重资产、自带多租的共享设施由 hostel 在 bed 外管一份，用应用原生机制切租（Chromium→BrowserContext、Jupyter→kernel），产物落对应 bed 的 workspace。amenity 有自己的生命周期（idle→running 按需启停）。新增实例 = 实现 `Amenity` + 注册，bed evict/purge 已接 `ReleaseAll` 钩子。北向只暴露 bed 级动作，**不透传 CDP/协议 socket**（会跨租户）。见 `docs/amenity.md`。
- **常驻 shell 的坑**：一个 Shell 只能有**一个** stdout reader（否则 run 间串输出——v1 踩过）；Run 之间串行；`exit` 会杀死 session，非零退出码用子 shell（`sh -c "exit N"`）。**锁纪律**：`runMu` 串行化 Run 且只有 Run 碰；`mu` 只护 `dead` 标志、纳秒级持有——曾因单锁设计让「shell 死亡+未断开客户端」死锁整个 daemon（含 healthz），别往 `mu` 里加阻塞代码（见 shell.go LOCKING 注释）。
- Go 项目常规：改完 `go build ./...` + `go test ./...` + `go vet ./...` 三件套过再提交（见 `Makefile`）。仓库在 `github.com/qiankunli/hostel`，保护分支 main 走 PR。

## References

- 设计文档（定位、bed 模型、managed-service 框架、决策表、v1 范围与 roadmap）：`docs/design.md`
- 数据隔离方案（tmpfs 遮蔽兄弟 bed、`/workspace` 规范挂载统一两套路径语义、降级与测试策略）：`docs/data-isolation.md`
- 数据持久化方案（本地 workspace=工作副本、S3 快照=持久身份、边界同步、Store 抽象）：`docs/persistence.md`
- 资源隔离方案（per-bed cgroup v2 子组、Limiter 抽象、委派前提与降级；实现推后）：`docs/resource-isolation.md`
- 快速上手 / API 一览 / 配置：`README.md`
- 归属（execd 参考的具体设计点）：`NOTICE`
- API 契约来源：上游 OpenSandbox 仓库的 `specs/execd-api.yaml`（https://github.com/alibaba/opensandbox）

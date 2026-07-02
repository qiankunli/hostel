# hostel 设计（v1 方案）

> 状态：**待确认**。确认后按本文实现 v1。

## 一、定位与边界

**hostel = 通用 sandbox 数据面管理程序**：管理一台机器 / 一个 K8s pod 内的多个隔离执行单元，对外提供 **OpenSandbox 兼容 API**。可单机跑（laptop / VM / CI），主要跑在 pod 里，作为弱档（共享 pod、仅文件隔离）的南向 runtime；由控制面（sandctl）经 `sandbox_id → (worker pod, bed)` 路由驱动。

| hostel 做 | 不做（留给控制面 sandctl） |
|---|---|
| bed 生命周期、exec / file、共享多租服务（Chromium/Jupyter…）管理 | 调度选 pod、workspace 供给、信任分档路由、计费 / 配额 |

设计源头与差集见 `../../docs/weak-tier.md`「hostel」节；OpenSandbox execd 是主要设计参考。

## 二、核心模型：bed

- **bed = 隔离单元 = 北向一个 sandbox**：一个 workspace 目录 + 一个常驻 shell + 自己的 mount namespace（bwrap 下）。名字与 hostel 对称。
- **默认 bed 兜底**：请求不带 bed id → 落到 `default` bed。调用方可完全无视 bed 概念（单租户体验）。OpenSandbox spec 不强制 session 概念，故 bed 不与其冲突。
- **bed 路由**：HTTP header `X-Hostel-Bed`（或 query `bed`），缺省 default。
- 一个 pod 只用 default bed = 独占；多 bed = 共享，每 bed 仍有私有 ns / workspace / shell state。
- **idle GC**：bed 空闲超时回收（默认 30min，可配；default bed 永不回收）。

## 三、通用 managed-service 框架

**通则**：重资产、自带多租能力的服务，由 hostel 在 bed 外统一管理**一份**，用应用**原生**的租户机制切分，产物落对应 bed 的 workspace。Chromium、Jupyter 各是一个实例（execd 的 `/code` 委托 Jupyter、我们 `as serve` 的 Chromium 是两个先例）。

内部接口（非 HTTP，是 hostel 内插件点）：

```go
type ManagedService interface {
    Name() string
    AcquireTenant(bedID, workspace string) (Tenant, error) // 取/建本 bed 的切片
    ReleaseTenant(bedID string) error                       // bed 删除/idle 时调
    Healthy() bool
}
```

| 维度 | Chromium 实例 | Jupyter 实例 |
|---|---|---|
| 共享进程 | 一个 Chromium（pod ns，bed 看不见） | 一个 Jupyter Server |
| 租户切片 | BrowserContext（~ms 创建） | per-bed kernel |
| 产物路由 | 下载 → `<bed>/downloads` | 输出 → `<bed>` |
| 所有权 | hostel 强制 bed 只碰自己的切片 | 同 |
| HTTP 面 | v1.1 `/v1/browser/*` | v1.1 `/code` 或 `/v1/jupyter/*` |

各服务 HTTP 面自定义（navigate vs run code 无法统一），通用的是**内部接口 + bed 拆除钩子**。

**v1 只做钩子**：`bed.Manager` 持有一个（v1 为空的）service registry，Delete / CollectIdle 时遍历 `ReleaseTenant(bedID)`。实例（Chromium/Jupyter）推 v1.1，此钩子让其 drop-in。

## 四、API（OpenSandbox 兼容，响应 JSON 结构对齐 execd）

**v1 实现**：
- `/ping`、`/healthz`（isolator 名 + 可用性 + bed 数）
- `/files/*`：info、mv、permissions、search、replace、upload、download、DELETE
- `/directories/*`：list、create、delete
- `/command`（SSE）：前台走常驻 shell（有状态）；后台走独立隔离进程 + `/command/status/{id}` + `/command/{id}/logs`
- `/session`：bash 会话 create / run / delete（= bed 常驻 shell 的显式句柄）
- `/v1/beds`：CRUD + capabilities（hostel 特有，bed 管理）

**v1 不做（v1.1+）**：`/code`（委托 Jupyter，AS 用不上，砍）、`/pty` WS、`/v1/isolated/*` 的 diff/commit/persist（execd 自己也没实现）。

## 五、isolation

- `direct`（默认，全平台）：仅 chdir 到 workspace，无隔离——dev / 可信单租户；
- `bwrap`（linux，build tag）：new mount/pid/uts/ipc ns + RO 根 + workspace bind；非 linux 退化 direct；
- 更强档（真 setuid / seccomp / per-bed cgroup）v1.1 按 OSEP-0013 增补。

## 六、数据面

- **workspace = `<root>/<bedID>` 目录**；pod 里 `<root>` 是共享 RWX FS 的 bind → bed 目录天然持久、跨 pod、ms 级绑定；
- overlay / upper（CoW）**v1 不做**：持久数据走 rw-bind，overlay 留临时态，v1.1 再加（内核 overlayfs 的 upper 不能放网络盘，见 weak-tier.md）。

## 七、目录结构

```
hostel/
├── cmd/hostel/main.go
├── internal/
│   ├── config/      flags + env（HOSTEL_*）
│   ├── bed/         bed 模型 / manager / 常驻 shell（marker 分帧）/ idle GC / service registry 钩子
│   ├── isolation/   Isolator 接口 + direct + bwrap(linux)
│   ├── fsops/       bed-workspace-rooted 文件操作（路径逃逸防护）
│   ├── service/     ManagedService 接口 + registry（v1 空）
│   └── web/         gin：router / sse / files / command / beds / errors（薄适配层）
├── docs/design.md
├── Makefile / README.md / NOTICE / .gitignore
```

**关键约束**：`bed` / `fsops` / `shell` / `isolation` 纯 Go、**不含任何 HTTP 类型** → 换框架只动 `web/`。

## 八、技术选型决策

| # | 决策 | 结论 | 依据 |
|---|---|---|---|
| 1 | HTTP 框架 | **gin** | 与 execd 一致（execd 即 gin）；gin/hertz 皆可 go get（byted proxy），可用性非变量 |
| 2 | 移植方式 | **净重写，execd 作参考** | 搬设计（bwrap argv / marker-shell / fs 防护 / UpperManager）；同为 gin，可直接借 execd 的 handler 片段（保 Apache-2.0 attribution） |
| 3 | managed-service | v1 只留 `ReleaseTenant` 钩子，实例推 v1.1 | Chromium 只是实例之一，框架先立 |
| 4 | v1 范围 | 砍 `/code`，PTY / cgroup / seccomp / overlay-commit 推 v1.1 | 先跑通 bed + exec + file 主干 |

> hertz 备选记录：若 hostel 将来产品化、要接字节内部可观测 / 服务网格，可迁 hertz；因 web 层是薄适配、核心零框架依赖，迁移成本集中在 `web/` 一层。本次为与 execd 一致选 gin。

## 九、v1 交付物

单二进制 `hostel`，`--isolation direct` 本机起、curl 通 `/files` `/directories` `/command`(SSE) `/session` `/v1/beds` `/healthz`；`go build` + `go test` 绿；README 记两层模型（bed = 隔离单元 / spec 原语在 bed 内）+ 决策 + roadmap。

## 十、持久化：S3-backed bed 快照 / 恢复（v1.1，设计）

**要解决的缺口**：bed 的 workspace 是本地目录（能进 mount ns、快），但 pod 重启 / 换 pod 就没了。而**内核 overlayfs 的 upper 不能放网络盘**（NFS 不支持 whiteout/xattr），"upper 直接落共享盘"这条路走不通。

**方案**：workspace 保持本地普通目录（不碰 overlay），另设一层对象存储做**持久后备**，在 bed 生命周期边界**同步上去 / 恢复下来**：

```
create bed(带 bed id) → store.Exists? → Restore 到本地 workspace 再放行
bed 活着            → 本地读写，无网络往返
idle / delete / checkpoint → 本地目录 → 打包同步到 s3://bucket/<prefix>/<bedID>/
```

这即"**持久身份（S3 对象）+ 可弃计算（本地 bed）**"，是**文件粒度**的 snapshot/restore——比 microVM 内存快照便宜一个量级，也正是 OSEP-0013 Phase 2（diff/commit/persist，OpenSandbox 自己都没实现）用 S3 sync 的更简单实现（同步普通目录，非 overlay upper）。

**抽象**（与 `Isolator` 同构，core 保持 store-agnostic）：

```go
type Store interface {
    Exists(bedID string) (bool, error)
    Restore(bedID, dir string) error   // create/resume 时,放行前拉下来
    Persist(bedID, dir string) error   // idle/delete/checkpoint 时,推上去
}
```

backend：`noop`（默认，laptop 零依赖）· `s3`（S3 兼容 API：AWS / MinIO / 火山 TOS / Ceph 皆可）。

**接入**：`Manager.Resolve` 若 `store.Exists(bedID)` → Restore 后放行；`Delete`/idle → Persist；新增 `POST /v1/beds/:id/checkpoint`（+ 可选 `/restore`）；capabilities 报 `persistence: s3`。配置 `--store` / `--s3-bucket` / `--s3-prefix` / `--s3-endpoint`（creds 走 AWS SDK 环境链）/ `--persist-on-idle` / `--persist-interval`。

**关键决策**：
- **persist 触发**：idle + delete + 显式 checkpoint + 可选周期兜底；**不每写必传**。周期 + on-idle 决定"崩溃丢多少"的窗口。
- **粒度**：v1.1 先**整包 tarball 一个对象**（原子、可版本化、简单，小文件海比 per-object sync 更快），接受 O(size)；后续再上增量（mtime+size/hash 差量）或内容寻址去重（restic 式，便宜历史）。
- **一致性**：活着的 bed 边写边传会不一致 → **空闲（无运行命令）时才 snapshot**；显式 checkpoint 先静默、打包、恢复。
- **单写者**：两个 hostel 同时 resume 同一 bed id 会互相覆盖（last-writer-wins）；hostel 可放 S3 软 lease 兜底，但"一个 bed id 同时只在一个 hostel 活"的**权威保证是控制面（sandctl 的类 RWO 独占）**。

**诚实边界**：sync-at-boundary **≠** 实时共享 FS（两 pod 不能同时 live 读写同一 bed；那要回网络 FS 那条路）；崩溃丢 last-sync 之后的改动（窗口靠周期/on-idle 压小，非零）。对"一 conv 一 bed、之后可能换 pod 恢复"的模型，边界同步正好且简单。

## 十一、Roadmap（v1.1+）

bwrap 全量（seccomp memfd / 真 setuid / 每 bed cgroup v2 子组）· **S3 Store 持久化（见 §十）** · overlay CoW（临时层）· PTY WS · Chromium & Jupyter managed-service 实例 · sandctl 弱档 driver 对接 · 产品化外壳（API 版本化、独立发布）。

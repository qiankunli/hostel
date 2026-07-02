# hostel 设计（v1 方案）

> 状态：**待确认**。确认后按本文实现 v1。

## 一、定位与边界

**hostel = 面向 AI agent 的 sandbox runtime**：管理一台机器 / 一个容器内的多个隔离执行单元，对外提供 **OpenSandbox 兼容 API**。可单机跑（laptop / VM / CI），也可作为多租户共享实例的 in-process runtime，由上层调度系统按 `sandbox_id → (实例, bed)` 路由驱动。

| hostel 做 | 不做（留给上层调度系统） |
|---|---|
| bed 生命周期、exec / file、共享多租服务（Chromium/Jupyter…）管理 | 调度选 pod、workspace 供给、信任分档路由、计费 / 配额 |

OpenSandbox execd 是主要设计参考。

## 二、核心模型：bed

- **bed = 隔离单元 = 对外一个 sandbox**：一个 workspace 目录 + 一个常驻 shell + 自己的 mount namespace（bwrap 下）。名字与 hostel 对称。
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

## 六、文件与数据

- **workspace = `<root>/<bedID>` 目录**；pod 里 `<root>` 是共享 RWX FS 的 bind → bed 目录天然持久、跨 pod、ms 级绑定；
- overlay / upper（CoW）**v1 不做**：持久数据走 rw-bind，overlay 留临时态，v1.1 再加（内核 overlayfs 的 upper 不能放网络 FS，见 `persistence.md`）。

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
| 1 | HTTP 框架 | **gin** | 与 execd 一致（execd 即 gin）；gin/hertz 皆可 go get，可用性非变量 |
| 2 | 移植方式 | **净重写，execd 作参考** | 搬设计（bwrap argv / marker-shell / fs 防护 / UpperManager）；同为 gin，可直接借 execd 的 handler 片段（保 Apache-2.0 attribution） |
| 3 | managed-service | v1 只留 `ReleaseTenant` 钩子，实例推 v1.1 | Chromium 只是实例之一，框架先立 |
| 4 | v1 范围 | 砍 `/code`，PTY / cgroup / seccomp / overlay-commit 推 v1.1 | 先跑通 bed + exec + file 主干 |

> hertz 备选记录：若 hostel 将来产品化、要接字节内部可观测 / 服务网格，可迁 hertz；因 web 层是薄适配、核心零框架依赖，迁移成本集中在 `web/` 一层。本次为与 execd 一致选 gin。

## 九、v1 交付物

单二进制 `hostel`，`--isolation direct` 本机起、curl 通 `/files` `/directories` `/command`(SSE) `/session` `/v1/beds` `/healthz`；`go build` + `go test` 绿；README 记两层模型（bed = 隔离单元 / spec 原语在 bed 内）+ 决策 + roadmap。

## 十、专题设计文档

bed 的三个正交维度各有专门文档，本文只留一句定位：

- **数据隔离**（一个 bed 不能读写另一个 bed / 宿主的数据；tmpfs 遮蔽兄弟 bed + `/workspace` 规范挂载）：`data-isolation.md`
- **数据持久化**（本地 workspace 是工作副本，S3 快照是持久身份；生命周期边界同步）：`persistence.md`
- **资源隔离**（per-bed cgroup v2 子组防吵闹邻居；方案已记、实现推后）：`resource-isolation.md`
- **amenity 共享设施**（Chromium/Jupyter 等重资产进程共享、按 bed 切租、bed 级动作不裸暴露 CDP）：`amenity.md`

## 十一、Roadmap（v1.1+）

数据隔离补强（`data-isolation.md`，先行）· S3 Store 持久化（`persistence.md`）· per-bed cgroup（`resource-isolation.md`，推后）· bwrap 安全纵深（seccomp memfd / 真 setuid）· overlay CoW（临时层）· PTY WS · Jupyter amenity 实例 · 交互动作全集 · 上层调度系统对接 · 产品化外壳（API 版本化、独立发布）。

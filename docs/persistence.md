# bed 数据持久化方案（S3 快照 / 恢复）

bed 的 workspace 是本地目录，pod 重启 / 换 pod 即丢。本文回答：**如何让 bed 的数据活得比承载它的进程/pod 久**。数据隔离见 `data-isolation.md`，资源隔离见 `resource-isolation.md`。

## 一、理念

1. **持久身份 + 可弃计算**：bed 的持久身份是对象存储里的一份快照（`s3://bucket/<prefix>/<bedID>/`），本地 workspace 只是它的**工作副本**。计算（pod、hostel 进程、bed 内进程）随时可弃，数据不随之陪葬。
2. **为什么不是共享文件系统**：直觉方案是把 workspace 直接放 NFS/共享盘。两个障碍——**内核 overlayfs 的 upper 不能放网络 FS**（不支持 whiteout/xattr，未来上 overlay CoW 就堵死）；且共享 FS 的每次读写都付网络往返，而 bed 活着时的读写是热路径。**本地目录 + 边界同步**把网络成本从"每次 IO"移到"生命周期边界"。
3. **文件粒度快照，比 microVM 便宜一个量级**：这即 OSEP-0013 Phase 2（diff/commit/persist，OpenSandbox 自己未实现）的更简单实现——同步的是普通目录，不是 overlay upper，也不是内存镜像。

## 二、流程

```
create/resume bed(bedID) ──→ store.Exists(bedID)?
                                ├─ 是 → Restore 到本地 workspace，再放行请求
                                └─ 否 → 空 workspace 直接放行
bed 活着                  ──→ 本地读写，零网络往返
idle / delete / checkpoint ──→ 静默（无运行中命令）→ 打包 → Persist 到 S3
```

接入点（锚点）：`bed.Manager.Resolve`（restore）、`Delete` / idle GC（persist）、新增 `POST /v1/beds/:id/checkpoint`（显式持久化，+ 可选 `/restore`）；capabilities 报 `persistence: s3|noop`。

## 三、关键设计

### 1. Store 抽象（与 Isolator 同构，core store-agnostic）

```go
type Store interface {
    Exists(bedID string) (bool, error)
    Restore(bedID, dir string) error   // create/resume 时，放行前拉下来
    Persist(bedID, dir string) error   // idle/delete/checkpoint 时，推上去
}
```

backend：`noop`（默认，laptop 零依赖）· `s3`（S3 兼容 API：AWS / MinIO / 火山 TOS / Ceph 皆可）。配置：`--store` / `--s3-bucket` / `--s3-prefix` / `--s3-endpoint`（creds 走 AWS SDK 标准环境链）/ `--persist-on-idle` / `--persist-interval`。

### 2. persist 触发：边界同步，不每写必传

idle 超时 + delete + 显式 checkpoint + 可选周期兜底。每次写都传太吵且拿不到一致快照；周期 + on-idle 共同决定"崩溃丢多少"的窗口。

### 3. 粒度：先整包 tarball，后增量

v1.1 一个 bed 一个 tarball 对象——原子、可版本化、实现简单；**小文件海**（node_modules）恰是 per-object sync 的死穴，tarball 反而更快。接受 O(size)。后续演进：mtime+size/hash 差量（`aws s3 sync` 语义）或内容寻址去重（restic 式，历史快照便宜）。

### 4. 一致性：静默后快照

活着的 bed 边写边传会拿到撕裂的快照。只在**空闲（无运行中命令）**时 snapshot；显式 checkpoint 先静默（暂停接新命令）→ 打包 → 恢复接单。

### 5. 单写者：软 lease + 上层调度系统权威

两个 hostel（不同 pod）同时 resume 同一 bedID → persist 互相覆盖（last-writer-wins）。hostel 侧可在 S3 放**软 lease 对象**兜底提示冲突，但"一个 bedID 同时只在一个 hostel 活着"的**权威保证属于上层调度系统**（对 bed 归属做类 RWO 独占），hostel 不硬解分布式锁。

## 诚实边界

- **边界同步 ≠ 实时共享 FS**：两个 pod 不能同时 live 读写同一个 bed；要那个语义就得回共享 FS 路线（并放弃 overlay 演进）。对"一 conv 一 bed、之后可能换 pod 恢复"的模型，边界同步正好且简单。
- **崩溃丢 last-sync 之后的改动**：窗口靠周期 + on-idle 压小，非零。要零丢失只能实时 FS，另一套复杂度。

## 状态

设计完成，待实现（v1.1）。实现顺序上排在数据隔离补强之后。

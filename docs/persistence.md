# bed 数据持久化方案（S3 快照 / 恢复）

bed 的 workspace 是本地目录，pod 重启 / 换 pod 即丢。本文回答：**如何让 bed 的数据活得比承载它的进程/pod 久**。数据隔离见 `data-isolation.md`，资源隔离见 `resource-isolation.md`。

## 一、理念

1. **持久身份 + 可弃计算**：bed 的持久身份是对象存储里的一份快照（`s3://bucket/<prefix>/<bedID>/`），本地 workspace 只是它的**工作副本**。计算（pod、hostel 进程、bed 内进程）随时可弃，数据不随之陪葬。
2. **为什么不是共享文件系统**：直觉方案是把 workspace 直接放 NFS/共享盘。两个障碍——**内核 overlayfs 的 upper 不能放网络 FS**（不支持 whiteout/xattr，未来上 overlay CoW 就堵死）；且共享 FS 的每次读写都付网络往返，而 bed 活着时的读写是热路径。**本地目录 + 边界同步**把网络成本从"每次 IO"移到"生命周期边界"。
3. **文件粒度快照，比 microVM 便宜一个量级**：这即 OSEP-0013 Phase 2（diff/commit/persist，OpenSandbox 自己未实现）的更简单实现——同步的是普通目录，不是 overlay upper，也不是内存镜像。

## 二、流程

```
create/resume bed(bedID) ──→ store.Stat(bedID)?
                                ├─ 有快照 → 本地 luggage 的 generation ≥ 快照的？
                                │            ├─ 是 → warm start（免下载，直接用现场）
                                │            └─ 否 → 丢弃过期现场，Restore 后放行
                                └─ 无快照 → 空 workspace（或 noop 下的遗留现场）直接放行
bed 活着                  ──→ 本地读写，零网络往返
idle / delete / checkpoint ──→ 静默（无运行中命令）→ 打包 → Persist 到 S3
evict 完成                ──→ 本地目录留作 luggage（现场缓存），交磁盘水位 GC 管
```

接入点（锚点）：`bed.Manager.Resolve`（restore）、`Delete` / idle GC（persist）、新增 `POST /v1/beds/:id/checkpoint`（显式持久化，+ 可选 `/restore`）；capabilities 报 `persistence: s3|noop`。

## 三、关键设计

### 1. Store 抽象（与 Isolator 同构，core store-agnostic）

```go
type Store interface {
    Stat(bedID string) (*SnapshotInfo, error)      // nil=无快照；含 generation/bytes，S3 上是 HEAD，免下载
    Restore(bedID, dir string) error               // create/resume 时，放行前拉下来
    Persist(bedID, dir string, generation int64) error // idle/delete/checkpoint 时，推上去
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

## 四、bed 生命周期与流转

bed 在单个 hostel 里是**瞬时的**（可驱逐、可恢复），因此需要显式生命周期，而不是"在 map 里/不在 map 里"的隐式状态。

### 状态

```
                Resolve(新 id)
   ABSENT ──────────────────────→ ACTIVE ←─────────────┐
   （无快照）                        │                   │ 新请求 touch
                                    │ idle 超时 / 显式驱逐│ （驱逐被取消）
                                    ▼                   │
                                 EVICTING ──────────────┘
                                    │ persist 成功 且 期间无新活动
                                    ▼
   DORMANT（不占内存、不占 max-beds 名额；快照即身份）
      │                                    │
      │ Resolve(同 id) → restore → ACTIVE  │ purge（连快照删除）
      ▼                                    ▼
   ACTIVE                               ABSENT
```

- **ACTIVE**：在内存 map 里、正常服务、占 max-beds 名额。
- **EVICTING**（过渡态）：persist 进行中。**期间新请求不被拒绝**——touch 即取消驱逐（服务优先于回收）；persist 完成后原子复查"期间是否有新活动"，有则中止移除、留在 ACTIVE（本轮快照仍有效，不白传）。这关掉了"persist 窗口写入丢失"的竞态。
- **DORMANT**：不在任何 hostel 内存里，唯一存在形式是 S3 快照。判定 = `store.Exists`，**不落额外注册表**——快照本身就是权威记录（哪台 hostel 都能凭 bedID 复活它）。
- **RESTORING 不是对外状态**：restore 在 `Resolve` 内同步完成，调用方只看到"第一个请求慢一点"。

### 动词与 API 语义

| 动作 | 语义 | API |
|---|---|---|
| **evict**（驱逐） | 释放计算、保留身份：persist → 出 map → 名额释放 | idle GC 自动；`DELETE /v1/beds/:id`（默认） |
| **purge**（清除） | 身份终结：驱逐 + 删除 S3 快照 | `DELETE /v1/beds/:id?purge=true` |
| **checkpoint** | 打快照，不动状态 | `POST /v1/beds/:id/checkpoint` |
| **resume** | DORMANT → ACTIVE（对调用方透明） | 任意携带该 bedID 的请求 |

`GET /v1/beds` 只列 ACTIVE：DORMANT 集合的权威在对象存储（及上层调度系统的记账），hostel 不维护第二份全量索引。调度器要的"本机全图"（含 luggage）走 `GET /v1/inventory`（见下节）。

### luggage：现场缓存与 generation

**原则：快照是唯一事实，其余一切都是缓存。** evict 不再删本地目录——它留下来成为 **luggage**（寄存行李）：DORMANT bed 的本机热副本。同机 resume 时若现场足够新就直接用（warm start，免下载）；判"够新"用 **generation**——meta.json 里单调递增的 persist 计数，随快照进对象元数据（`Stat` 一次 HEAD 就能比对）。现场落后于快照（bed 期间在别的实例跑过）则整目录丢弃后重新 Restore，**只换不合**。为什么不用时间戳判序：bed 跨机迁移时钟有偏差，序会反转；时间戳只做观测（`last_persisted_at` / `last_used_at`），判序只认 generation。

luggage 是纯缓存，删错零正确性代价（多付一次 Restore），所以磁盘上限走独立水位而不占 max-beds：超过 `--luggage-high-bytes` 时按"generation 过期优先（纯垃圾）→ LRU"的顺序删到 `--luggage-low-bytes` 以下。这个排序是 cost-aware 驱逐的演化缝，v1 只认新旧。

`GET /v1/inventory` 把容量 + 全部本机 bed（active/evicting/luggage + generation）一次给上层调度器：谁有新鲜现场就优先派谁（省下载），但这只是 hint——新鲜度在激活时兜底复查，调度器拿着过期数据路由也只是慢、不会错。**单写者约束（§三.5）不变**：同 bedID 双活的防线在调度器租约 + 将来 store 条件写，inventory 不承担正确性。

### noop store 下的退化语义

没有快照，luggage 就是唯一副本：evict 后同机 resume 仍然有效（比 v1 的"evict 即销毁"更好），但 luggage GC 删掉它 = 数据销毁，且 bed 不可跨实例迁移（inventory 的 `store: "noop"` 明示这一点）。部署要么接受 bed 数据只活在本机，要么开 s3。healthz 的 `persistence` 字段让调用方能区分这两种世界。

### bed 目录分层（配套）

```
{workspace-root}/{bedID}/        ← 快照打包的根（meta + data 一起上 S3）；evict 后整体留作 luggage
  meta.json   # hostel 私有：created_at、last_persisted_at、generation、last_used_at（将来：manifest、lease）
  *.local     # 约定：本机私有元数据，不进快照（当前无，留位）
  data/       # bed 的 workspace：唯一 bind 给沙箱的部分
```

**快照内容 = meta（可移植部分）+ data**：DORMANT 的唯一存在形式是快照，元数据若只留本地，驱逐即丢、换一台 hostel 复活就残缺。约定"默认可移植"——meta.json 随 data 一起打包；确属本机私有的状态用 `*.local` 后缀排除在打包之外。

meta 对 bed 内代码**不可见**（bwrap 只 bind `data/`，root 整体被 tmpfs 遮蔽）——沙箱代码不能篡改 hostel 的记账。`last_persisted_at` 落盘使 dirty 追踪跨进程重启仍正确。

## 诚实边界

- **边界同步 ≠ 实时共享 FS**：两个 pod 不能同时 live 读写同一个 bed；要那个语义就得回共享 FS 路线（并放弃 overlay 演进）。对"一 conv 一 bed、之后可能换 pod 恢复"的模型，边界同步正好且简单。
- **崩溃丢 last-sync 之后的改动**：窗口靠周期 + on-idle 压小，非零。要零丢失只能实时 FS，另一套复杂度。

## 实现状态

已实现（`internal/store/` + `bed.Manager` 生命周期钩子）：

- `Store` 接口 + `noop`（默认）/ `s3`（aws-sdk-go-v2，`--s3-endpoint` 支持 S3 兼容存储，凭据走 AWS SDK 标准链）；一 bed 一 tarball（`<prefix>/<bedID>.tar.gz`），解包带 zip-slip/逃逸 symlink 防护
- restore-on-create（`Resolve` 新建时，restore 失败即拒绝服务——静默空启动等于数据丢失）、**persist 失败中止 Evict**（毁掉唯一副本比留着 bed 重试更糟）、`POST /v1/beds/:id/checkpoint`、`--persist-interval` 周期兜底（只传 dirty bed，watermark 落 meta.json 跨重启有效）
- **生命周期已落地**（§四）：`Evict`（EVICTING 期间新活动**取消驱逐**——关掉 persist 窗口写丢竞态）、`Purge`（`DELETE ?purge=true`，default bed 拒绝）、快照根 = bed 目录（meta+data，顶层 `*.local` 排除）、`GET /v1/beds` 报 `state: active|evicting`、驱逐被并发活动取消时 API 返回 409 `BED_BUSY`
- capabilities / healthz 报 `persistence: noop|s3`
- **luggage 已落地**：evict 留现场 + `LastUsedAt` 盖章、`Resolve` 按 generation 判新鲜（warm start / 丢弃重拉）、`--luggage-high/low-bytes` 水位 GC（stale 优先 → LRU，rename-under-lock 防与 Resolve 竞态）、`GET /v1/inventory` 报容量与全部本机 bed；generation 存 S3 object user metadata（`Stat`=HEAD 免下载）

与设计的两处偏差：checkpoint **暂不硬静默**（不暂停接单，调用方自选空闲点打快照）；未单设 `--persist-on-idle`（idle GC 走 Delete，persist-before-delete 天然覆盖）。s3 backend 未在本地 CI 验证（无 MinIO），tar/生命周期逻辑有单测覆盖。

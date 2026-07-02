# bed 资源隔离方案（per-bed cgroup）

> **状态：方案先记，实现推后**（当前优先级：数据隔离 > 持久化 > 资源隔离）。

聚焦**资源消耗隔离**：一个 bed 跑飞 CPU/内存/进程数，不能拖垮同一 hostel 里的其它 bed。数据隔离见 `data-isolation.md`，持久化见 `persistence.md`。

## 一、理念

1. **多 bed 密度成立的前提是资源公平**：把 N 个 bed 挤进一个 pod，第一个坏掉的不是安全，是"吵闹邻居"——一个 `while(1)` 或内存泄漏就吃光整个 pod 配额，殃及全部租户。cgroup 是密度目标的地基；seccomp/setuid 那类安全纵深是加高，单独设计。
2. **复用内核原语，不发明配额器**：cgroup v2 已提供层级化的 cpu/memory/pids 控制，hostel 只做两件事——**给每个 bed 建一个子组、把 bed 的进程放进去**。不在用户态做任何"测量-驳回"式的自制限流。
3. **与 Isolator 正交**：隔离（namespace 视图）与限额（资源上限）是两个维度——bwrap 不管 cgroup。故新设 `Limiter` 接口与 `Isolator` 并列，而不是塞进 Isolator；direct 模式（无 bwrap）同样可以有 cgroup 限额，两者自由组合。

## 二、流程

```
hostel 启动 → 探测 cgroup v2 可写（/sys/fs/cgroup/<hostel-scope>/ 能建子目录且
              cgroup.subtree_control 可开 cpu memory pids）
                ├─ 否 → Limiter=noop，capabilities 报 resource_limits: false
                └─ 是 → 每个 bed 首次启动进程时：
                        1. mkdir <scope>/beds/<bedID>/
                        2. 写 cpu.max / memory.max / memory.swap.max / pids.max
                        3. 启动进程时经 CLONE_INTO_CGROUP 直接落入该组
                           （Go: SysProcAttr{UseCgroupFD, CgroupFD}，Linux 5.7+）
bed 删除 / idle GC → 杀进程组 → rmdir cgroup 子目录
```

关键点：用 `CLONE_INTO_CGROUP` 而非"启动后写 `cgroup.procs`"——后者在 fork 与写入之间有窗口，进程可能已 fork 出子进程逃出限额。

## 三、关键设计

### 1. Limiter 抽象

```go
type Limits struct {
    CPUMax    string // cgroup v2 cpu.max 语法，如 "50000 100000"（0.5 核）
    MemoryMax int64  // bytes；0 = 不限
    PidsMax   int64
}
type Limiter interface {
    Available() bool
    Prepare(bedID string, l Limits) (cgroupFD int, err error) // 建组+写限额，返回可 CLONE 的 fd
    Release(bedID string) error                               // rmdir
}
```

backend：`noop`（默认 / 非 Linux / 无写权限）· `cgroupv2`。与 `Store`、`Isolator` 同一模式：core 只依赖接口。

### 2. 限额来源：默认值 + 每 bed 覆盖

配置 `--bed-cpu-max` / `--bed-memory-max` / `--bed-pids-max` 给全局默认；`POST /v1/beds` body 可带 `limits` 覆盖（调用方按租户等级差异化）。**默认建议偏保守**（如 1 核 / 2GiB / 256 pids）：宁可让重任务显式申请，不让默认值放任吵闹邻居。

### 3. 前提：pod 内 cgroup v2 委派

容器里能否建子组取决于运行时把容器 cgroup 以何种权限挂给进程：
- K8s + cgroup v2 节点：容器内 `/sys/fs/cgroup` 通常挂 ro，需要 pod 配置（`securityContext` 或运行时支持）拿到自己 scope 的写权限；
- 拿不到 → `Available()==false` → noop 降级 + capabilities 如实上报（同 bwrap 缺失时的哲学：不假装隔离）。
- 部署侧要求写进 helm/values 注释，属部署契约而非代码逻辑。

### 4. managed-service 的位置

Chromium/Jupyter 等共享服务**不进任何 bed 的 cgroup**（它们是 per-hostel 单例），放独立的 `<scope>/services/<name>/` 子组单独限额。per-tenant（bed）粒度的用量归因是已知难点——浏览器进程模型不按租户划分——先接受服务级限额，租户级归因推后。

### 5. 测试策略

- **mac/CI 可跑**：Limits→文件内容的构造单测；noop 降级路径。
- **Linux 真验证**（devbox）：bed 内 `stress`/fork 炸弹，断言限额生效（CPU throttle、OOM kill、EAGAIN）且邻居 bed 命令延迟不受显著影响；删除 bed 后 cgroup 目录回收。

## 非目标

- 磁盘配额（io.max 管带宽不管容量；容量配额靠 overlay 上限或 fs quota，与持久化/overlay 一并考虑）；
- 网络带宽限速；
- 租户级（bed 级）managed-service 用量归因。

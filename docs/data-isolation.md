# bed 数据隔离方案

聚焦**数据隔离**：一个 bed 不能读、更不能写另一个 bed（或宿主）的数据。资源消耗隔离见 `resource-isolation.md`，持久化见 `persistence.md`，安全纵深（seccomp / 真 setuid）推后单独设计。

## 一、理念

1. **信任模型**：hostel 面向可信 / 半可信代码，多 bed 挤一个进程/容器。在这个模型里，**可读即泄漏**——bed A 读到 bed B 的 workspace，和写坏它一样是隔离失败。数据隔离是多租户成立的底线，比 syscall 加固更优先。
2. **bed 的全部数据 = 一个目录**：bed 的全部持久数据就是它的 workspace 目录（`<workspace-root>/<bedID>`）。隔离方案只需回答一个问题：**如何让 bed 的文件视图里只有自己的 workspace**。持久化（S3 快照/恢复）建立在同一个目录之上，见 `persistence.md`。
3. **两套路径语义应当收敛**：v1 里 `/workspace` 是 file API 的虚拟前缀（`fsops.Resolve` rebase），而 shell 里 cwd 是宿主真实路径——同一个文件两个名字。数据隔离补强顺带把 `/workspace` 变成 bed 内的**真实挂载点**，file API 与 shell 看到同一个 `/workspace`，与 OpenSandbox SDK 语义完全对齐。

## 二、流程（bwrap 模式下启动 bed 内进程）

每次为 bed 启动进程（常驻 shell / 一次性命令），`Isolator.Wrap` 构造 bwrap 沙箱，目标文件视图：

```
/            ← 宿主根，只读（工具链、解释器可用）
/workspace   ← 只有自己：bind <workspace-root>/<bedID> → /workspace（rw）
/tmp         ← per-process tmpfs（不跨 bed、不落盘）
/dev /proc   ← 全新挂载
<workspace-root>  ← tmpfs 遮蔽：兄弟 bed 的目录不可见（不是"不可读"，是"不存在"）
/root /home  ← tmpfs 遮蔽（宿主用户数据不可见）
cwd          ← /workspace
```

对应 argv 骨架（锚点：`internal/isolation/bwrap_linux.go`）：

```
bwrap --unshare-pid --unshare-uts --unshare-ipc \
  --ro-bind / / \
  --dev /dev --proc /proc --tmpfs /tmp \
  --tmpfs <workspace-root> \            # 遮蔽所有 bed 目录
  --tmpfs /root --tmpfs /home \         # 遮蔽宿主用户数据（及存在时的 /run/secrets、/var/run/secrets）
  --bind <workspace-root>/<bedID> /workspace \  # 只挂自己，且给规范名
  --unsetenv <密钥形 env>... \           # 见 §4：宿主凭据不进 bed
  --chdir /workspace --die-with-parent -- <cmd>
```

顺序敏感：`--tmpfs <workspace-root>` 必须在 `--ro-bind / /` 之后（后挂的盖前面的），`--bind ... /workspace` 与遮蔽无冲突（挂载点不同）。

## 三、关键设计

### 1. 为什么要遮蔽，而不是只依赖 RO

v1 的 argv 是 `--ro-bind / /` + bind 自己的 workspace（宿主路径原位）。RO 根挡住了写，但 `<workspace-root>/` 下**所有兄弟 bed 目录仍然可读**——多租户下这是真实的数据泄漏洞。修法不是给兄弟目录改权限（同 uid 下权限位挡不住），而是**让它们从视图里消失**：tmpfs 盖住 workspace-root，再只把自己的目录 bind 回来。

### 2. `/workspace` 规范挂载：统一两套路径语义

bind 目标从"宿主原位路径"改为 bed 内固定的 `/workspace`：

- shell 里 `cd /workspace`、`cat /workspace/a.txt` 直接可用，与 file API 的虚拟前缀同名同物；
- 命令/会话的默认 cwd 从"宿主真实目录"变为 `/workspace`，SDK 拿到的路径在两个通道间可以互换；
- **direct 模式（无 bwrap，mac/dev）无法造挂载点**，维持 v1 语义（cwd=宿主真实目录，`/workspace` 仅 file API 前缀）。两种模式的差异收敛为一条注释良好的能力位（capabilities 报 `workspace_mount: true/false`），调用方可探测。

### 3. workspace-root 外部可配

已支持：`--workspace-root` flag / `HOSTEL_WORKSPACE_ROOT` env（默认 `/workspace`）。参考 execd 的配置惯例：execd 走 `EXECD_ISOLATION_CONFIG` env → TOML 文件（`upper_root` 等）；hostel 当前配置项少，维持"每项一个 `HOSTEL_*` env"的直接形式，配置膨胀后再学 execd 收敛为单一 config 文件。

注意 workspace-root 与规范挂载点重名时（宿主 `/workspace` 作 root、bed 内也叫 `/workspace`）bwrap 序列依然成立：先 tmpfs 盖 `/workspace`，再 bind `<root>/<bedID>` → `/workspace`，自身目录作为挂载点被替换、兄弟目录被 tmpfs 吞掉。

### 4. 敏感数据遮蔽清单：文件路径 + 环境变量

文件路径最小集合：`/root`、`/home`（宿主用户数据）+ 存在时的 `/run/secrets`、`/var/run/secrets`（K8s serviceaccount token 等平台挂载凭据）——**默认遮蔽**，需要网络凭据的场景由 managed-service 层代持，而不是把凭据暴露给 bed 内任意代码。

环境变量同理（借鉴 execd strict profile 的黑名单）：hostel 进程自身 env 里密钥形变量（`*_API_KEY` / `*_TOKEN` / `*_SECRET` / `*_PASSWORD` / `AWS_*` / `K8S_*` / `KUBE_*`）经 `--unsetenv` 剥除后才进 bed。文件遮蔽挡"挂载进来的凭据"，env 剥除挡"进程继承的凭据"，两条泄漏通道都要关。

### 5. 降级行为（与 v1 一致的哲学）

- 非 Linux / 无 bwrap：退化 direct（chdir only），数据隔离**不存在**，healthz/capabilities 如实上报——不假装隔离。
- bwrap 存在但版本不支持某 flag：启动 bed 进程失败即报错，**不静默降级**（数据隔离是被明确请求的，降级等于欺骗调用方）。

### 6. 测试策略

- **mac/CI 可跑**：argv 构造单测——给定 root/bedID，断言遮蔽序列、bind 目标、顺序敏感项、env 剥除（argv 构造放在无 build tag 的 `bwrap_args.go`，exec 侧才是 `bwrap_linux.go`）。
- **Linux 真验证**（devbox）：起两个 bed，A 写文件，断言 B 内 `ls <workspace-root>` 看不到 A 的目录、`cat` A 的宿主路径报不存在；`/workspace` 内读写互通 file API。
- 回归：direct 模式行为不变（现有 web/bed 测试全绿）。

## 非目标（明确推后）

- per-bed cgroup（资源消耗隔离）——见 `resource-isolation.md`；
- seccomp / 真 setuid / userns（安全纵深）——单独设计；
- overlay CoW 临时层——与持久化（`persistence.md`）合并考虑；
- 跨 pod 实时共享 workspace——见 `persistence.md` 的诚实边界。

## 实现状态

已实现（`internal/isolation/`）：boot 时 bwrap probe（binary + **全形态 smoke**——用真实 argv 起 `true`，namespace/遮蔽/`/workspace` bind 全过一遍；宿主挂载点缺失等问题在 boot 即暴露并诚实降 direct，不再误报 `workspace_mount`）、遮蔽 argv、`/workspace` 规范挂载、cwd 模式感知映射（`web` 层 `resolveCwd`）、env 剥除、capabilities/healthz 报 `workspace_mount`。mac argv 单测绿；**Linux 真机双 bed 验证已通过**（devbox，bwrap 0.8.0 / kernel 5.15：兄弟遮蔽、规范挂载、敏感路径+env 剥除、direct 负面对照全 PASS）。真机验证同时暴露两个 bug 均已修复：宿主缺 `/workspace` 挂载点（probe 改全形态 + boot 时确保挂载点）；shell 死亡 + 未断开客户端导致全 daemon 死锁（Shell 锁职责拆分，见 `internal/bed/shell.go` LOCKING 注释）。

## 隔离能力阶梯：无 userns / 无 CAP_SYS_ADMIN 时怎么办（调研）

bwrap 的 mount namespace 遮蔽依赖 **unprivileged userns** 或 `CAP_SYS_ADMIN`。很多现代内核默认 `kernel.unprivileged_userns_clone=1`，此时 bwrap **不需要 root 也能真隔离**（devbox 特权容器已验，普通容器开了 userns 同样成立）——所以"没有 CAP_SYS_ADMIN"不等于"没有隔离"，先看内核这个开关。但确实存在两者都关的环境。按"环境给什么"降级，隔离能力分四档：

| 档 | 需要的能力 | 隔离强度 | 机制 |
|---|---|---|---|
| **bwrap** | userns 或 `CAP_SYS_ADMIN` | 强：兄弟 bed **不可见**（tmpfs 遮蔽）+ RO 根 + `/workspace` 规范挂载 + env 剥除 | mount ns |
| **landlock**（建议新增） | **无（非特权）**，内核 ≥5.13 | 中强：兄弟 bed **可见但不可访问**（open 得 EACCES），内核强制 | Landlock LSM |
| **per-bed uid** | `CAP_SETUID`（比 SYS_ADMIN 轻） | 中：跨 uid + 0700 权限位挡读写，但目录结构可见、root 可逃 | setuid/setgid |
| **direct** | 无 | 无强制隔离，仅组织性分隔（见 §5） | chdir only |

### landlock 是"无 userns"这个洞的最佳补位

**调研结论**（参考本地 clone `../go-landlock`，及同类现成品 `../greywall`——container-free、deny-by-default、面向 AI coding agent 的 landlock 沙箱）：

- **官方库 `github.com/landlock-lsm/go-landlock`**：核心 API 一行——
  `landlock.V9.BestEffort().RestrictPaths(landlock.RODirs("/usr","/bin",...), landlock.RWDirs(bedDataDir))`。调用后**本进程（含 exec 出的子进程）只能访问列出的路径**。
- **无需任何 capability**，Landlock 自 Linux 5.13 起（FS 访问控制是 ABI v1，最广部署的部分；网络限制要 ABI v4 / 内核 6.7，暂不需要）。`BestEffort()` 在老内核/无 landlock 时**优雅降级**——与 hostel"诚实降级"哲学一致。
- **集成模式已明确**（greywall 同款、与 bwrap 外部前缀同构）：landlock 只能限制**调用它的进程**，不能限制 hostel 自己（hostel 要管所有 bed、需全 FS）。做法是 **hostel 自 re-exec**：新增隐藏子命令 `hostel __confine <bedDataDir> -- <shell>`，子进程先 `RestrictPaths` 再 `exec` bash。这**正好塞进现有 `Isolator` 接口**——landlock isolator 的 `Wrap` 把 `<hostelBin> __confine <bedDir> --` 前缀进 `cmd.Args`，与 bwrap 前缀 argv 一模一样。
- **系统路径放行清单**（shell 要能跑，参考 greywall）：RO = `/usr /lib /lib64 /bin /sbin /etc /proc /sys /run /opt`；RW = 该 bed 的 `data/`（+ `/dev/null` 等）。

### landlock 相对 bwrap 的诚实差距

- **"不可访问" ≠ "不存在"**：landlock 让兄弟 bed 目录 open EACCES，但 `readdir` 父目录仍能看到它们的**名字**（存在性泄漏）；bwrap 的 tmpfs 让它们从视图消失。对数据机密性两者都挡住读，对"有没有 bed X"这类元数据 landlock 会漏。
- **无 `/workspace` 规范挂载**：路径仍是真实宿主路径，file API 与 shell 两套语义的统一（bwrap 的附带收益）在 landlock 下不自动获得。
- **仅 FS（+ 高版本部分 net/ipc）**：不带 pid/uts/ipc/net namespace，进程表、主机名等不隔离——但那些是"安全纵深"，不是数据隔离。
- **必须在把控制权交给不可信代码前 apply**，且不能限制已打开的 fd。

### 建议

新增 **`--isolation landlock`** 作为中间档，`New(mode, root)` 路由：bwrap（探测到 userns）> landlock（探测到内核 ≥5.13）> direct。boot 时同样 probe（起一个 `hostel __confine` 空跑），capabilities 报实际生效档位（如 `isolator: landlock`）。**实现待定**：本节为调研记录，clone 已落地 `../go-landlock`、`../greywall` 供参考。

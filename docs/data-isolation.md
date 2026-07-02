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

已实现（`internal/isolation/`）：boot 时 bwrap probe（binary + namespace smoke test，借鉴 execd）、遮蔽 argv、`/workspace` 规范挂载、cwd 模式感知映射（`web` 层 `resolveCwd`）、env 剥除、capabilities/healthz 报 `workspace_mount`。mac argv 单测绿；**Linux 真机双 bed 验证待跑**（devbox）。

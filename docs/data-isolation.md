# bed 数据隔离方案

聚焦**数据隔离**：一个 bed 不能读、更不能写另一个 bed（或宿主）的数据。资源消耗隔离见 `resource-isolation.md`，持久化见 `persistence.md`，安全纵深（seccomp / 真 setuid）推后单独设计。

## 一、理念

1. **信任模型**：hostel 面向可信 / 半可信代码，多 bed 挤一个进程/容器。在这个模型里，**可读即泄漏**——bed A 读到 bed B 的 workspace，和写坏它一样是隔离失败。数据隔离是多租户成立的底线，比 syscall 加固更优先。
2. **bed 的全部数据 = 一个目录**：bed 的全部持久数据就是它的 workspace 目录（`bed_home = <workspace-root>/<bedID>/data`）。隔离方案只需回答一个问题：**如何让 bed 的文件视图按当前房型兑现访问边界**。持久化（S3 快照/恢复）建立在同一个目录之上，见 `persistence.md`。
3. **路径映射是三档共同的基础契约，不是高档房型的附加能力**：请求先由 `X-Hostel-Bed` 确定 bed，再把 file API、cwd 等显式路径映射到该 bed 的 `bed_home`；hostel 不靠路径判断请求属于哪个 bed。`dorm / room / suite` 只决定映射后的数据能否被兄弟 bed 看见、访问，不得改变同一个客户端路径落到哪个 bed-local 位置。
4. **两套进程路径语义应当收敛**：`/workspace` 保留为 OpenSandbox 的规范别名，而其它客户端绝对路径同样应 rebase 到 `bed_home`。底层可以按房型使用宿主真实路径、Landlock/UID 或 mount namespace，但对调用方暴露的路径结果必须一致；不能因为 room/direct 没有 `/workspace` bind，就拒绝本可安全映射到 `bed_home` 的路径。

## 二、流程（bwrap 模式下启动 bed 内进程）

每次为 bed 启动进程（常驻 shell / 一次性命令），`Isolator.Wrap` 构造 bwrap 沙箱，目标文件视图：

```
/            ← 宿主根，只读（工具链、解释器可用）
/workspace   ← 只有自己：bind <bed_home> → /workspace（rw）
/tmp         ← per-process tmpfs（不跨 bed、不落盘）
/dev /proc   ← 全新挂载
<workspace-root>  ← tmpfs 遮蔽：兄弟 bed 的目录不可见（不是"不可读"，是"不存在"）
/root /home  ← tmpfs 遮蔽（宿主用户数据不可见）
cwd          ← /workspace
```

对应 argv 骨架（锚点：`internal/isolation/bwrap_linux.go`）：

```
bwrap --unshare-user --unshare-uts --unshare-ipc \
  --ro-bind / / \
  --dev /dev --ro-bind /proc /proc --tmpfs /tmp \
  --tmpfs <workspace-root> \            # 遮蔽所有 bed 目录
  --tmpfs /root --tmpfs /home \         # 遮蔽宿主用户数据（及存在时的 /run/secrets、/var/run/secrets）
  --bind <bed_home> /workspace \  # 只挂自己，且给规范名
  --unsetenv <密钥形 env>... \           # 见 §4：宿主凭据不进 bed
  --chdir /workspace --die-with-parent -- <cmd>
```

顺序敏感：`--tmpfs <workspace-root>` 必须在 `--ro-bind / /` 之后（后挂的盖前面的），`--bind ... /workspace` 与遮蔽无冲突（挂载点不同）。

**k8s pod 内可达性（真实集群踩点）**：以上 argv 有两处不是随手选的，是让 suite 在**普通非特权 pod** 里够得着的硬前提——

- **`--unshare-user`（而非无 userns）**：容器里 hostel 以 root 跑，无 `CAP_SYS_ADMIN`；不开 userns 时 bwrap 走特权 `clone(NEWNS)` 直接 EPERM。开 user namespace 后建 mount ns 无需宿主特权，只要内核允许非特权 userns（主流默认）。
- **`--ro-bind /proc /proc`（而非 `--unshare-pid` + `--proc /proc`）**：k8s 把容器 `/proc` 的部分路径 mask 成只读；userns 内内核禁止在 masked proc 上重挂新 procfs（`mount proc: Operation not permitted`）。pid namespace 属安全纵深、不是数据隔离（路径契约）的一部分，故弃之，改 bind 宿主 `/proc`。

**第三道闸——AppArmor（部署项，非 hostel 能自解）**：节点启用 AppArmor 时，containerd 默认 profile（`cri-containerd.apparmor.d`）**deny mount**——userns 开着、上面 argv 也对，bwrap 仍死在 `Failed to make / slave: Permission denied`。hostel 探不过就诚实降级到 room/dorm，并把 `HostFacts.apparmor_profile` 报进 `/healthz`、boot 日志点名（"userns 在、疑似 AppArmor 拦截"）。放开需给 carrier pod 打 `container.apparmor.security.beta.kubernetes.io/<容器>: unconfined` annotation——**不作为 hostel 的硬性部署要求**（客户集群未必接受该 annotation），由上层（sandctl 建 carrier 时按 k8s 版本+准入策略自适应）决定带不带。三点全不需要特权（无 CAP_SYS_ADMIN、无 privileged）。

## 三、关键设计

### 1. 为什么要遮蔽，而不是只依赖 RO

v1 的 argv 是 `--ro-bind / /` + bind 自己的 workspace（宿主路径原位）。RO 根挡住了写，但 `<workspace-root>/` 下**所有兄弟 bed 目录仍然可读**——多租户下这是真实的数据泄漏洞。修法不是给兄弟目录改权限（同 uid 下权限位挡不住），而是**让它们从视图里消失**：tmpfs 盖住 workspace-root，再只把自己的目录 bind 回来。

### 2. 统一路径映射：先选 bed，再落到 bed_home

bed 已由 `X-Hostel-Bed` 选定后，所有房型共用同一套客户端路径解析规则：

| 客户端路径 | 三档统一映射结果 |
|---|---|
| `/workspace/a.txt` | `<bed_home>/a.txt`（OpenSandbox 规范别名） |
| `/tmp/workspace/job` | `<bed_home>/tmp/workspace/job` |
| `tmp/workspace/job` | `<bed_home>/tmp/workspace/job` |

映射必须先做规范化并拒绝 `..`、symlink 等任何实际逃出 `bed_home` 的路径。房型只影响映射结果周围的墙：dorm 没有跨 bed 访问屏障，room 让兄弟访问报 EACCES，suite 进一步让兄弟路径不可见；不能让同一个输入在不同房型落到不同数据目录。

`/workspace` 的规范挂载是 suite 实现这份契约的一种**进程视图机制**，不是路径映射本身：

- suite 把 `bed_home` bind 到 `/workspace`，shell 与 file API 可直接使用同名路径；
- direct/room 即使暂时使用宿主真实路径执行 `cd`，file API、cwd 等北向显式路径仍必须先映射到同一个 `bed_home`；
- `capabilities.workspace_mount` 只表示是否存在 `/workspace` 真实挂载，不表示是否支持 bed-local 路径映射——后者是三档必备能力，不应作为可选 capability。

路径字段和命令文本要分开处理：hostel 可以直接解析 file API path、cwd 等结构化字段，但不能可靠改写任意 shell command 字符串。如果要求命令里的绝对字面量（如 `cat /tmp/workspace/job/a.txt`）也命中同一 bed-local 文件，就必须由进程文件系统视图提供对应投影或 bind，不能靠字符串替换碰运气。

### 3. workspace-root 外部可配

已支持：`--workspace-root` flag / `HOSTEL_WORKSPACE_ROOT` env（默认 `/workspace`）。参考 execd 的配置惯例：execd 走 `EXECD_ISOLATION_CONFIG` env → TOML 文件（`upper_root` 等）；hostel 当前配置项少，维持"每项一个 `HOSTEL_*` env"的直接形式，配置膨胀后再学 execd 收敛为单一 config 文件。

注意 workspace-root 与规范挂载点重名时（宿主 `/workspace` 作 root、bed 内也叫 `/workspace`）bwrap 序列依然成立：先 tmpfs 盖 `/workspace`，再 bind `<bed_home>` → `/workspace`，自身目录作为挂载点被替换、兄弟目录被 tmpfs 吞掉。

### 4. 敏感数据遮蔽清单：文件路径 + 环境变量

文件路径最小集合：`/root`、`/home`（宿主用户数据）+ 存在时的 `/run/secrets`、`/var/run/secrets`（K8s serviceaccount token 等平台挂载凭据）——**默认遮蔽**，需要网络凭据的场景由 managed-service 层代持，而不是把凭据暴露给 bed 内任意代码。

环境变量同理（借鉴 execd strict profile 的黑名单）：hostel 进程自身 env 里密钥形变量（`*_API_KEY` / `*_TOKEN` / `*_SECRET` / `*_PASSWORD` / `AWS_*` / `K8S_*` / `KUBE_*`）经 `--unsetenv` 剥除后才进 bed。文件遮蔽挡"挂载进来的凭据"，env 剥除挡"进程继承的凭据"，两条泄漏通道都要关。

### 5. 降级行为（与 v1 一致的哲学）

- 非 Linux / 无 bwrap：可退化 direct（chdir only），跨 bed 访问屏障**不存在**，healthz/capabilities 如实上报——但客户端路径仍统一映射到所选 bed 的 `bed_home`，不能随隔离档一起降掉。
- bwrap 存在但版本不支持某 flag：启动 bed 进程失败即报错，**不静默降级**（数据隔离是被明确请求的，降级等于欺骗调用方）。

### 6. 测试策略

- **mac/CI 可跑**：argv 构造单测——给定 root/bedID，断言遮蔽序列、bind 目标、顺序敏感项、env 剥除（argv 构造放在无 build tag 的 `bwrap_args.go`，exec 侧才是 `bwrap_linux.go`）。
- **三档共同契约**：对 dorm/room/suite 跑同一组路径表，断言 `/workspace/a`、`/tmp/workspace/a` 和相对路径都落到对应 `bed_home`，并覆盖 `..` 与 symlink 逃逸；隔离等级只改变跨 bed 访问结果，不改变映射结果。
- **Linux 真验证**（devbox）：起两个 bed，A 写文件，断言 B 内 `ls <workspace-root>` 看不到 A 的目录、`cat` A 的宿主路径报不存在；`/workspace` 内读写互通 file API。
- 回归：direct 模式行为不变（现有 web/bed 测试全绿）。

## 非目标（明确推后）

- per-bed cgroup（资源消耗隔离）——见 `resource-isolation.md`；
- seccomp / 真 setuid / userns（安全纵深）——单独设计；
- overlay CoW 临时层——与持久化（`persistence.md`）合并考虑；
- 跨 pod 实时共享 workspace——见 `persistence.md` 的诚实边界。

## 实现状态

已实现（`internal/isolation/`）：boot 时 bwrap probe（binary + **全形态 smoke**——用真实 argv 起 `true`，namespace/遮蔽/`/workspace` bind 全过一遍；宿主挂载点缺失等问题在 boot 即暴露并诚实降 direct，不再误报 `workspace_mount`）、遮蔽 argv、`/workspace` 规范挂载、cwd 模式感知映射（`web` 层 `resolveCwd`）、env 剥除、capabilities/healthz 报 `workspace_mount`。mac argv 单测绿；**Linux 真机双 bed 验证已通过**（devbox，bwrap 0.8.0 / kernel 5.15：兄弟遮蔽、规范挂载、敏感路径+env 剥除、direct 负面对照全 PASS）。真机验证同时暴露两个 bug 均已修复：宿主缺 `/workspace` 挂载点（probe 改全形态 + boot 时确保挂载点）；shell 死亡 + 未断开客户端导致全 daemon 死锁（Shell 锁职责拆分，见 `internal/bed/shell.go` LOCKING 注释）。

**尚未完全兑现的共同路径契约**：当前 `fsops.Paths.FromClient` 仍只接受相对路径和 `/workspace` 前缀，`/tmp/workspace/...` 会被判为 workspace 外；direct/room 也没有为命令内绝对路径提供 bed-local 投影，daemon 文件操作的 symlink 防逃逸同样需要补齐。三档隔离机制虽已实装，但这部分不能算某一房型的能力差异，后续应在三档共用路径层补齐。

## 隔离分档模型：青年旅社房型（档 / 机制 / 上限 / 请求）

**bed 与房型是正交的两件事**，别混：**bed = 客人占用的单元**（一个 sandbox：workspace + 常驻 shell + 会话状态），跨档不变、是稳定的基本单位；**房型（dorm/room/suite）= 这张床所在房间的隔私度**，是"床周围的墙"有多严，不是替代 bed 的另一个名词。一张 dorm 床 / 单间床 / 套房床都还是 bed，区别只在墙。对应关系：dorm = 多个 bed 同室无墙（共享进程、bed 间无隔离）；room = bed 独占单间（数据锁死、厕所公用）；suite = bed 独占套房（私有 mount 视图）。

**威胁模型**：一个 bed 逃出自己的空间、去读写别的 bed 的数据 = **越狱 / 串门**（jailbreak / escape）。数据隔离就是按强弱分档地防串门。档是对外保证，实现机制是内部细节。四个正交概念：

- **Level（档）** = 对外保证：一个 bed 能对兄弟 / 宿主数据做什么。配置与上报用它。
- **Mechanism（机制）** = 实现：direct / bwrap / landlock / uid，hostel 按环境自己选，调用方不关心。
- **Ceiling（上限）** = 环境（内核版本 + capability）决定本机**最高能到哪档**。
- **Request（请求）** = 用户要的档，可以 ≤ 上限——**故意降档合法**（觉得顶格没必要）。

解析规则：

```
effective = min(requested, ceiling)
请求 > 上限 → 诚实降级 + 警告日志（不假装隔离）
请求 < 上限 → 尊重（按需降档）
```

### 三档 = 三种房型

档名借青年旅社房型，梯度即隔私度，配置零学习成本（真实订房就是 dorm / room / suite）：

| Level | 房型 | 保证：bed 对兄弟 / 宿主数据 | 机制 | 环境门槛 |
|---|---|---|---|---|
| **`dorm`** | 上下铺 / 通铺 | **无私人空间**：床位名义是你的但无屏障，伸手够到隔壁铺（仅组织性分隔，见 §5） | direct（chdir only） | 无 |
| **`room`** | 单间（可锁门，**厕所公用**） | 别人**进不了你的房间**（数据 open EACCES），但走廊看得见你的门牌（存在可见），且 `/tmp`、`/usr`、系统路径等**公共设施仍共享** | landlock（优先）/ per-bed uid | 内核 ≥5.13 / `CAP_SETUID` |
| **`suite`** | 套房（**全私有**） | 别人**看不见你的单元** + 私有 mount 视图（自己的 `/tmp`）+ `/workspace` 规范挂载 + env 剥除 | bwrap（mount ns） | userns 或 `CAP_SYS_ADMIN` |

无论住哪种房型，前台都先按 bed header 把客人的路径送到同一个 `bed_home`；表中的高低只描述兄弟 bed 能否看见、进入或读写该位置。路径映射不是 Level，也不参与 `effective = min(requested, ceiling)` 的降级。

房型隐喻的技术精度：
- **room = 单间厕所公用**：landlock 锁死「你的数据」，但不给私有视图——host 的 `/tmp` / 系统路径 / 父目录（兄弟门牌）仍共享可见。锁的是房间，不是整层楼。
- **suite = 套房全私有**：bwrap 给私有 mount 视图——自己的 `/tmp`（tmpfs）、`/workspace` 规范挂载、兄弟从视图消失。连厕所都是自己的。
- `room` 一档两机制：landlock（内核 LSM，无 cap）与 per-bed uid（权限位，需 `CAP_SETUID`）。**level 是地板不是精确刻画**：两机制兑现同一条下限（跨 bed 不可读写），但真实强度不齐一——uid 多送进程边界（kill/ptrace/procfs 跨 uid 拒绝），也多两条部署前提（subuid 段不碰撞、`protected_hardlinks=1`，见〈uid 档的诚实边界〉）；landlock 纯 FS、零特权。调用方不应假设"room 就是某一种墙"，实际选中的机制经 healthz `isolation.mechanism` 披露。优先 landlock。

很多现代内核默认 `kernel.unprivileged_userns_clone=1`，此时 bwrap **无需 root 也能到 suite**（devbox 特权容器已验，普通容器开 userns 同样成立）——"没 CAP_SYS_ADMIN"不等于"住不了套房"，先看内核这个开关。

### 配置与上报

- `--isolation dorm | room | suite | auto`，**默认 `auto`**（顶格取 ceiling）。取值是**房型（档）**，不是机制名（`direct/bwrap` 旧值迁移，hostel 无真实用户、零成本）。
- 机制不进配置词汇；真需要强制才加 `--isolation-mechanism`（少用）。
- capabilities / healthz 报四元组：`requested / effective / ceiling / mechanism`，调用方一目了然。

### room 档实现（landlock，调研结论）

参考本地 clone `../go-landlock`（官方库）与 `../greywall`（同类现成品：container-free、deny-by-default、面向 AI coding agent 的 landlock 沙箱）：

- **`github.com/landlock-lsm/go-landlock`**，核心 API 一行——
  `landlock.V9.BestEffort().RestrictPaths(landlock.RODirs("/usr","/bin",...), landlock.RWDirs(bedDataDir))`。调用后**本进程（含 exec 出的子进程）只能访问列出的路径**。
- **无需任何 capability**，Landlock 自 Linux 5.13 起（FS 访问控制是 ABI v1，最广部署那档；网络限制要 ABI v4 / 内核 6.7，暂不需要）。`BestEffort()` 老内核 / 无 landlock 时**优雅降级**——与 hostel 诚实降级哲学一致。
- **集成模式已明确**（greywall 同款、与 bwrap 外部前缀同构）：landlock 只能限制**调用它的进程**，不能限制 hostel 自己（hostel 要管所有 bed、需全 FS）。做法是 **hostel 自 re-exec**：隐藏子命令 `hostel __confine <bedDataDir> -- <shell>`，子进程先 `RestrictPaths` 再 `exec` bash。这**正好塞进现有 `Isolator` 接口**——room 机制的 `Wrap` 把 `<hostelBin> __confine <bedDir> --` 前缀进 `cmd.Args`，和 bwrap 前缀 argv 一模一样。
- **系统路径放行清单**（shell 要能跑，参考 greywall）：RO = `/usr /lib /lib64 /bin /sbin /etc /proc /sys /run /opt`；RW = 该 bed 的 `data/`（+ `/dev/null` 等）。

### room 相对 suite 的诚实差距（就是"厕所公用"）

- **"进不去" ≠ "看不见"**：room（landlock）让兄弟 bed 目录 open EACCES，但 `readdir` 父目录仍能看到名字（存在性泄漏）；suite（bwrap tmpfs）让它们从视图消失。数据机密性两者都挡读，"有没有 bed X"这类元数据 room 会漏。
- **公共设施仍共享**：`/tmp`、系统路径是宿主共用的，无私有 mount 视图；room 下不自动获得 suite 的 `/workspace` 真实挂载，但 file API、cwd 等结构化路径仍必须遵守三档共同的 `bed_home` 映射契约。
- **仅 FS**（+ 高版本部分 net/ipc）：不带 pid/uts/ipc/net namespace——那些是安全纵深，不是数据隔离。
- 必须在把控制权交给不可信代码**前** apply，且不能限制已打开的 fd。

### room 档第二实现（uid，Unix DAC）

landlock 依赖内核编译了 `CONFIG_SECURITY_LANDLOCK`，我们两个真实环境都不满足（devbox bsk 5.15、test 集群 stock 5.4 均无）。uid 机制补上这个格子：**不需要任何特殊内核，只借最古老的 Unix DAC**，把 room 保证换一种方式兑现。

原理（锚点 `uid_linux.go`）：

- **每个 bed 一个专属 uid**，其 `data/` 目录 `0700` 且 `chown` 给该 uid。bed B（uid_B）穿越 `{root}/bedA/data` 时内核在这一级判 EACCES——判定发生在**目录穿越**，与里面文件权限无关。`{root}/bedA` 本身 `0755`，所以兄弟**门牌可见、房间进不去**，正是 room 语义。
- **uid 派生自 data 目录路径**（`bedUID` = 高位段 hash，无注册表）：`Prepare`（chown）与 `Wrap`（setuid）各自独立算出同一个 uid，无共享状态、跨重启稳定。两个 bed 撞同一 uid 可能但罕见，撞了只是这一对退化成互相可读（dorm），不会崩——已在注释和本节记录。
- **比 landlock 多送一层进程边界**：uid 不只挡文件，跨 uid 的 `kill`/`ptrace`/读 `/proc/<pid>/environ` 内核一并拒绝——兄弟 bed 的进程杀不掉、内存偷不到、环境变量（常含密钥）读不到。landlock 只管 FS。

三个必须处理的工程点：

1. **setuid 后门**：镜像里的 setuid-root 程序（`su`/`mount`/`passwd`…test 镜像实测就有 9 个）会让降权后的 bed 一执行就升回 root。`__asuser` re-exec 里在 `setuid` 前置 `PR_SET_NO_NEW_PRIVS`——设了之后 setuid 位失效，后门一次性封死（真机已验 bed 进程 `NoNewPrivs:1`）。
2. **降权顺序**：`setgroups(空) → setgid → no_new_privs → setuid → chdir`，每个特权步骤必须在丢掉对应能力**之前**跑（丢了 uid 就没权限再改 gid/组）。Go 的 `SysProcAttr.Credential` 会做对这套舞蹈，但我们要内联 NNP，故走 re-exec 自己实现（与 landlock `__confine` 同构）。
3. **daemon 侧读回**：`Prepare` 把 data 目录整棵 chown 给 bed uid。daemon 若是 root，跨 bed 的 file API / persist 读取不受 `0700` 影响；若 daemon 非 root（setcap 形态），额外需要 `CAP_DAC_READ_SEARCH`——见下方能力矩阵。

**uid 档的诚实边界**（都是 room 通性或部署假设，不是 bug，但要说清）：

- **env 不剥离**：bed 继承 daemon 的全部环境变量（room 一档本就不承诺 env 剥离，只 suite/bwrap 承诺）——所以 daemon 侧密钥别放 env，或需要 env 私密时上 suite。`__asuser` 只补了 `HOME`/`USER`，没删任何东西。
- **uid 段是假设不是保证**：`[200000,300000)` 可能与 `/etc/subuid` 的 userns 映射（第二个默认用户起 231072）或 LDAP/服务账号重叠。撞上真实身份 → bed 能碰那个身份的文件。当前威胁模型（bed 误串门，非对抗性 uid 抢占）下可接受，属部署需核对项。
- **依赖 `fs.protected_hardlinks=1`**：`chownTree` 已跳过 `Nlink>1` 的普通文件（防 bed 硬链接宿主文件后被 `Prepare` chown 走属主提权），但纵深上仍建议部署侧保持内核默认的 `protected_hardlinks=1`——尤其 uid 档正是面向可能关掉它的老/定制内核。

**workspace 单一属主不变式（已落地）**："写进 bed workspace 的一切归 bed"——workspace 曾有两个写入者（shell 以 bed uid、file API 以 daemon 身份），daemon 落盘的文件 bed 能读却改不了，属主分裂是这类 edge case 的共同根。现在 fsops 构造时读 workspace 目录属主，新建的文件/目录按属主 chown 归位（`fsops.chownNew`，best-effort：无 CAP_CHOWN 时退回旧行为，下次 `Prepare` 兜底）；沿用 `chownTree` 同款硬链接纪律（`Nlink>1` 不 rehome，防属主提权）。

环境能力 → 机制矩阵（`New` 的探测顺序 bwrap → landlock → uid → direct）：

| 环境给了什么 | 能到的档 / 机制 | daemon 全程特权？ |
|---|---|---|
| userns / `CAP_SYS_ADMIN` | suite / bwrap | 否（userns 免特权） |
| 内核有 Landlock（≥5.13 且编译） | room / landlock | 否（进程自缚，零 cap） |
| `CAP_SETUID+SETGID+CHOWN`（root 或 setcap 二进制） | room / uid | 是（非 root 形态另需 `CAP_DAC_READ_SEARCH` 做读回） |
| 什么都没有 | dorm（fsops API 层 confine 仍在） | —— |

注意 **landlock 是唯一"daemon 全程零特权"就能立墙的机制**；uid 的定位是"内核太老没 landlock、但能拿到 setuid 能力"这个格子的填充，不是无特权方案。

**宿主事实集中采集（`HostFacts`，`hostfacts.go`）**：kernel / landlock-ABI / caps / bwrap / userns / cgroup 在 `New` 里**一次探测**、传给每个机制读（省去各读一遍 `/proc`），并挂进 `/healthz` 的 `isolation.host`——运维不进 pod 就能看清"这台宿主为什么顶到这一档"（没 bwrap → suite 不可用、`landlock_abi:0` → room 只能走 uid）。关键纪律：**事实只是快速预检、不是判决**——能 rule out（没 ABI 就跳过），但 rule in 仍靠各机制自己的 boot smoke（声明的能力 ≠ 真在执行：landlock ABI 可能 BestEffort no-op、caps 在也可能被 seccomp 挡掉 setuid）。

### 实现状态

**三档全部实装，room 双机制**（`internal/isolation/`）：
- 房型路由 `New(requested, root)`：`effective = 请求 ≤ 内最高可达档`；每个机制 boot 时探可用性，`unavailable` 标记保留 Level 以便算 ceiling；解析结果日志 + capabilities/healthz 报 `isolation.{level,mechanism,requested,effective,ceiling}` + `workspace_mount`。
- **room = landlock**（`landlock_linux.go`）：boot 探测两级——`LandlockGetABIVersion()≥1` 之外再跑**全形态 smoke**（bwrap 同款哲学：ABI 在场 ≠ 真在执行。用真实 `__confine` 形态确认自己目录可写、假兄弟目录不可读，抓 `BestEffort` 静默 no-op 和 workspace-root 落在 `/tmp` 等共享 RW 路径下的假隔离，探不过诚实降级）；机制 `Wrap` 前缀 `hostel __confine <bedData> --`，`main` 的 `__confine` 子命令 `landlock.V9.BestEffort().RestrictPaths(RODirs 系统路径, RWDirs bedData+/tmp+/dev)` 后 `syscall.Exec`——与 bwrap 外部前缀同构，参考 `../greywall`。
- 解析规则纯逻辑单测（`resolve_test.go`，注入可用性矩阵，mac 可跑）；**landlock 真机隔离验证待 landlock-enabled 环境**——devbox 已排除：bsk 定制内核（5.15.120.bsk.3）未编译 `CONFIG_SECURITY_LANDLOCK`，内核版本 ≥5.13 不是充分条件，容器共享宿主内核也绕不过；换 stock 内核（Debian 12 / Ubuntu 22.04+，`/sys/kernel/security/lsm` 含 `landlock`）即可验。devbox 上已真机验证的部分：请求 room 时的诚实降级上报（requested/effective/ceiling 四元组）与 dorm 负面对照。
- **room = uid**（`uid_linux.go`）：boot 探测两级——`CapEff` 读 `/proc/self/status` 确认 `CAP_SETUID/SETGID/CHOWN`（快速失败），再跑**全形态 smoke**（landlock 同款哲学：cap 在场 ≠ setuid 真能用。准备两个不同 uid 的兄弟目录、以其一 `__asuser` 跑，验证自己目录可写、兄弟 secret EACCES，抓被 seccomp 静默挡掉的 setuid）；`Wrap` 前缀 `hostel __asuser <uid> <bedData> --`，`main` 的 `__asuser` 子命令降权 + NNP 后 `syscall.Exec`；`Prepare`（`isolation.Preparer` 可选接口，bed manager 在 Resolve 里调）chown data 目录。**test 集群真机验证已通过**（a-test sandbox pod，root / 内核 5.4 无 landlock：请求 room → 选中 uid 机制，两 bed 不同 uid、跨 bed 数据 EACCES、门牌可见、`NoNewPrivs:1` 封 setuid 后门，全 PASS）。
- 纯逻辑单测（`uid_linux_test.go`：uid 派生确定性/范围、cap 解析、chown——非 root 部分 skip）；机制的完整 re-exec 验证同 landlock，只在真机 boot smoke 与 pod 里跑。

依赖：`github.com/landlock-lsm/go-landlock`（landlock，仅 linux）、`golang.org/x/sys/unix`（uid 的 `PR_SET_NO_NEW_PRIVS`）；非 linux 走各自 `*_other.go` 报 room unavailable。

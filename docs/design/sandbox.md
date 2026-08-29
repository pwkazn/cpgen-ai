# DockerSandbox 详细设计

## 1. 目标与边界

`DockerSandbox` 只完成两件事：按固定工具链编译源码，以及在受限 Linux 容器中运行一个已构建程序。它不解释 WA/PE，不接收自然语言命令，也不允许 Agent 控制 Docker 参数。

MVP 连接本机 Docker Engine：Windows 使用 Docker Desktop/WSL2，Linux 使用 Docker Engine。不存在 host process fallback。`cpgen sandbox-watchdog` 是同一 Go 二进制的脱离式安全子进程，不执行题目内容，只在 owner 消失后按持久化 `watchdog_safety_deadline` 收敛已标记 Docker 资源；它不是调度服务或本地进程执行后端。

## 2. 启动能力检查

Runner 启动时只做不创建容器的静态检查（Engine ping/version/OS、镜像 digest、显式配置）。首次 run 需要某 profile、缓存 TTL/Engine identity 失效或 BLOCKED resume 时，由该 run 的 `MeteredDependencyProber` 执行版本化 canary 并缓存下面的 CapabilitySnapshot：

```text
engine_reachable, server_os=linux, api_version
engine_identity_digest, endpoint_digest, daemon_id, engine_instance_marker
cgroup_version, cpu_quota, memory_limit, pids_limit
read_only_rootfs, tmpfs, seccomp, no_new_privileges
named_volume, local_tmpfs_volume_quota, volume_keeper
log_none_with_attach, detached_watchdog, watchdog_control_acl, swap_max_zero
builder_image_digest, runtime_image_digest, transfer_image_digest
execution_protocol_version=docker-direct-v2
```

- `compile` profile 要求 Linux container、只读根、tmpfs、PIDs/内存/CPU 限制。
- `mvp execute` 额外要求 local-driver quota tmpfs volume 跨 target stop、脱离式 watchdog及 control-record ACL、`none + attach`、`memory.swap.max=0`、主进程/子进程 OOM 和输出限制 canary 全部通过。
- `release execute` 只允许 allowlist 内本地 rootful Linux Engine，并要求 Runner 可预建/读取/删除专用 cgroup v2 父 subtree；Docker Desktop、remote context 和未验证 rootless Engine 不提供 release capability。
- 能力不满足所选 profile 时返回分类后的 `INFRA_ERROR(capability_missing)`，任务进入 `BLOCKED`。
- 正式 canary 的每个 ContainerCreate 都有 run-scoped AttemptCall/预算/SandboxExecution/watchdog；不存在“应用启动时无父调用创建容器”的旁路。Slice 0 仅用测试专用 harness。`cpgen doctor` 默认只报告静态能力与最近 canary 的非权威摘要，不把它写成可推进 run 的 observation。
- endpoint 只能来自已保存的有效配置，禁止继承 `DOCKER_HOST/DOCKER_CONTEXT`。`engine_identity_digest` 规范包含 endpoint、daemon ID、Engine instance/boot marker、server/API/OS/arch、runtime/cgroup/security options 和 builder/runtime/transfer image digests；capability cache、每次 dispatch、watchdog 与 cleanup recovery 都必须匹配该 digest，Engine 重启/切换或配置漂移后旧 snapshot 不可用。

## 3. 镜像

### cpgen-builder

按 digest 固定，包含：

- 固定版本 C++20 编译器及标准库。
- 固定 Go toolchain。
- 固定 digest 的 `testlib.h`。
- 非 root 用户和空默认 entrypoint。

### cpgen-runtime

MVP 可与 builder 使用同一基础发行版，但不暴露编译器命令。后续拆分精简 runtime 镜像时必须改变 profile digest 和缓存键。

镜像不能使用浮动 tag 参与质量门禁。

### cpgen-transfer

按 digest 固定的可信 import/export helper。它不运行模型生成程序：import 只把长度前缀字节流写入 Engine volume；export 只读挂载输出卷，使用 `openat2/openat + NOFOLLOW` 校验预声明普通文件并将字节流送回 Runner。helper 禁网、只读根、资源受限；只有初始化路径可拥有最小 `CHOWN/FOWNER`，target 始终 `CapDrop=ALL`。

## 4. 请求契约

```go
type DispatchAuthorization interface {
	CallID() AttemptCallID
	RunID() RunID
	AttemptID() AttemptID
	OwnerID() OwnerID
	LeaseEpoch() int64
	ScopeDigest() Digest
	sealDispatchAuthorization() // owning port package 的私有方法
}

type SandboxDispatchAuthorization interface {
	LogicalOperationID() string
	RunID() RunID
	AttemptID() AttemptID
	OwnerID() OwnerID
	LeaseEpoch() int64
	ScopeDigest() Digest
	PlanDigest() Digest
	ContainerPlan() ContainerPlan
	ClaimEnginePing(ctx context.Context) (DispatchAuthorization, error)
	ClaimNextContainer(ctx context.Context, role ContainerRole) (ContainerDispatchGrant, error)
	sealSandboxDispatchAuthorization() // owning port package 的私有方法
}

type ContainerDispatchGrant struct {
	Role ContainerRole // IMPORT | KEEPER | TARGET | EXPORT
	Auth DispatchAuthorization
}

type CompileRequest struct {
	Language       Language       // CPP20 | GO
	Role           ProgramRole    // SOLUTION | BRUTE | GENERATOR | VALIDATOR | CHECKER
	SourceBundle   SourceBundleManifest
	Toolchain      ToolchainID
	Limits         CompileLimits
	ExpectedOutput LogicalName
}

type RunRequest struct {
	Role      ProgramRole
	Program   BlobRef
	Args      RoleArgs
	Stdin     *BlobRef
	Files     []InputMount
	Outputs   []OutputDeclaration
	Limits    RunLimits
	Seed      *uint64
}

type SourceBundleManifest struct {
	Files      []SourceFile
	EntryPoint SafeRelPath
	Digest     Digest
}

type SourceFile struct {
	Path SafeRelPath
	Blob BlobRef
}

type PendingArtifact struct {
	Blob        BlobRef
	MediaType   string
	Role        ArtifactRole
	LogicalPath SafeRelPath
	CallID      AttemptCallID
	ReservationID ReservationID
	WriterTokenID ArtifactWriterTokenID
	PinID       BlobPinID
	PhysicalNewBytes int64
	Provenance  ProvenanceCandidate
}

type ContainerPlan struct {
	PlanDigest           Digest
	EngineIdentityDigest Digest
	Resources            []PlannedResource // container/volume/cgroup 的完整不可扩张计划
	TransferBytesMax     int64
}

type PlannedResource struct {
	Ordinal             int
	Kind                ResourceKind // CONTAINER | VOLUME | CGROUP
	Role                ResourceRole // IMPORT | KEEPER | TARGET | EXPORT | INPUT | OUTPUT | RELEASE_PARENT
	DeterministicName   string
	ExpectedLabelsDigest Digest // Docker resource；CGROUP 使用 kind-specific identity digest
	CreateCallOrdinal   *int // 由 logical operation + ordinal 映射到已授权 AttemptCall
	CgroupRelativePath  *SafeRelPath
	CreationNonce       *string
}

type ResourceKind string // CONTAINER | VOLUME | CGROUP
type ResourceRole string

type SandboxExecution struct {
	LogicalOperationID string
	ResourcePlanDigest Digest
	EngineIdentityDigest Digest
	ResultCallID    *AttemptCallID
	Resources       []SandboxResourceRef
	WatchdogToken   string
	Phase            SandboxPhase // CREATED|ARMED|TARGET_STARTED|STOPPING|TARGET_STOPPED|EXPORTING|ALL_STOPPED|CLEANUP_PENDING|CLEANED
	ProgramDeadlineUTC time.Time
	WatchdogSafetyDeadlineUTC time.Time
	WatchdogArmedDuration time.Duration
	TriggerCause     *ExecutionCause
}

type DockerProbeResult struct {
	Capabilities CapabilitySnapshot
	CallTrace    CallTrace
}
```

低层 `DockerSandbox` 使用 `Compile/Run(ctx, SandboxDispatchAuthorization, request)` 和 `Probe(...) -> DockerProbeResult`；`Probe` 只由 `MeteredDependencyProber` 调用。Probe policy 明确选择 Engine ping 或固定 canary 容器：前者的 authorization 恰含一个 `DOCKER_ENGINE_PING` AttemptCall 和空 ContainerPlan，只能调用一次 `ClaimEnginePing` 且不扣 `max_sandbox_runs`；后者不含 ping row，每个 canary `ContainerCreate` 都使用独立 grant、计量和清理协议。两个 claim 路径互斥，错类型或重复 claim 均在 Engine 调用前失败。

Step 只构造不含执行身份的 `CompileRequest/RunRequest`。`MeteredSandbox` 先生成稳定的 logical operation ID 和完整 `ContainerPlan`（包含所有 container/volume/cgroup 的 ordinal、kind、role、确定性 name 或受限 cgroup 相对路径、labels/identity digest 以及对应的 CreateCallOrdinal），计算不可变 `PlanDigest`，再在一个守卫事务中为 import/keeper/target/export 的每次计划 `ContainerCreate` 分别创建 `AUTHORIZED` AttemptCall 并按 logical operation + ordinal 建立映射，预留全部 sandbox-run/artifact 预算；任一额度不足则在创建 Docker 资源前整体失败。Engine ping probe 以同一守卫事务创建唯一 ping row，不创建 sandbox-run reservation。owning port package 的未导出 concrete type 把固定身份、`PlanDigest` 和 `EngineIdentityDigest` 封装为 `SandboxDispatchAuthorization`，再调用低层 `DockerSandbox.Compile/Run/Probe`。聚合 capability 的只读 run/attempt/owner/epoch/scope 用于 volume/cgroup/SandboxExecution 标签和守卫，不授权 ContainerCreate；计划 getter 必须返回不可变值或深拷贝，Runner 重算并核对 `PlanDigest`/`EngineIdentityDigest`。Go 包外代码无法实现这两个带私有方法的接口，Agent/模型也拿不到构造器。

`ClaimNextContainer(role)` 只能按固定 plan 顺序把对应 AUTHORIZED row 经 fencing/CANCEL/scope CAS 唯一推进到 DISPATCHING，并返回其 sealed physical grant；低层 Runner 必须在每次 `ContainerCreate` 紧前调用，不能扩张计划。每个 grant 的 call/run/attempt/owner/epoch/scope、container role 和 logical operation ID 必须与数据库一致；缺少、重复、错序或多余 role 都在 Create 前失败。`CompileResult/RunResult` 返回公共 `CallTrace`：`PhysicalAttemptCallIDs` 包含所有实际 dispatch；正常执行时 `ResultAttemptCallID` 指向 target，若 import/keeper/export 的失败直接决定本次结果则指向相应失败 call。Metered proxy 在返回前核对集合、顺序和 scope，禁止把迟到或其他操作的结果接错 attempt。未执行的预留 grant 必须在确认未 Create 后收敛为 `ABORTED_NO_DISPATCH`。固定 plan 内不做新的 ContainerCreate retry；可重试基础设施失败必须先收敛本 operation，再由 Policy 创建新的 logical operation/ContainerPlan/预算，历史 trace 保留。import/keeper/target/export 每次物理 ContainerCreate 都计入 `max_sandbox_runs`，不能把 helper 隐藏在 adapter 内重试。

ContainerPlan 由可信 profile builder 生成，ordinal 必须连续。普通 Compile/Run 恰有一个 TARGET；无声明文件输出时顺序为 IMPORT*→TARGET，有输出时为 IMPORT*→KEEPER→TARGET→EXPORT。EXPORT 只在 target STOPPED 后 Create/Start；计划可预授权但不能提前运行。Probe canary 使用独立的版本化 plan allowlist。模型不能提供或修改 plan。

`CreateCallOrdinal` 是计划摘要中的稳定映射键；授权事务创建 AttemptCall 后，将其 `(logical_operation_id, physical_ordinal)` 绑定到该 ordinal 并写入 resource row。数据库生成的 call ID 不参与授权前的 `PlanDigest`，避免出现“先有 call ID 才能算计划、先有计划才能建 call”的循环。

初始授权事务同时固定 ArtifactDeclaration→physical call 的 producer 映射：target call 绑定 stdout/stderr、执行记录和纯 stdout 结果；export call 绑定编译产物及 `/result` 声明文件；各 helper 诊断只绑定自身 call。所有 writer token/字节 reservation 在首个 Docker 资源前创建，不能等发现文件后新增。某 role 未 dispatch 或未产生声明输出时相应 token 转 RELEASED；任何 PendingArtifact 的 CallID 与映射不符都拒绝接入。

`max_sandbox_runs` 的计量单位按产品定义固定为实际 `ContainerCreate`；VolumeCreate/Remove、Attach、Start、Wait、Inspect、Stop/Kill 和 cgroup 文件操作不是新的不可信程序执行，不另扣该账户，但必须属于已授权 logical operation，逐项受 fencing/CANCEL、固定参数、阶段 timeout、SandboxExecution 资源记录和审计事件约束。它们不能借机创建 plan 外容器；若未来要对这些控制调用单独限额，应新增预算维度而非改变旧账户语义。

Slice 0 使用测试专用 `Slice0ProbeHarness` 注入内存 `ProbeDispatchLedger/ProbeArtifactSink/ProbeDispatchAuthorization`。它实现与生产相同的单次 dispatch CAS、scope guard 和固定硬上限，但只存在于 `internal/probe` 或测试包；正式 CLI 无构造器、无 fallback。Slice 1 用 SQLite guard 替换同一内部接口。

禁止字段：shell command、任意 Docker option、image、mount host path、environment map、capability、network 和 linker flags。

Runner 只从当前物理 grant 读取身份并生成固定标签 `cpgen.run_id/cpgen.attempt_id/cpgen.logical_operation_id/cpgen.call_id/cpgen.lease_epoch/cpgen.container_role`。每次 `ContainerCreate` 和 `ContainerStart` 紧前都重新检查对应 AttemptCall 的 dispatch claim、scope、当前 fencing 和 context；任一守卫失败时立即删除已创建容器。预算 reservation 只通过 FK 参与计量，不能单独授权创建/启动。恢复取得新 token 后持续监听 Docker events 并按标签停止旧 epoch 容器，直到旧 AttemptCall/reservation/pin 全部收敛；不是只做一次易受“扫描后迟到创建”影响的快照扫描。Resource Gate 在测量前后若观察到本项目旧 epoch 容器则丢弃本次 timing evidence。

`SourceBundleManifest` 不是 tar/zip。Runner 对每个输入、源码和可执行 Blob 调用 Artifact backend 的 `OpenVerified`，从同一已验句柄复制到固定只读 `/src`/`/input`；任何 size/digest 不匹配都在创建容器前失败关闭。从根源避免 archive 路径穿越、损坏 cache 和压缩炸弹。Bundle 校验：

- 文件数量、单文件和总大小受限。
- 路径满足 SafeRelPath，拒绝大小写或 Unicode 归一化冲突。
- EntryPoint 必须精确引用 files 中一个普通源码文件。
- Blob 只表示字节，不携带 symlink、权限或设备语义。
- Bundle digest 为 `schema_version + EntryPoint + 按 SafeRelPath 排序的 (path, Blob digest)` 的规范化、长度前缀组合哈希。
- Runner 必须自行重算 Bundle digest，不能信任调用方提供的 `Digest`；运行已编译 Blob 时使用固定只读挂载并在容器内赋予预定执行权限，不继承 Blob 的宿主文件 mode。

### 4.1 参数白名单

- solution/brute：无模型可控参数。
- generator：只允许 Schema 定义的数值/枚举参数和显式 seed。
- validator：固定 stdin 模式及 Runner 生成的 testlib flags。
- checker：固定 `<input> <output> <answer>` 路径和 Runner 生成的 testlib flags。
- Go/C++ 编译参数由 Toolchain manifest 生成；模型只能选择受支持语言/role。

## 5. 文件布局

容器内固定布局：

```text
/program/main               普通 Engine volume，只读，本次目标程序
/src/...                    普通 Engine volume，只读，SourceBundle
/input/...                  普通 Engine volume，只读，请求 Blob
/work                       容器 tmpfs，可丢弃 scratch，size limited
/result/files/...           quota tmpfs named volume，仅声明输出时挂载
/dev/shm                    显式小尺寸 Docker shm
```

不得挂载仓库、SQLite、artifact root、Docker socket、用户主目录、任意宿主 bind path 或密钥。编译输出和声明输出不能只写 `/work`：它们必须写入跨 target stop 存活的 `/result` transfer volume。

## 6. docker-direct-v2 执行协议

可信 Go Runner 位于目标容器之外，只通过 Docker Engine API 管理容器和 volume。运行容器没有内置 supervisor/init/sidecar：固定 OCI entrypoint 直接执行 `/program/main`，目标程序成为容器 PID 1，参数以数组传递且不经过 shell。编译容器直接执行 Toolchain manifest 指定的编译器。

### 6.1 Engine volume transfer

- 源码、输入和程序使用每 call 随机命名、带固定 labels 的普通 named volume。`cpgen-transfer import` 从 `OpenVerified` 的同一已验句柄读取长度前缀流，写为 root-owned 只读文件；Runner 不直接访问 Docker daemon 的 volume 宿主路径。
- 每个可写输出使用 local driver 创建 `type=tmpfs,device=tmpfs,o=size=<declared-total>,uid=<target>,nosuid,nodev,noexec` 的 named volume。可信 `volume-keeper` 在 target 前挂载并保持运行，所以 target 停止后 volume 仍存在；keeper 位于独立 cgroup，不计入题目资源。
- target 停止后，`cpgen-transfer export` 只读挂载输出卷，按第 10 节流式提升。PendingArtifact 持久化后才删除 target、keeper 与 volume。
- 输出 volume 的声明总量同时占用全局带权信号量；Desktop local-driver tmpfs、配额、keeper 跨 stop 任一 canary 失败时 execute profile 进入 `BLOCKED`，禁止退回无硬配额 writable volume。普通 named volume 仅可承载只读 payload。

### 6.2 启动、日志与 watchdog

target 固定 `LogConfig.Type=none`、`Tty=false`、`AutoRemove=false`、`RestartPolicy=no`。严格顺序为：

```text
persist SandboxExecution(CREATED) + complete PLANNED resource/name/label set
-> spawn detached sandbox-watchdog
-> watchdog subscribes events, performs baseline list-by-name/label, wait initial ACK
-> for each resource: CAS CREATING -> watchdog PRECREATE ACK -> Create
   -> persist returned ID/CREATED -> watchdog resource ACK
-> start/import readonly payload and start keeper (each after resource ACK)
-> ContainerAttach(target stdout/stderr)
-> subscribe OOM/die events
-> final fencing/CANCEL/scope check
-> arm authoritative program timer and watchdog target-phase deadline; wait ACK
-> ContainerStart
```

每个 target/helper 的 `ContainerCreate` 前消费其独立 grant；container/volume/cgroup 都使用 plan 派生的不可重用随机 name 与固定 labels。watchdog 在 Create 前已知道完整集合，Create 成功后 Runner 再用短事务补 ID 并取得资源 ACK，之后才允许相关容器 Start。发现同名但 label/engine identity 不匹配的既有资源时只报告冲突并失败关闭，绝不把它当成本 operation 资源删除。最后四步描述 target；helper 同样必须 `LogConfig=none`、restart=no、固定命令和资源上限。Create、pre-create/resource ACK 失败时不得 Start，进入 cleanup protocol。

Windows watchdog 使用命名管道，Linux 使用 Unix socket/`setsid`；它必须在父 CLI 被强杀后继续存活。watchdog 只接受随机 call token、持久化 container/volume IDs、固定 labels 和 `watchdog_safety_deadline`，不接受模型路径或 Docker 参数。token 原文保存在 owner-only ACL 的本机 control record，数据库只保存其 digest/ref，不能进入 artifact、日志或容器。

`watchdog_safety_deadline` 由当前 monotonic-to-UTC anchor、尚余 operation phase 上限、program hard limit、cleanup 上限和固定 safety slack 确定，必须不晚于当前 `step_deadline/run_budget_deadline + safety_slack`。target Start 前必须把 watchdog 切到 target phase 并取得 ACK；该 phase deadline 不得晚于 `start_boundary + program_hard_limit + stop/kill/wait 上限 + safety_slack`，从而 owner 在 ContainerStart 前后崩溃也不能让不可信程序越过安全包络无限运行。ACK 时 watchdog 用收到的剩余 duration 建立自己的 monotonic timer；数据库 UTC 值用于重启/janitor 对账，不用可回拨 wall clock 驱动活跃 watchdog。target STOPPED 后可在初始 operation envelope 内切换到 evidence/export phase；每次 phase 更新只能按同一快照公式收紧或推进已预留的 envelope，不能借 heartbeat 无限延长任务；TAKEOVER 也不得增加原 run/step 预算。

新 recovery owner 若取得更高 lease epoch，可用 token 请求 cleanup-only `TAKEOVER`；watchdog 以只读方式核对数据库当前 lease、`resource_plan_digest`、`engine_identity_digest`、sandbox operation 和 kind-specific identity 后才转移清理控制，并拒绝旧 control client，但绝不授权继续旧 export/Start。无法核对或无人接管时，控制管道断开即在 safety deadline Stop/Kill target 及 helper；输出可能丢失，recovery 将 attempt 记为 ABANDONED 后重跑，不能假装成功。

watchdog 在 safety deadline 先停止当前匹配资源，此后仍保持 Engine event subscription 和反复执行 kind-specific reconcile：Docker `CONTAINER/VOLUME` 使用 Engine identity + name/labels，release `CGROUP` 使用预告的受限相对路径、creation nonce、owner epoch，并在 `populated=0` 后执行 `cgroup.kill`/删除。只要任一计划 ContainerCreate/mkdir 为 DISPATCHING/SENT/UNKNOWN，或资源尚未 STOPPED/REMOVED，它就保持 armed，迟到 Create/mkdir 一出现即停止/回收。只有全部计划 call 已成为可证明终态、最终扫描无匹配运行容器且 SandboxExecution 进入 CLEANED，watchdog 才可退出；Engine 或 cgroup 文件系统暂时失联则保留 control record/tombstone 并重试。watchdog 不自行提交领域状态或删除仍有 pin/export 义务的 output volume；janitor/recovery 负责账务与允许的删除。

Owner 必须持续监控 watchdog 进程和 control channel。若 watchdog 在 `ARMED`、`TARGET_STARTED` 或 `CLEANUP_PENDING` 期间异常退出、EOF 或失去心跳，owner 立即禁止新的 Start/phase 推进并启动独立 cleanup；owner 随后也退出时由 startup janitor 取得 cleanup-only custody。该路径同样先核对两类 digest 与 kind-specific identity，不能把 watchdog 的异常退出当作 target 已停止。

target 的 hijacked Attach 明确选择 stdout/stderr，并仅在 role 声明 stdin 时启用 stdin；Start 成功后 Runner 从 `OpenVerified` 的同一受限输入句柄流式写入并关闭写半边，不通过 shell 重定向。输入字节上限在授权前固定，断流/短写按运行证据分类。Runner 持续读取并分别计量 stdout/stderr；读取 `limit+1` 即形成 OLE 证据并触发 Stop/Kill，清理期间继续 drain/discard，不能只截断或关闭 reader。禁止继承 daemon 的 `json-file`；若 Engine 不支持 `none + attach`，仅允许经磁盘增长 canary 验证的版本化 bounded `local(max-size,max-file=1)` profile。

### 6.3 资源与执行记录

target 设置 `Memory=RunLimits.MemoryBytes`、`MemorySwap=Memory`、PIDs/CPU 限制，不增加 headroom。目标及全部后代是该 cgroup 中唯一不可信进程树；containerd/runc/Runner/helper 位于其外。target 固定非 root、`CapDrop=ALL`、`no-new-privileges`、只读根、`network=none` 且不启用 init。

Runner 生成目标不可写的结构化执行记录：

```json
{
  "protocol": "docker-direct-v2",
  "started": true,
  "process_outcome": "EXITED",
  "exit_code": 0,
  "signal": null,
  "signal_evidence": "none",
  "wall_time_ms": 12,
  "cpu_time_ms": null,
  "peak_rss_bytes": null,
  "measurement_profile": "cgroup-v2-release",
  "stdout_bytes": 24,
  "stderr_bytes": 0,
  "stdout_truncated": false,
  "stderr_truncated": false,
  "oom_killed": false
}
```

- wall time 使用宿主 monotonic 的 authoritative program timer；`started=true` 只在 ContainerStart 明确成功后记录。Engine start timeout 与程序 timer 分离，start 结果未知不能判 TLE。
- Runner 在发出 ContainerStart 紧前记录 monotonic `start_boundary` 并预置 `start_boundary + RunLimits.Time` 定时器，使进程不会因 Start API 卡住而无限超额运行；但定时器先触发时只产生待对账的 `start_unknown` 并执行 cleanup。只有 Start 响应/Engine evidence 最终证明容器确已启动，才可把该触发归为 TLE；证明未启动则为 INFRA_ERROR，仍未知则保持 UNKNOWN/恢复对账。成功运行的 wall time 口径为 start_boundary 到 Wait，包含版本化的小量 Engine 边界开销。
- `mvp-v2` 要求 Start 前 OOM event subscription，MLE 仅由该 target 的 OOM event 或 Wait 后 `State.OOMKilled` 证明；`cpu_time_ms/peak_rss_bytes` 均可为空，live Stats 仅作 advisory。capability canary 必须验证容器内 `memory.max` 等于请求、`memory.swap.max=0`、PID 1 OOM、子进程 OOM 而 PID 1 存活/退出 0 均能被捕获；失败时不提供 mvp execute。
- `release-v2` 仅在合格 Linux host：Runner 先创建唯一父 cgroup，读取基线，并用 `CgroupParent` 只放入 target；helper 不在其中。wait 后从仍存在的父 cgroup 读取层级 `cpu.stat/memory.peak/memory.events`，确认 `cgroup.events populated=0`、保存证据后才删除父 cgroup。这样立即退出进程也不会丢指标。
- Engine wait/inspect/event/cgroup 证据由 Runner 序列化并通过 MeteredArtifactSink 保存。profile 要求的证据缺失、矛盾或归属不明为 `INFRA_ERROR(runtime_evidence)`；可空 advisory 指标缺失不算协议错误。
- Docker API 不保证对所有退出都提供原始 signal；只有 runtime/cgroup 提供权威信号证据时填写 `signal`。否则保留原始 exit code，role adapter 仍将非零结果判为 RE/工具错误，不伪造 signal。

## 7. ProcessOutcome 决定顺序

1. 任一 Orchestrator cause（`user_cancel/quiesce/revision_invalidated/lease_lost/step_deadline/run_budget_deadline`）已生效：用独立 cleanup context 收敛容器后返回 typed `ExecutionInterrupted(cause)`，不生成 ProcessOutcome。只有 user_cancel 可令 run CANCELLED；run_budget_deadline 进入 NEEDS_REVIEW。
2. ContainerCreate/Start 失败，或无法建立/控制目标容器：`INFRA_ERROR`。
3. ContainerStart 明确成功后 authoritative `program_hard_deadline` 到期并完成容器停止：`TLE`。`step_deadline/run_budget_deadline` 属于步骤 1，`watchdog_safety_deadline` 也不产生 TLE。
4. Docker `OOMKilled`，或目标 cgroup `memory.events` 的 `oom/oom_kill` 相对基线增加：`MLE`。
5. ContainerWait/inspect/cgroup 证据缺失或矛盾且不满足步骤 4：`INFRA_ERROR(runtime_evidence)`。
6. stdout/stderr 或声明输出超过限制：`OLE`。
7. 正常 wait 且由 signal 终止：`SIGNALED`。
8. 正常退出：`EXITED`，保留原始 exit code。

多个程序条件同时出现时按上面优先级保存主 outcome，其他证据放入 details。若 context cause 与 TLE/OOM/退出同时可见，先持久化底层证据供审计，但对调用方返回 `ExecutionInterrupted(cause)`；后续不得用迟到程序证据推进 current verdict。

每次执行固定 create、attach/watchdog-arm、engine-start、program、stop-grace、kill/wait、evidence/export 和 safety-slack 独立上限。`step_deadline` 必须覆盖完整 envelope；dispatch 前若剩余 run wall budget不足以容纳它则禁止 Start。release Resource Gate 串行执行并固定 cpuset/CPU quota，不与普通并行测试共享测量窗口。

## 8. 超时、kill 与回收

- Orchestrator root context 只触发停止；Stop/Kill/Wait/Remove 使用从 `context.Background()` 派生、固定上限的 `cleanupCtx`，不能复用已经取消的 context。
- portable 顺序为固定 `StopSignal=SIGTERM` 的 `ContainerStop`、极短 grace 后 `ContainerKill(SIGKILL)`、`ContainerWait(NotRunning)` 和 Inspect。安全停止证明可由以下任一独立、归属已核对的证据成立：Wait 成功；精确 container ID 的 Inspect 显示 `Running=false/Pid=0`；或已持久化 ID/确定性 name+labels 的 Inspect NotFound，且对应 Create call 已非 DISPATCHING/SENT/UNKNOWN、watchdog 最终扫描无匹配资源。历史 die event 只增强 verdict provenance，恢复时缺失它不能让 run 永久卡住。release 还必须验证父 cgroup `populated=0`，必要时使用 `cgroup.kill` 后重验。
- 任一 cause 触发后 run 进入 `RUNNING(mode=QUIESCING)`，禁止新物理调用。只有 target 确认 STOPPED 才可返回 ExecutionInterrupted；清理未完成返回独立 `CleanupPending` 控制结果，持久化 SandboxExecution，不能伪装为 ProcessOutcome INFRA_ERROR，也不能提交 CANCELLED/BLOCKED/READY。
- 新 lease owner 必须先收敛 CLEANUP_PENDING 再恢复工作流。已经 STOPPED 后的 helper/volume 删除可成为有期限 GC obligation，但未提升输出不得提前删除。
- recovery owner 的 TAKEOVER 只允许收敛旧 operation，禁止继续其 export/evidence Docker 动作或读取未持久化 output volume；全部容器停止后把未完成 operation 记为 ABANDONED并以新 logical operation 重跑。只有崩溃前已完整持久化的 PendingArtifact/执行证据可在不发 Docker 动作的情况下重放提交。
- Docker Engine 失联时 watchdog/janitor 持续重试，run 保持 QUIESCING；容器停止是释放 attempt/lease 和提交终态的前置条件。

## 9. 安全配置

- `network=none`。
- root filesystem read-only；完整 mount allowlist 只有 `/program,/src,/input` 只读 volume、`/work` scratch tmpfs、可选 `/result` quota transfer volume、显式小尺寸 `/dev/shm` 及 Docker 固定 pseudo mounts。所有 Engine volume mount 强制 `VolumeOptions.NoCopy=true`，避免镜像路径内容注入卷。Start 前必须从 Inspect 反查实际 mounts；拒绝镜像 `Config.Volumes`、缺失 NoCopy 和任何额外 writable mount。
- 目标程序直接使用镜像内固定非 root UID，丢弃全部 capabilities 并设置 `no-new-privileges`；容器内没有持有额外权限的可信控制进程。
- 默认 seccomp；宿主支持时应用项目 AppArmor profile。
- PIDs、CPU、memory、open files、file size、tmpfs、stdout/stderr 全部有限制。
- 环境变量使用固定 allowlist，如 `LANG=C.UTF-8`、`TZ=UTC`；不继承宿主环境。
- 固定 `WorkingDir=/work`，`TMPDIR/GOTMPDIR/GOCACHE/HOME` 指向受限 `/work` 子目录；Go 编译固定 `GOPROXY=off,CGO_ENABLED=0`。设置 core=0、nofile/fsize/nproc ulimit、PidsLimit 和明确 ShmSize。
- target/helper 固定 `LogConfig=none`（或已验证 bounded-local profile）、restart=no、AutoRemove=false；目标程序达到输出上限时必须被停止。
- 禁止 privileged、device、host PID/IPC/network namespace。

Docker 共享宿主内核。MVP 接受这一剩余风险，但 Runner 推荐运行在不含私人文件和生产密钥的专用 Docker VM/主机。

## 10. 输出提升

输出来自 keeper 持有、跨 target stop 存活的 quota tmpfs named volume；容器级 tmpfs 只作可丢弃 `/work`，不得承载编译产物或待提升结果。target STOPPED 后由可信 export helper 只读挂载 volume，只有 `OutputDeclaration` 中的相对路径可提升：

1. 路径必须为规范化安全相对路径，不能包含 `..`、空段、盘符或绝对前缀。
2. export helper 对每级路径使用 `openat2/openat + NOFOLLOW`，目标必须为普通文件且不跟随链接。
3. 最终对象必须是普通文件；拒绝 symlink、hardlink（link count != 1）、目录、device、FIFO 和 socket。
4. 限制文件数、单文件大小和总大小。
5. helper 通过同一已打开句柄输出长度前缀流，Runner 同时计算 SHA-256/字节数，避免检查后替换；禁止使用 tar/zip 或访问 volume daemon 宿主路径。
6. 通过 run-scoped `MeteredArtifactSink` 写入 Blob 临时文件，校验长度/hash 和 artifact-byte reservation 后原子提升。
7. `MeteredArtifactSink` 完成 token finalize 和 Blob 校验后返回包含 `CallID/ReservationID/WriterTokenID/PinID` 完整链的 `PendingArtifact`；Sandbox 只透传给 Orchestrator，不能直接调用 BlobStore 或写 ArtifactOccurrence。
8. PendingArtifact 已持久化并可恢复后才允许删除 keeper/volume；Orchestrator 在 attempt 完成或失败事务中建立 ArtifactOccurrence 后，制品才进入当前任务快照。

generator 未声明的文件全部丢弃。

## 11. 构建依赖策略

### C++

- 仅标准库和固定 `testlib.h`。
- include 路径由 Toolchain manifest 固定。
- 不接受 Agent 传入 `-I/-L/-l/-Xlinker` 等参数。
- 默认禁用动态下载、编译插件和 sanitizer（专用调试 profile 除外）。

### Go

- generator MVP 只允许标准库；如后续开放依赖，必须使用固定 vendor/module cache digest。
- `GONOSUMDB/GOPROXY` 不可用于联网，编译容器仍保持禁网。
- 默认 `CGO_ENABLED=0`，禁止 plugin 和模型可控 ldflags。

## 12. 测试

- Docker daemon 不可用、错误 OS、缺少 cgroup 能力。
- 非零、signal、TLE、MLE、OLE 及 context cancellation。
- 包外伪造的 DispatchAuthorization/SandboxDispatchAuthorization 无法通过私有 seal；缺失、错 run/attempt/scope/epoch、CallTrace 集合或 ResultAttemptCallID 不一致均在 ContainerCreate/结果接入前失败。
- 同一 Sandbox logical operation 的每个 import/keeper/target/export ContainerCreate 都有独立 AttemptCall/reservation/grant；缺少、多余、重复、错序 grant 以及 CallTrace 集合/ResultAttemptCallID 不一致均失败关闭。
- 六种 context cause 分别与 TLE/OOM/正常退出竞态，返回正确 ExecutionInterrupted 且不接入迟到 verdict。
- fork child 后超时，验证整个 cgroup 被清理。
- stdout/stderr 截断仍生成完整的宿主执行记录。
- symlink、hardlink、FIFO、路径穿越、超多文件和超大文件提升被拒绝。
- tar/zip 类型 SourceBundle 被拒绝；多文件只能使用 manifest + BlobRef。
- SourceBundle digest 固定向量覆盖 Schema version/EntryPoint/path/blob；只改 EntryPoint 必须改变 digest/cache key。
- 输入/可执行/源码 Blob 在容器创建前经 OpenVerified；同 size 篡改返回完整性错误且不启动容器。
- 模型试图注入 shell 字符或 linker 参数时请求 Schema 直接拒绝。
- 目标程序尝试写入伪造执行记录、访问 Runner fd/environment 或遗留子进程时，均不能影响宿主生成的执行记录；容器停止后所有后代必须被清理。
- lease 接管后按标签清理旧 epoch 容器；旧 owner 即使收到迟到结果也不能创建 ArtifactOccurrence 或提交 attempt。
- 旧 Runner 暂停在清理扫描与 Create/Start 之间再恢复时，fencing 守卫拒绝或 Docker event reconciler 立即停止迟到容器；其 timing 不进入 Resource Gate。
- 目标内存超过题目限制但低于旧版“限制 + headroom”的 fixture 必须由精确容器 memory limit 判为 MLE；无可信 Engine/cgroup 证据或证据矛盾时为 INFRA_ERROR。
- target 停止后编译产物/声明输出仍由 keeper 持有并能被 export helper 提升；强杀 CLI 后 detached watchdog 在 `watchdog_safety_deadline` 内停止 target，重启 janitor 可收敛残留资源。
- target/helper 的 Docker 日志驱动均为 none（或通过 bounded profile canary）；无限 stdout/stderr 在 limit+1 触发 OLE/停止且 daemon 数据目录不持续增长。
- mvp canary 逐项验证 `memory.max=请求值`、`memory.swap.max=0`、PID 1 OOM 与子进程 OOM；release canary 验证瞬时进程退出后父 cgroup 指标仍可读取且 helper 不在目标父 cgroup。

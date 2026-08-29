# 内部题包与导出设计

## 1. 原则

内部题包是唯一事实来源。Polygon 等格式由 Exporter 从内部包生成，不能反向成为领域模型。只有 Package Gate 通过后 run 才能进入 `READY`。

Package Agent 的唯一业务输入是 Orchestrator 从 current snapshot 构造的不可变 `VerifiedProblem`：它只引用同一组 ProblemSpec/Solution/TestPlan/工具/测试/答案 revision、Blob occurrence 和当前 PrePackage gate evidence。Package Agent 不从“最近文件”猜输入，也不能把历史/失效制品混入题包。

## 2. 目录

```text
problem-package/
├── manifest.json
├── statement/
│   ├── zh-CN.md
│   └── samples.json
├── solution/
│   ├── editorial.md
│   ├── reference.cpp
│   └── brute.cpp
├── judge/
│   ├── validator.cpp
│   ├── checker.cpp
│   └── generator/
├── tests/
│   ├── 001.in
│   ├── 001.ans
│   └── ...
├── reports/
│   ├── similarity.json
│   ├── prepackage-quality.json
│   └── provenance.json
└── export/
    └── polygon/
```

`reports/similarity.json` 必须符合 `cpgen.similarity-report/v1` package-safe DTO，只含受限 ID/标题/来源/规范化 URL/排名分数、决策 reason code 和本地 evidence digest；禁止第三方正文、snippet、`original/t0/t1`、raw response 或任意 provider metadata。

## 3. manifest

```json
{
  "schema_version": "cpgen.package/v1",
  "package_id": "...",
  "run_id": "...",
  "problem": {
    "slug": "sample-problem",
    "title": "...",
    "language": "zh-CN",
    "problem_spec_revision": 7
  },
  "limits": {"time_ms": 2000, "memory_mb": 256, "output_bytes": 67108864},
  "checker": {
    "kind": "token",
    "protocol": "testlib_v1",
    "artifact_path": "judge/checker.cpp"
  },
  "toolchain_manifest_digest": "sha256:...",
  "test_groups": [
    {"name": "main", "tests": ["001"], "coverage_tags": ["minimum"]}
  ],
  "files": [
    {"path": "tests/001.in", "sha256": "...", "size": 12, "role": "test_input"}
  ],
  "verification": {
    "profile": "mvp",
    "prepackage_report_path": "reports/prepackage-quality.json",
    "environment_digest": "sha256:..."
  },
  "provenance_path": "reports/provenance.json"
}
```

`files` 不包含 `manifest.json` 自身，避免递归哈希。`package_id` 为规范化 manifest（暂不含 package_id）与所有 file digest 的组合哈希。包内 `PrePackageQualityReport` 只覆盖原始生成/派生 run 的打包前门禁；Package Gate 的结果不写回被校验包。后续独立 verification run 生成自己的外部 PrePackage 证据，不以包内原始报告冒充当前验证。

## 4. 路径规则

- 使用 `/`、Unicode NFC 和 UTF-8。
- 必须是相对路径，禁止空段、`.`、`..`、反斜线、NUL、盘符和控制字符。
- 拒绝大小写折叠后冲突，确保 Windows/Linux 解包一致。
- 单路径、文件数、单文件和总包大小有限制。
- 题目 slug 只允许稳定 ASCII 子集 `[a-z0-9-]`。
- 包内只能包含普通文件和目录；不得包含链接、设备或其他特殊文件。

### 4.1 MeteredPackageWriter

Package builder 和 Exporter 都不能直接取得 staging 路径、宿主文件系统句柄或 Blob backend。Orchestrator 为一次 package attempt 创建唯一 `MeteredPackageWriter`，它只在已经解析并固定的 staging 根下工作，并负责：

- 只允许预先声明且通过 SafeRelPath/Unicode/大小写冲突检查的逻辑路径；Exporter 只能得到限定在 `export/<target>/` 且绑定 `ExportPlan` allowlist 的 scoped writer。
- 以不跟随链接、不可覆盖的安全打开方式创建普通文件；流式计算 digest/size，执行每文件、文件数和 `max_package_bytes` 总量上限。
- 从 Blob 复制时只接受 `OpenVerified` 返回的同一受控 reader；不暴露内容寻址存储路径，也不接受任意宿主路径。
- 维护最终 file-entry 集和 flush 状态；未关闭、超额、重复、未声明或越界写入使本次 package attempt 失败。

`max_package_bytes` 计量 staging 中包括 `manifest.json` 和所有 export 文件在内的实际普通文件字节，与 `max_artifact_bytes` 的 Blob 物理新增字节账户相互独立。

## 5. 构建协议

1. PrePackage Gates 全部 PASSED/有效 WAIVED。
2. 在 `output/.staging/<package-id-temp>` 创建 staging，并为该 package attempt 创建绑定 `max_package_bytes` 的 `MeteredPackageWriter`；其他组件不取得 staging 写能力。
3. 按 manifest 候选通过 `OpenVerified` 读取 Blob，再经 PackageWriter 写入普通文件并重算完整 digest；不从 Agent 工作目录直接复制，任何损坏 Blob 立即失败关闭。
4. 逐文件计算并核对 digest/size；每个 Exporter 先返回静态 `ExportPlan`，Orchestrator 校验路径、上限和冲突后才把对应 scoped PackageWriter 交给它。
5. Exporter 只经 scoped PackageWriter 写 `export/<target>`，随后由只读 `PackageReader` 执行目标格式验证；未声明输出或越界写入直接失败。
6. 非 manifest entry 全部关闭后，Orchestrator 先在无 CANCEL 的守卫事务中授权一个本地 artifact-write `AttemptCall`，为 manifest 预留 artifact bytes 并创建 `MeteredArtifactSink` writer token；随后才根据 PackageWriter 的最终 entries 生成规范化 manifest bytes，并经该 writer 保存为带 ACTIVE pin 的 PendingArtifact。再调用 `OpenVerified` 读取该 Pending Blob，把完全相同的字节经 PackageWriter 写成 `manifest.json`。manifest 不列入自身 `files`，但计入总包字节。
7. 关闭并 flush PackageWriter，对 staging 执行一次完整 Package Gate 预检；除常规检查外，staging 的 manifest digest 必须等于步骤 6 的 PendingArtifact digest。失败时不发布，并按 AttemptCall/FINALIZED writer token/artifact reservation/pin 完整链收敛。
8. 再次探测 active CANCEL。目标不存在时，先 flush/fsync 所有 staging 文件与 staging 目录，在同一 `output_root` 文件系统内原子 rename 到 `output/<slug>/<run-id>`，再 flush/fsync 最终目录及其父目录。目标已存在时禁止覆盖，保留 staging 待比对。
9. 对最终目录重新执行完整 Package Gate、从文件字节重算 package ID，并核对 current revisions/profile/PrePackage evidence 和 manifest PendingArtifact digest；既有目标还必须与 staging 候选 ID 相同。Gate 通过后，Orchestrator 在无 CANCEL 的守卫事务中授权新的本地 artifact-write `AttemptCall`，一次性预留 receipt/final report 的 artifact bytes 和两个 writer token，再通过这些 `MeteredArtifactSink` writer 生成绑定最终目录验证的 `PackageVerificationReceipt` 与最终综合 QualityReport；二者保存为包外、带 ACTIVE pin 的 PendingArtifact，未产生的声明输出必须释放。
10. 在单个数据库事务中检查 fencing、current revision 和“不存在 active CANCEL”，验证 manifest/receipt/最终 QualityReport PendingArtifact 的 AttemptCall、原始 artifact declaration(role/path/media type)、FINALIZED writer token、artifact reservation、ACTIVE pin 与 digest/size 完整绑定，再以 `writer_token_id` 幂等创建其 ArtifactOccurrence、结算 reservation、把 pin 转为 `RELEASABLE`，提交 package 内容身份和本 run 的 `VERIFIED` package occurrence，并把 run 的 `ready_package_occurrence_id` 与状态更新为 `READY`。任一环节崩溃均由 recovery 依据 token/pin 收敛，不能留下无保护的待提交 Blob。

目标目录已存在时不能只读取其 manifest 中自述的 package ID。只有目标目录本身通过步骤 9 的完整 Package Gate、重算 ID 与 staging 候选相同且当前证据绑定一致，才能在 READY 事务成功后清理本次 staging；任何缺失、篡改、额外文件或绑定差异都是冲突，保留 staging diagnostics，不认领也不覆盖目标。

`output/.staging` 与最终目录必须位于同一已解析文件系统/volume，禁止退化为 copy+delete。平台文件系统 adapter 启动时验证原子 rename 和 durable flush 能力；不满足 `release` profile 时任务 `BLOCKED`。`output_root` 权限只允许 cpgen 部署身份和明确运维修改，Package Gate 到 READY 提交之间若检测到 manifest/file identity 变化则失败关闭。

若进程在步骤 8 与步骤 10 之间崩溃，正式目录只是尚未被数据库认领的发布候选，run 仍非 `READY`。新 lease owner 必须从磁盘重跑步骤 9，并核对 package ID、run/revision/profile 和当前 PrePackage 证据；全部匹配后重新生成/验证 receipt，才能幂等补交步骤 10。冲突目录不得覆盖，须保留诊断并进入一致性失败。

若 CANCEL 在步骤 8 后、步骤 10 前先提交，READY 事务必须失败，目录保持未认领且不能作为成功输出；取消收敛只记录其 package ID/path 及已有 receipt/final report PendingArtifact 供 reconciliation，不能绕过 Package Gate 接入 run。

## 6. Package Gate

Package Gate 从受控磁盘目录重新打开题包，不使用构建过程的内存对象：正常 generation/derived run 使用 Package builder staging；幂等发布候选还必须对已存在最终目录独立重跑；外部 verification run 则只从导入后的已校验 Blob 重建私有只读 staging，绝不回读 observed source path。VERIFICATION 的内容门禁失败只产生 blocker/FAILED，不修改 staging 对应 Blob 或调度内容 Agent；任何修复必须进入显式 DERIVED run。

- Schema version 可识别，未知字段策略符合版本规则。
- manifest 中每个文件存在、是普通文件、digest/size 一致。
- 磁盘不存在 manifest 未声明的危险文件；允许的辅助文件必须由 Schema 声明。
- statement samples 与 `samples.json` 一致。
- `reports/similarity.json` 通过 package-safe Schema，未知/正文/raw provider 字段和不安全 URL 被拒绝；generation/derived run 还必须将其与 current Evidence/Decision occurrence 确定性交叉核对。外部 verification 只把包内投影视为 origin attestation，并使用包外新 Evidence/Decision 参与 current PrePackage Gate。
- 测试 ID 唯一，每个 `.in` 有相应 `.ans`（SPJ Schema 另行定义）。
- checker/validator/generator/solutions 路径存在且关联 toolchain manifest。
- 包内原始 `PrePackageQualityReport` 与 manifest 声明的 origin run/revision/profile 一致，作为不可变 provenance 的一部分。
- 调用 Gate 时提供的 current PrePackage evidence 属于当前 generation/derived/verification run、当前 profile/revision，所有打包前不可豁免门禁通过；generation/derived run 可令它与包内报告为同一 digest，独立 verification run 则保存在包外。
- waiver 仍然 active。
- 通过只读 reverse reader 可重新构造 `VerifiedProblem`。

Package Gate 失败时不得写 `READY`。

### PackageStructuralGate

`PackageStructuralGate` 是可独立复用的安全结构子集，用于 Slice 0 和 `cpgen verify` 导入未知目录前。它从不修改源目录；`inspect` 模式只校验，`import` 模式还会通过 MeteredArtifactSink 将已打开的普通文件流式复制成待提交 Blob：

- 以不跟随链接的安全遍历读取目录，先实施文件数、路径和总字节上限。
- 校验 manifest Schema、package ID、声明文件的类型/digest/size、额外文件策略和跨平台路径冲突；文件检查与复制使用同一个不跟随链接的已打开句柄，避免检查后替换。
- 校验包内原始 PrePackage/provenance、SimilarityPackageReport 的 Evidence/Decision digest 引用与 manifest origin 字段自洽，但不尝试证明未随包携带的私有 Evidence 内容，也不把这些 origin 自述视为当前 run 已通过质量门禁。
- `inspect` 模式通过受限 reader 重构 `VerifiedProblem` DTO；`import` 模式的 reverse reader 只读取刚复制且 digest 已校验的 Blob，不再读取 observed path。
- StructuralGate 自身不编译/运行包内程序，也不直接写 ArtifactOccurrence、receipt、package occurrence 或 run 状态；import 成功后由 Orchestrator 单事务提交 PendingArtifact 和 `status=IMPORTED` 的 package occurrence。只有完整门禁事务可补入 receipt/final report 并转 `VERIFIED`。

正式 `verify import` 必须已经有 verification run/lease/budget，使用其 MeteredArtifactSink。Slice 0 无 SQLite run，只允许 `Slice0ProbeHarness` 对仓库内固定可信 fixture 使用 `ProbeArtifactSink` 的硬编码字节/文件上限；该路径不能被正式 CLI 或外部输入复用。

完整 Package Gate 在 StructuralGate 之后，再检查当前 run 的 PrePackage evidence、waiver、profile/environment 绑定并产生 receipt。StructuralGate 通过本身永远不能使 run `READY`。

### PackageVerificationReceipt

```json
{
  "schema_version": "cpgen.package-receipt/v1",
  "package_id": "...",
  "verification_run_id": "...",
  "verification_profile": "release",
  "environment_digest": "sha256:...",
  "manifest_digest": "sha256:...",
  "prepackage_evidence_digest": "sha256:...",
  "gate_version": "package-gate-v1",
  "status": "PASSED",
  "checked_at": "...",
  "evidence_digests": []
}
```

Receipt 不位于 package tree 内，因此不会改变 package ID。它绑定当前 verification run/profile/environment 及本次 PrePackage evidence。最终综合 QualityReport 引用该 evidence 和 receipt；它同样位于包外，并由数据库 package occurrence 关联。同一 package ID 被另一个 verification run 重验时会产生新的 occurrence、receipt 和最终报告，不修改原包。

### READY 提交

最终 fencing 事务必须同时完成：核对 current revisions/PrePackage evidence/waiver 与刚重验的正式目录；建立 receipt 和 final QualityReport 的 ArtifactOccurrence；为 generation/derived run 新建 `VERIFIED` occurrence，或把 verify run 已有的 `IMPORTED` occurrence填入二者并转为 `VERIFIED`；最后把 `runs.state/ready_package_occurrence_id/ready_package_status` 同一条 UPDATE 设为 `READY/<id>/VERIFIED`。SQLite 的同行 CHECK 与延迟组合 FK 要求 pointer 指向同一 run 的 VERIFIED occurrence。任何跨 run pointer、缺 receipt/final report、只通过 StructuralGate 或仅完成 rename 的目录都不能提交 READY。

## 7. Polygon Exporter

MVP Exporter 接口：

```go
type PackageExporter interface {
	Target() ExportTarget
	Plan(context.Context, VerifiedProblem) (ExportPlan, error)
	Export(context.Context, VerifiedProblem, MeteredPackageWriter) (ExportResult, error)
	Verify(context.Context, PackageReader) (ExportVerification, error)
}
```

传给 `Export` 的 writer 已被 Orchestrator 限定到该 target 的已验证 `ExportPlan`，没有 scope/路径扩张、raw filesystem、Blob put 或 shell 能力；`Verify` 只拿到同一 staging 的受限只读视图。

Polygon adapter 负责：

- statement Markdown/样例到目标 statement 布局。
- 测试、validator、checker、generator 和 solutions 到目标目录。
- 时间/内存、测试组和 checker 类型到目标 metadata/problem.xml 模板。
- 生成 exporter-specific manifest，记录模板和 exporter version。

在取得 Polygon 真实导入夹具前不得宣称所有版本完全兼容；MVP 以固定 contract fixture 验证目标版本。GenerationRequest 中声明的 export target 在 Package Gate 前生成并进入同一个 package ID。对已经 `READY` 的包追加导出目标时，必须创建独立 derived package/verification run 和新 package ID，不能原地修改。

## 8. 修复与预算

- `max_package_attempts` 默认 3。
- 模板/路径/缺失派生文件错误可重试 Package Agent。
- 缺失上游 Blob 或 digest 不一致为不可恢复一致性错误。
- Schema 目标不支持当前题型时返回明确 unsupported，不静默降级。
- Package attempt 也有 attempt ID、日志、`max_artifact_bytes` 和独立 `max_package_bytes` 预算。

## 9. 测试

- 路径穿越、大小写冲突、Unicode 归一化冲突。
- 文件缺失、额外文件、hash/size 篡改。
- staging 崩溃后不出现半成品正式目录。
- staging 与最终目录跨 volume 配置被拒绝；故障注入验证 file flush → rename → parent flush → DB READY 顺序。
- 正式目录 rename 后、READY 事务前崩溃可被重新验证并幂等认领；不匹配目录不能被认领或覆盖。
- 重复构建相同 package_id 幂等。
- 预置“manifest ID 相同但文件缺失/同尺寸篡改/额外文件/证据过期”的目标目录，完整 Gate 必须拒绝且不得 READY/覆盖。
- Package Gate 失败时数据库没有 READY。
- Polygon 固定 fixture 的导出与反向验证。
- Exporter 无法写出 `ExportPlan`、目标前缀或 `max_package_bytes`，也无法取得 staging/BlobStore 的原始路径或创建链接/特殊文件。
- staging `manifest.json` 与 manifest PendingArtifact 经 `OpenVerified` 读取的字节/digest 完全一致；在 manifest、receipt、最终报告产生后到 READY 提交前逐点崩溃，recovery 均能通过 writer token/pin 安全重放或收敛。

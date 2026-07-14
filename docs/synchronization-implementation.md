# 同步机制实现说明

本文按 Pull 和 Push 的实际执行顺序说明同步实现，在各步骤首次引入其使用的内容对象；随后说明这些对象如何存储、引用和回收，以及相关 Kernel 能力和实现边界。

## 目录

- [本地工作树假设](#本地工作树假设)
- [Pull 执行流](#pull-执行流)
  - [执行流程](#执行流程)
  - [冻结本地工作副本](#冻结本地工作副本)
  - [读取 Kernel 并构造远端快照](#读取-kernel-并构造远端快照)
  - [PullOperationState 与中断恢复](#pulloperationstate-与中断恢复)
- [Push 执行流](#push-执行流)
  - [`add` 与 `commit`：形成推送意图](#add-与-commit形成推送意图)
  - [执行流程与 PushOperationState](#执行流程与-pushoperationstate)
  - [文档历史与块操作](#文档历史与块操作)
  - [规范化、本地物化与完成](#规范化本地物化与完成)
  - [Push 中断恢复](#push-中断恢复)
  - [活动 Push 的对账与继续](#活动-push-的对账与继续)
- [对象关系总览](#对象关系总览)
- [对象存储与引用](#对象存储与引用)
- [辅助本地操作](#辅助本地操作)
- [Kernel API](#kernel-api)
- [当前实现边界](#当前实现边界)
- [术语与相关文档](#术语与相关文档)

## 本地工作树假设

实现把本地映射目录视为单写者工作树：同一时刻只允许一个 `siyuan-worktree` 命令运行，并假设 pull/push 物化 Markdown 期间编辑器、网盘同步和其他程序不会同时写入这些文件。CLI 工作树锁只负责前一部分，不会锁住编辑器或操作系统中的其他进程。

在该假设下，本地文件并发不作为需要自动合并的同步冲突来源。WorkingTreeSnapshot、物化前哈希比较和紧邻替换前的二次读取继续保留，用于发现误操作并安全停止；它们不是文件租约或通用多写者协议。普通编辑仍可在命令执行前后进行，只有与 pull/push 的本地物化同时发生的保存不在支持范围内。

## Pull 执行流

pull 负责读取 SiYuan 当前状态，并把远端变化安全合并到本地工作副本。它只读取 Kernel，不调用任何 Kernel mutation API。

### 执行流程

正常 pull 按 `fetch + merge/rebase` 执行：

1. 稳定读取已跟踪 Markdown，持久化 WorkingTreeSnapshot。
2. 保存 `prepared` PullOperationState，并由 `refs.operation` 指向它。
3. 通过读取屏障等待调用前已经排队的 Kernel transaction 完成，随后重复读取映射范围内的 notebook、doc、subdoc、块关系、Kramdown 和属性，生成本次 Observed Remote WorkspaceTree。
4. 以事务开始时的 Remote Tracking WorkspaceTree 为 Base，使用已冻结的 WorkingTreeSnapshot 逐文档比较 Local 与 Observed Remote，生成完整物化计划和冲突材料。
5. 保存 `remote-snapshot-created`，再进入 `materializing-working-tree`。
6. 逐文档重新验证对应文件仍符合事务开始时的内容哈希，然后物化 Markdown、metadata 和冲突材料；每完成一篇文档就保存进度。
7. 保存最终 `state.json`。
8. 通过一次带 generation 校验的 refs 原子更新推进 `refs.remote`，使 Observed Remote 成为新的 Remote Tracking；无冲突时同时创建 baseline Commit 并推进 HEAD/Index，最后清空 Operation。

### 冻结本地工作副本

执行流程开始时，pull 先稳定读取本地 Markdown。下面两个对象共同保存这次读取结果，并成为后续三方比较和覆盖检查使用的 Local。

WorkingTreeSnapshot 表示一次命令开始时实际读到的本地工作副本。它按已跟踪文档保存：

```text
WorkingTreeSnapshot
  └── WorkingFileRecord
        ├── documentId / localPath
        ├── missing
        ├── size / modifiedAtUnixNano
        ├── contentHash
        └── contentObject -> WorkingFileContent
```

WorkingFileContent 的落盘对象是用 JSON 包装的 Markdown：

```json
{
  "type": "working-file",
  "version": 3,
  "data": {
    "markdown": "<!-- siyuan-worktree:block 20260714120000-abcdefg type=p -->\n正文内容。\n{: id=\"20260714120000-abcdefg\"}\n<!-- /siyuan-worktree:block 20260714120000-abcdefg -->\n"
  }
}
```

WorkingTreeSnapshot 本身不重复保存 Markdown，而是通过 `contentObject` 引用 WorkingFileContent：

```json
{
  "type": "working-tree",
  "version": 3,
  "data": {
    "files": [
      {
        "documentId": "20260714110000-hijklmn",
        "localPath": "notes/example.md",
        "size": 188,
        "modifiedAtUnixNano": 1784088000000000000,
        "contentHash": "<sha256-hex>",
        "contentObject": "sha256:<working-file-object>"
      }
    ]
  }
}
```

如果已跟踪文件不存在，该记录会保存 `"missing": true`，并省略内容哈希和 `contentObject`。

WorkingFileContent 保存规范化后的 Markdown。它和其他对象一样按内容寻址，因此不同扫描或事务读到相同内容时会复用同一个对象，不会重复保存文件副本。

扫描文件时会在读取前后比较文件身份、大小和修改时间；如果文件正在变化，最多重试三次，仍不稳定就停止命令。`size` 和 `modifiedAtUnixNano` 用于描述本次读取和辅助判断，最终内容相等性与覆盖前置条件以 `contentHash` 为准。

WorkingTreeSnapshot 不是长期历史。普通 `status`、`diff` 和 `add` 只在内存中使用一次稳定扫描；pull 会把它持久化到当前 PullOperationState，事务收敛后由 GC 回收。

### 读取 Kernel 并构造远端快照

pull 读取顶层块顺序、Kramdown 和属性后，用 BlockSnapshot、DocumentTree 和 WorkspaceTree 表示本次 fetch 接受的 Observed Remote。它们同时也是 `add` 和 push 共用的状态对象，并非 pull 专用。

这些 API 不是一次原子查询。实现不会把一次 inventory、一次块列表和一次 Kramdown 返回直接拼成快照，而是使用稳定观察协议：

1. 调用 `/api/sqlite/flushTransaction` 建立读取屏障，等待调用前已经排队的 Kernel transaction 和相关索引写入完成。该调用返回后不持有锁，新的 transaction 可以立即进入。
2. 开始一轮映射范围采集，读取 notebook 和文档的 ID、名称、顺序、路径和层级信息作为本轮 inventory。
3. 对每篇文档递归读取块关系，再批量读取同一组 Block 的 Kramdown 和属性，随后重新读取块关系；结构在单篇采集内变化则丢弃整轮结果。
4. 将同一批数据同时构造成文档内容和 metadata，计算包含有序块关系、Kramdown 和受保护属性的文档语义版本。
5. 所有文档完成后再次读取 inventory；如果映射范围在本轮中发生变化，则丢弃整轮结果。
6. 以 `inventory + 按 Document ID 排序的文档语义版本集合` 计算本轮版本。只有连续两轮版本完全相同才接受本次 Observed Remote。

整个映射范围最多采集四轮，并要求其中出现两个连续相同的完整版本集合。四轮内无法满足条件时，命令返回 `remote unstable`；PullOperationState 保持在 `prepared`，Working Tree 和内容 refs 不会被推进。

#### BlockSnapshot

BlockSnapshot 以一个顶层 Block 及其完整子树作为最小可写快照单元：

```json
{
  "type": "block-snapshot",
  "version": 3,
  "data": {
    "blockId": "20260714120000-abcdefg",
    "blockType": "l",
    "kramdown": "...",
    "attrsByBlockId": {
      "20260714120000-abcdefg": {
        "custom-owner": "agent"
      }
    }
  }
}
```

SiYuan Block ID、`blockType`、Kramdown、IAL 和受保护属性都参与对象哈希。`blockType` 是 SiYuan 的块类型代码，例如 `p` 表示段落、`h` 表示标题、`l` 表示列表。单个 BlockSnapshot 不记录顶层块顺序；顺序由 DocumentTree 中有序的 BlockSnapshot 引用表达。

本地新增内容在 push 前还没有 SiYuan Block ID。系统会暂时使用 provisional ID 表示它；push 成功后，由 SiYuan Kernel 创建 Block 并分配正式的 SiYuan Block ID。

#### DocumentTree 与 WorkspaceTree

DocumentTree 保存文档根 Block ID（下文简称 Document ID）、有序的顶层 BlockSnapshot 引用，以及用于保留工作副本排版的完整 Markdown：

```json
{
  "type": "document-tree",
  "version": 3,
  "data": {
    "documentId": "20260714110000-hijklmn",
    "markdown": "...",
    "blocks": [
      {
        "blockId": "20260714120000-abcdefg",
        "objectId": "sha256:..."
      }
    ]
  }
}
```

加载 DocumentTree 时会重新解析 Markdown，并逐项校验 Block ID、类型、内容和 provisional 状态是否与 BlockSnapshot 图一致。

WorkspaceTree 保存映射范围、notebook 列表和所有文档引用，包括 Document ID、notebook、标题、远端路径、本地路径和 DocumentTree Object ID。doc/subdoc 层次当前通过远端路径和本地路径间接保留。

WorkspaceTree 持久化的是一个文档内容版本向量：`Document ID -> 已通过整轮重复读取验证的 DocumentTree Object ID`，再加上每轮前后复核过的 inventory。它不宣称 Kernel 为所有文档提供了同一全局时刻的原子快照；完整版本集合连续两轮相同只能提供乐观稳定性证据，不能排除 ABA，也不能阻止最后一次读取后的新写入。

稳定观察还会校验文档根和 Block 的完整 Kernel metadata，并用同一批数据生成 `meta/documents/` 侧车；但当前 v3 WorkspaceTree 没有独立引用完整 metadata 对象。因此 Remote Tracking 的内容哈希覆盖 inventory 和 DocumentTree，完整 metadata 仍是可重建 projection，尚未全部进入 Baseline/Remote Tracking 哈希。

### PullOperationState 与中断恢复

文件系统不能把多篇 Markdown、metadata 和 `state.json` 作为一个共同的原子写入。PullOperationState 因此保存完整计划和逐文档进度，使中断后的 pull 可以继续收敛，而不是重新猜测哪些文件已经更新。

PullOperationState 保存：

- 事务开始时的 HEAD、Index、Remote Tracking 和 `state.json`。
- WorkingTreeSnapshot，以及随后 fetch 接受的 Observed Remote WorkspaceTree。
- 每篇文档的目标路径、目标内容、metadata、冲突材料和移动计划。
- 写文件前应看到的缺失状态或内容哈希。
- 已完成物化的文档前缀、PullResult、错误和更新时间。

阶段为：

```text
prepared
  -> remote-snapshot-created
  -> materializing-working-tree
  -> 原子推进 refs 并清空 Operation
```

`prepared` 表示本地读取已经冻结，但 Observed Remote 和合并计划尚未完成；`remote-snapshot-created` 表示 fetch、三方比较与完整计划已经持久化；`materializing-working-tree` 表示可以按 journal 继续更新本地文件。

活动 pull 存在时，`diff`、`add`、`restore` 和 `push` 会要求先再次运行 `pull` 完成恢复。`status` 仍可执行，并从 PullOperationState 显示当前 phase、物化进度、当前文档、错误和下一条 `pull` 命令；由于 Working Tree 可能只完成了计划前缀，它不会把这套混合状态与一次新的远端观察比较。进入本地物化阶段后，普通 `reset` 也会拒绝丢弃恢复证据，因为进程可能已经写完文件但尚未来得及保存该文档的完成标记；`reset --force` 只应在人工检查本地文件后使用。

pull 重启后从 `refs.operation` 加载 PullOperationState：

- `prepared`：重新执行 fetch 和计划生成，但继续使用事务开始时冻结的 WorkingTreeSnapshot 作为 Local。
- `remote-snapshot-created`：先持久化进入物化阶段，再开始写本地文件。
- `materializing-working-tree`：跳过已经记录完成的文档，从下一篇继续。

每篇文档物化前都会检查本地文件；完成 Kernel 回读和 metadata 准备后，还会在紧邻 Markdown 替换前再次读取。文件仍是事务开始时的内容时可以写入；文件已经等于本事务目标时，说明上次可能在“写文件后、保存进度前”中断，也可直接确认完成；其他内容一律视为事务期间出现的新编辑并停止。第二次检查仍不能把普通文件系统的最终 `check -> rename` 变成条件写入，但会显著缩短覆盖编辑器新保存内容的窗口。

## Push 执行流

push 不直接发送当前 Working Tree，而是只执行用户已经通过 `add` 和 `commit` 冻结的推送意图。

### `add` 与 `commit`：形成推送意图

`add` 的实现步骤为：

1. 从 HEAD 读取 Base WorkspaceTree。
2. 从当前 Index 保留未选择文档的暂存状态。
3. 解析所选 Markdown，并拒绝只读属性、复杂块和不安全结构变化。
4. 生成候选 DocumentTree 和 WorkspaceTree。
5. 从 BaseTree 与 TargetTree 生成并验证一个 PatchObject。
6. 通过一次带 generation 校验的 refs 原子更新，同时切换 `index` 和 `indexPatch`。

Patch 会在读取暂存状态、执行 commit 和 push 前再次根据两个快照生成并校验，不能脱离 BaseTree/TargetTree 单独成为事实来源。

`commit` 不重新读取 Working Tree，只冻结当前 Index。成功后 HEAD 指向 user CommitObject，Index 保持指向该 Commit 的 WorkspaceTree，IndexPatch 清空，Remote Tracking 不变。

当前一次只允许一个待 push user Commit。

#### PatchObject 与 CommitObject

PatchObject 连接完整的 BaseTree 和 TargetTree，并按文档保存 DocumentPatch：

```json
{
  "type": "patch",
  "version": 3,
  "data": {
    "baseTree": "sha256:<base-workspace-tree>",
    "targetTree": "sha256:<target-workspace-tree>",
    "documentPatches": [
      {
        "version": 3,
        "documentId": "20260714110000-hijklmn",
        "localPath": "notes/example.md",
        "operations": []
      }
    ]
  }
}
```

一个 Commit 只引用一个 Patch；DocumentPatch 是单篇文档的执行和恢复边界。DocumentPatch 内按顺序保存：

| 操作 | 不可变执行意图 |
| --- | --- |
| `update` | 目标 SiYuan Block ID、块类型、expected hash、目标内容 |
| `insert` | 目标文档的根 Block ID、父 Block ID、前后锚点 Block ID、待插入 Markdown |
| `delete` | 目标 SiYuan Block ID、expected hash |

每个操作都有 `sha256:<hex>` 形式的稳定 `operationId`。哈希只包含不可变执行意图；属性备份、Insert 前置条件、receipt 和回读结果不参与哈希。`operationId` 只用于在本地关联操作意图、Kernel 返回结果和回读证据，不会传给 Kernel。因此同一个请求被重复发送时，Kernel 无法根据 `operationId` 判断它已经处理过，也就不能自动阻止重复插入。

用户执行 `commit` 时创建临时 user CommitObject：

```json
{
  "type": "commit",
  "version": 3,
  "data": {
    "kind": "user",
    "displayId": "20260715T...",
    "tree": "sha256:<workspace-tree>",
    "baseHead": "sha256:<stable-base-commit>",
    "remoteBase": "sha256:<stable-baseline-tree>",
    "patch": "sha256:<approved-patch>",
    "message": "更新设计文档",
    "createdAt": "..."
  }
}
```

`baseHead` 是 reset 目标，`remoteBase` 是 Patch Base。稳定的 baseline CommitObject 只包装当前 Baseline Tree，不含 `baseHead` 或 Patch。

### 执行流程与 PushOperationState

push 从 HEAD user CommitObject 和 PatchObject 恢复执行意图，重新校验 BaseTree、TargetTree 和 Working Tree，然后创建 PushOperationState。

新 push 在进入 `applying` 前先稳定读取 Commit 涉及的全部文档，并对每个 DocumentPatch 做完整预检。只有该 Commit 的所有 DocumentPatch 都可应用，才开始创建第一篇文档历史。全部预检通过后，得到的 DocumentTree 会作为完整集合一次性写入 `PreflightDocuments`；如果任一文档从一开始就有冲突，整个 preflight 在零 Kernel mutation、零文档历史的状态下停止，也不会持久化半套 `PreflightDocuments`。

全局 phase 为：

```text
prepared
  -> applying
  -> remote-verified
  -> canonical-snapshot-created
  -> materializing-working-tree
  -> 原子推进 HEAD/Index/refs.remote 并清空 Operation ref
```

phase 只表示耐久进度并单调向前。冲突或失败通过 `Commit.status`、`DocumentPatch.status` 和 `Operation.error` 附着在当前 phase，不通过倒退 phase 表示。

PushOperationState 保存：

- CommitObject 的 Object ID、BaseTree Object ID 和 TargetTree Object ID。
- mutation 前完整通过预检的文档快照 `PreflightDocuments`。
- 带运行时进度的 Commit 与 DocumentPatch。
- 已完成文档的 canonical DocumentTree。
- canonical WorkspaceTree。
- 已经物化到 Working Tree 的文档列表。
- 当前错误信息和更新时间。

`applying` 期间，已经完成文档的 canonical DocumentTree 立即写入 `CanonicalDocuments`，但全局 phase 只有在全部文档验证完成后才进入 `remote-verified`。

全量预检不能替代操作前置条件。每个 Update/Delete 仍会在最靠近 mutation 的位置连续读取目标 Block 并比较 expected hash；Insert 会连续读取父节点的有序直接子 Block 列表。读取前再次调用 transaction flush barrier，写后仍执行稳定回读。

### 文档历史与块操作

#### 文档历史子状态

每篇文档在第一个块写操作前建立一次历史检查点：

```text
none
  -> requested
  -> accepted-unverified
  -> verified | unverified
```

`requested` 一定先于 `createDocHistory` 调用保存。create 成功后保存 `accepted-unverified`，随后搜索历史。恢复时已经进入 accepted 状态的文档不会重复 create，只重新搜索。

当前普通 push 在 `unverified` 时继续，因为历史接口只能提供弱验证；严格模式尚未实现。

#### 块操作子状态

单个块操作的耐久顺序为：

```text
planned
  -> prepared
       update: preserved-attrs-saved
       insert: insert-precondition-saved
  -> in-flight-saved
  -> Kernel API call
  -> transaction-returned          仅存在于内存
  -> receipt-persisted
  -> readback-verified
  -> applied-persisted
```

`transaction-returned` 与 `receipt-persisted` 是两个不同边界：HTTP 响应到达后，本地仍需一次原子写入才能把 receipt 变成耐久事实。

Update 通过目标 Kramdown 等价证明成功；Delete 通过目标 Block 不存在证明成功。Insert 在请求前保存完整顶层 `childBlockIds`，响应后保存 transaction receipt 返回的新 SiYuan Block ID，再回读父文档并保存锚点间完整的 `resultBlockIds`。

一次 Markdown Insert 可能生成多个顶层 Block，因此 receipt 返回的 `receiptBlockIds` 和回读得到的 `resultBlockIds` 都必须保留。

### 规范化、本地物化与完成

所有块操作完成后，push 重新读取本次 Commit 影响的每篇文档并保存 canonical DocumentTree。Kernel 为新增 Block 分配的 SiYuan Block ID 和规范化后的 Kramdown 以该回读结果为准；属性保持只读，并在 mutation 前后验证，发现差异时停止而不会把旧值覆盖回去。

canonical WorkspaceTree 以 Commit Target 为基础，只用这些受影响文档的 canonical DocumentTree 替换对应引用；push 不会在完成阶段重新遍历整个映射范围。因此它不是整个 Kernel 的最新全局观察，未参与 Commit 的文档若在远端变化，会由后续 `status` 或 `pull` 发现。

canonical DocumentTree 一定先于 Working Tree 物化保存。每篇 Markdown、metadata 和 state 更新完成后，文档 ID 才会加入 `MaterializedDocuments`。

本次 Commit 的全部文档物化完成后生成 baseline CommitObject，并通过一次带 generation 校验的 refs 原子更新：

- 将 HEAD、Index 和 `refs.remote` 指向 canonical 结果。
- 清空 IndexPatch 和 Operation。
- 使 user Commit、Patch、旧基线和 PushOperationState 进入不可达状态，等待 GC。

如果 commit 后 Working Tree 又被编辑，push 会在发送 Kernel mutation 前拒绝，避免 canonical 物化覆盖新编辑。

### Push 中断恢复

push 重启后从 `refs.operation` 加载最后一个耐久 PushOperationState，验证 Commit、Patch、BaseTree 和已记录进度，再从当前 phase 继续。

多文档执行采用已完成前缀。`Commit.AppliedDocuments` 之前的文档必须都存在 canonical DocumentTree，当前文档再以 `AppliedOperations` 表示已验证的块操作前缀。后续文档失败时不会清空这些进度，也不会重写已经完成的文档；普通 `push` 和 `push --continue` 都从第一个未完成文档继续。

恢复只使用已经持久化的事实：

- mutation 前保存的远端状态和锚点。
- 不可变操作意图和 `operationId`。
- 已保存的 Kernel receipt。
- Kernel 当前回读结果。
- 已完成的 canonical 快照和本地物化范围。

不会根据“请求大概成功了”直接重复产生副作用。

下面三个动作不能形成跨系统原子提交：

```text
本地保存 in-flight
  -> SiYuan Kernel 写入
  -> 本地保存 receipt
```

当前保证为：

- CLI 工作树锁串行化同一工作树中的本地命令；它不限制 SiYuan GUI、插件、同步任务或其他 API 客户端写入 Kernel。
- in-flight 一定先于 Kernel mutation 保存。
- receipt 一定先于回读结果保存，回读结果一定先于 applied 计数推进。
- Update/Delete 可以通过目标状态幂等确认。
- Insert receipt 丢失时由 pull 建立新的 Observed Remote 并执行合并判断。
- 新基线只在远端验证和全部本地物化完成后生效。

### 活动 Push 的对账与继续

存在活动 PushOperation 时，pull 不执行普通 Working Tree 合并，而是作为 `fetch + reconcile/rebase` 恢复步骤：

1. fetch 稳定读取当前未完成文档；在尚无远端效果的 preflight 阶段，还会检查 Commit 的全部剩余文档。
2. 对账当前 in-flight 操作，验证当前文档中已经记录为 applied 的操作前缀。
3. 用当前 Observed Remote 重新校验剩余 DocumentPatch。远端只修改了其他 Block 时，原操作仍可直接重放，相当于把剩余意图 rebase 到当前远端。
4. 持久化新的完成前缀或冲突，但不调用任何 Kernel mutation，也不物化 Working Tree。

通用结果为：

| `pushReconciliation` | 含义 | 后续处理 |
| --- | --- | --- |
| `ready` | 已完成前缀有效，剩余 Patch 可在当前远端继续 | `push --continue` |
| `applied` | in-flight 操作的目标状态已经存在，已补记为 applied | `push --continue`，从后续操作或文档继续 |
| `not-applied` | 远端仍是可信写入前状态，已清除 in-flight | `push --continue` 安全重试当前操作 |
| `conflict` | 当前状态既不是可信前态也不是目标结果，或剩余 Patch 无法安全重放 | 处理冲突后重新 pull |

Update 以“当前 Kramdown 等价于目标”证明已执行，以 expected hash 证明未执行；Delete 以“Block 不存在”证明已执行，以 Block 仍保持 expected hash 证明未执行。Insert 需要额外保存写前 children、前后锚点和待插入内容：

| 写前 children 与当前 children | receipt | 判定 |
| --- | --- | --- |
| 完全相同 | 无 | 上次 Insert 未发生；pull 清除 in-flight，push 可以安全重试 |
| 完全相同 | 有 | receipt 指向的结果已不存在；保存冲突 |
| 发生变化 | 无，锚点间内容唯一匹配目标 | pull 保存新增 Block 的 SiYuan Block ID，标记 operation applied |
| 发生变化 | 无，内容不能唯一匹配 | 保存冲突，不自动重试 |
| 发生变化 | 有，`receiptBlockIds` 位于锚点间新增 Block ID 集合 | 保存完整的 `resultBlockIds`，标记 applied |
| 发生变化 | 有，但 Block ID、锚点或原有顺序不匹配 | 保存冲突 |

内容匹配会忽略 IAL 中由 Kernel 自动生成的 Block ID（`id`）和更新时间（`updated`），但不会忽略用户属性。

该恢复路径对应 Git sequencer 的 `continue`，不是远端事务回滚。已经验证完成的文档是不可撤销的远端事实；`reset --force` 只能丢弃本地恢复证据，不会恢复这些文档。

如果 Insert 请求已经发出但响应丢失，之后其他客户端又在相同锚点间插入内容，当前实现不会猜测其中某个子集属于本次操作，而是保存冲突。

## 对象关系总览

前面的执行流已经在对象第一次出现时说明了其结构。下面汇总它们在各命令中的角色：

| 对象 | 主要创建者 | 主要使用者 |
| --- | --- | --- |
| BlockSnapshot | pull、`add`、push 规范化回读 | pull 合并、`add` 生成 Patch、push 校验和规范化 |
| DocumentTree | pull、`add`、push 规范化回读 | 表示单篇文档在 Base、Index、Target、Observed Remote、Remote Tracking 或 canonical 边界上的内容状态 |
| WorkspaceTree | pull、`add`、push | 表示整个映射范围，作为 Observed Remote、Remote Tracking、Index、Commit Target 和 canonical 结果 |
| WorkingFileContent | pull | 保存 pull 开始时的本地 Markdown、物化目标和冲突内容 |
| WorkingTreeSnapshot | pull | 冻结 pull 开始时的工作副本，并支持覆盖检查和中断恢复 |
| PatchObject | `add` | `commit` 引用，push 加载、校验并执行 |
| CommitObject | `commit`、pull、push | user Commit 冻结推送意图；baseline Commit 包装当前 Baseline |
| PullOperationState | pull | 保存 fetch、合并计划和多文件物化进度 |
| PushOperationState | push | 保存全量 preflight、Kernel 写入、receipt、已完成文档前缀、回读和本地物化进度；pull 用它统一对账 Update、Delete、Insert 和剩余 DocumentPatch |

## 对象存储与引用

### 内容寻址对象

BlockSnapshot、DocumentTree、WorkspaceTree、WorkingFileContent、WorkingTreeSnapshot、PatchObject、CommitObject、PullOperationState 和 PushOperationState 都以“对象类型 + 格式版本 + 规范化 JSON”计算 SHA-256。

相同内容得到相同对象 ID，已经写入的对象不再原地修改。状态变化会写入新对象，再通过 refs 使新对象生效。

### 原子引用

`refs/state.json` 一次保存所有关键引用：

```json
{
  "version": 3,
  "generation": 42,
  "head": "sha256:<commit>",
  "index": "sha256:<workspace-tree>",
  "indexPatch": "sha256:<patch-or-empty>",
  "remote": "sha256:<workspace-tree>",
  "operation": "sha256:<operation-state-or-empty>"
}
```

更新顺序为：

1. 写入新的不可变对象并同步文件。
2. 写入 `refs/state.json.lock`。
3. 校验调用方读取到的旧 generation 仍然匹配。
4. 同步 lock 文件并通过 rename 替换正式 ref。
5. 在支持的平台同步父目录。

这里使用 CAS（Compare-And-Swap，比较并交换）更新 refs：只有当前 `generation` 仍等于命令开始时读取的值，才写入整套新引用并将 generation 加一；如果不相等，说明期间状态已经被其他操作修改，本次更新会失败。这样 HEAD、Index、IndexPatch、作为 Remote Tracking 的 Remote 字段和 Operation 可以一次切换，避免只更新其中一部分。

进程若在 ref 更新前退出，新对象只是暂时不可达；若 rename 已完成，新状态就是完整可读的。下一次取得工作树锁时会检查遗留 lock：只有内容完整、对象图有效且 generation 连续的候选状态才会完成提交，无效候选会被丢弃。

Linux 和 macOS 会同步文件及父目录。Windows 使用同一 rename 流程并可正常构建运行，但平台不提供同等级的目录 `fsync` 保证，因此断电耐久承诺弱于 Unix 平台。

### 对象生命周期与 GC

稳定且无冲突时，HEAD、Index 和作为 Remote Tracking 的 Remote 字段指向同一套 Baseline 内容。额外对象只在当前编辑或同步事务需要时存在：

| 阶段 | 额外可达对象 |
| --- | --- |
| `add` 后 | Index WorkspaceTree、Index Patch |
| `commit` 后 | user CommitObject、目标 WorkspaceTree、Patch |
| `pull` 中 | WorkingTreeSnapshot、PullOperationState、新 Observed Remote WorkspaceTree、物化计划及冲突内容 |
| `push` 中 | PushOperationState、preflight DocumentTree、receipt、canonical DocumentTree/WorkspaceTree |
| 冲突时 | 原 Baseline、当前 Remote Tracking、事务对象和冲突材料 |

push、pull 或 reset 成功收敛后，旧基线和已完成事务对象不再被 refs 引用。best-effort GC 从 HEAD、Index、IndexPatch、Remote 和 Operation 出发标记可达对象，再尝试删除其他对象。

GC 失败不影响 refs 的正确性，后续状态变更会再次尝试。当前没有单独的 GC 诊断状态或手动 `gc` 命令。

## 辅助本地操作

### `status`、`diff` 与 `log`

本地内容状态由 Working Tree、refs 和本次 Observed Remote 的比较结果计算。读取 Working Tree 时，命令先完成一次稳定扫描，再基于这次扫描得到的内容和哈希执行后续比较，避免同一命令的不同步骤看到不同版本的文件：

| 状态面 | 实现来源 |
| --- | --- |
| Observed Remote | 当前命令连续稳定读取得到的临时 Kernel 版本集合 |
| Remote Tracking | `refs.remote` 指向的最近一次已接受 WorkspaceTree |
| Baseline | `kind=baseline` CommitObject；user Commit 通过 `baseHead`/`remoteBase`、Operation 通过 BaseTree 固定事务基线 |
| Working | 当前 Markdown 解析得到的 DocumentTree |
| Staged | `refs.index` 与 `refs.indexPatch` |
| Committed | HEAD 中 `kind=user` 的 CommitObject |

没有活动事务时，`status` 稳定读取 Kernel 得到 Observed Remote，并比较 Remote Tracking、Baseline 和 Working；再结合 Index、HEAD 和冲突目录生成 `clean`、`local-modified`、`remote-modified`、`mergeable`、`converged`、`conflict`、`local-missing` 或 `remote-missing`。

只要 `refs.operation` 非空，`status.activeOperation` 就会从不可变 OperationState 汇总 Git sequencer 风格的恢复信息：事务类型、phase、运行/失败/冲突状态、Commit、远端已应用文档数、本地已物化文档数、当前文档、错误和 `nextAction`。为了让 Kernel 离线或持续变化时仍能读取恢复指引，活动 pull/push 的 `status` 都不重新建立 Observed Remote，而是设置 `documentComparisonDeferred: true` 并只显示可靠的 journal、Index、Commit 和冲突状态。活动 push 发生失败或冲突且仍处于写入阶段时提示先 `pull` 对账，对账成功或已经进入规范化/本地物化阶段后提示 `push --continue`。

`diff` 比较 Working 与 Index，`diff --staged` 比较 Index 与 HEAD Tree。`log` 只读取当前待 push 的 user CommitObject，干净状态返回空列表。

### `reset`

`reset` 不修改 Working Tree：

- 只有暂存状态时，Index 回到 HEAD Tree 并清空 IndexPatch。
- 存在待 push Commit 时，HEAD 和 Index 回到 `baseHead` 指向的 baseline。
- 活动 push 尚无远端 mutation 证据时可以清空 OperationState。
- 已经保存 in-flight、receipt、applied 或 canonical 证据时，普通 reset 拒绝丢弃 OperationState；`reset --force` 只删除本地恢复证据，不撤销 Kernel 已完成的写入。

### 冲突材料与 `restore`

三方冲突物化为：

```text
.siyuan-worktree/conflicts/<document-id>/
├── base.md
├── local.md
├── remote.md
└── conflict.json
```

`conflict.json` 保存文档 ID、本地路径和三方内容哈希。`restore --ours/--theirs` 与用于确认手工解决的 `add` 都会重新建立 Observed Remote；如果新的观察与冲突生成时保存的远端版本不同，命令拒绝使用过期材料。

## Kernel API

### 读取接口

| 用途 | Kernel API |
| --- | --- |
| 枚举 notebook | `/api/notebook/lsNotebooks` |
| 递归枚举文档 | `/api/filetree/listDocsByPath` |
| 读取顶层块顺序 | `/api/block/getChildBlocks` |
| 读取 Kramdown | `/api/block/getBlockKramdown`、`/api/block/getBlockKramdowns` |
| 等待已排队事务和索引写入 | `/api/sqlite/flushTransaction` |

pull 和普通 status 使用这些接口建立稳定远端观察。push 也使用块读取接口执行全量 preflight、操作前置检查和写后验证。Kernel 返回成功不能替代回读。

`flushTransaction` 只保证调用前已经进入队列的 transaction 和 SQL 写入完成。它返回后新的 transaction 可以立即进入，因此实现只把它当作读取屏障，不把它当作锁或快照事务。

### 文档历史

每篇文档执行第一个 mutation 前调用：

```text
POST /api/history/createDocHistory
POST /api/history/searchHistory
```

`createDocHistory` 始终接收文档根 Block ID。随后最多搜索 5 次、每次间隔 100ms，查找 `type=3`、`op=update` 且 `created >= requestedAt.Unix()-1` 的记录。

create 接口不返回唯一的历史记录 ID，历史时间也只有秒级精度，因此搜索结果只是弱证明。状态机不会自动调用 `rollbackDocHistory`、`rollbackNotebookHistory` 等破坏性恢复接口。

### 块写入与 Transaction Receipt

当前写入接口为：

```text
POST /api/block/insertBlock
POST /api/block/updateBlock
POST /api/block/deleteBlock
```

Insert 和 Update 负责把 Markdown 转换为 BlockDOM，Delete 按 Block ID 删除。三者返回 Kernel transaction 数组。

Mutation receipt 保存：

- 本地稳定 `operationId` 和 receipt 接收时间。
- Kernel transaction timestamp。
- `doOperations` 和 `undoOperations` 中的 `action`，以及 `id`、`rootID`、`parentID`、`previousID`、`nextID`、`blockID`、`blockIDs` 等 Block ID 字段。
- 从 `doOperations` 提取的 `receiptBlockIds`。

receipt 是执行证据，最终结果仍由 Kramdown、子块顺序和属性回读确认。

### 属性保护

使用的接口为：

```text
POST /api/attr/batchGetBlockAttrs
```

属性在第一阶段完全只读：

- Base 和 Local 中同一 Block ID 的属性集合和值必须一致，属性顺序可以不同。
- 本地新增内容不能伪造未知的 SiYuan Block ID。
- Update 准备阶段保存现有属性，最靠近 mutation 时再次验证；Kernel 更新内容后再做一次验证。
- 任何属性差异都会停止 push。实现不会调用无条件属性写接口“恢复”旧值，因为那可能覆盖其他客户端刚完成的属性修改。
- 删除整个 Block 可以连同属性一起删除；这属于结构删除，不是属性编辑。

### `/api/transactions`

基于 SiYuan `v3.7.0` Kernel 实现：

- 一个 Transaction 可以包含多个 `doOperations`，任一操作在 commit 前失败会触发该 Transaction rollback。
- 请求中的多个 Transaction 分别进入队列，不构成跨 Transaction 的整体原子提交。
- handler 等待队列 flush/commit 后返回；成功的 insert operation 会补充 Kernel 为新增 Block 分配的 SiYuan Block ID。
- 接口直接接收 Kernel transaction operation，写入内容通常是 BlockDOM，不会像块 API 一样把 Markdown 转成 BlockDOM。
- transaction 错误没有稳定映射为 HTTP envelope 错误，即使 `code == 0` 仍必须回读验证。
- Operation 没有 `expectedHash`、`expectedRevision` 或等价的条件字段；`reqId` 也不是幂等键。
- 它不能提供文件系统级跨文档原子性。

当前继续使用单独的块 API。未来可以评估把单篇文档的多个操作合并为一个 Transaction，以减少同一文档多个 mutation 之间的部分成功；但前提是完成可靠的 Markdown-to-BlockDOM 转换和故障注入测试。即使合并为一个 Transaction，只要 expected revision 的校验仍发生在客户端，它也不能消除校验与写入之间的竞争窗口。

### 写锁与只读接口

基于 SiYuan `v3.7.0` Kernel 源码，目前没有可由外部客户端 acquire/release 的临时写锁：

- `/api/setting/setEditorReadOnly` 只修改编辑器 UI 的只读配置；保护 mutation API 的 `CheckReadonly` 不检查这个字段，插件和其他管理员 API 仍可写入。
- Kernel `--readonly` 是启动模式，开启后 `siyuan-worktree` 自身也无法 push，不是临时租约。
- `/api/sync/setSyncEnable` 修改的是持久配置，既不等待也不取消已经开始的同步任务。
- Kernel 内部 transaction `flushLock` 没有对外 lease token，客户端不能从前置检查一直持有到 mutation 完成。

因此当前保证属于乐观并发控制：稳定读取、全量 preflight、操作前再次校验、写后回读和不确定时停止。若要严格关闭 TOCTOU，需要 Kernel 提供文档级条件事务：在内部 transaction 锁下比较强 revision，匹配后才执行操作；Insert 还应校验父节点有序子 Block ID 摘要，并支持幂等键。

### 数据仓库快照

SiYuan 的数据仓库快照不是把整个 `data/` 目录再复制一份，而是一个“快照索引 + 文件记录 + 文件内容分块”的仓库结构：

```text
Snapshot Index
  ├── Repository Snapshot ID、备注、创建时间、文件数、总大小和设备信息
  └── File Object IDs
        └── File Object
              ├── 路径、大小、更新时间
              └── Chunk IDs
                    └── 压缩并加密保存的实际文件内容
```

创建快照时，Kernel 会先完成待处理事务，再扫描 SiYuan `data/` 目录中未被忽略的文件。Snapshot Index 记录当时包含的所有 File Object ID；每个 File Object 再记录文件路径和组成它的内容分块。内容相同的分块可以被不同文件或不同快照复用，所以多个快照不会各自保存一套完整文件副本。

Chunk ID 根据分块内容计算；File Object ID 当前主要由文件路径和秒级更新时间生成，不是 SiYuan Block ID，也不是整文件内容哈希。

它的恢复粒度仍然是文件：整个快照恢复会按索引重建、更新或删除 `data/` 中的文件；单文件恢复则从 File Object 的 Chunk 列表重新组装该文件。对于 `.sy` 文档，这一层看到的是整个 `.sy` 文件，不理解其中的 Block、Block ID 或插入位置。

SiYuan 数据仓库提供：

```text
/api/repo/createSnapshot       /api/repo/tagSnapshot
/api/repo/getRepoSnapshots     /api/repo/diffRepoSnapshots
/api/repo/checkoutRepo         /api/repo/rollbackRepoSnapshotFile
```

普通 push 当前只创建文档历史，不自动创建仓库快照。仓库快照更适合批量或高风险 Commit 的显式检查点。

`getRepoSnapshots` 返回 Repository Snapshot ID、备注、时间、文件数、总大小、设备和标签等摘要；为避免响应过大，不直接返回完整文件列表。`diffRepoSnapshots(left, right)` 再按文件路径和秒级更新时间返回文件级变化：

| 字段 | 含义 | 条目来自 |
| --- | --- | --- |
| `addsLeft` | `right` 中新增的文件 | `right` |
| `updatesLeft` | 更新文件的旧版本 | `left` |
| `updatesRight` | 更新文件的新版本 | `right` |
| `removesRight` | `right` 中已删除的文件 | `left` |

条目中的 `fileID` 是数据仓库 File Object ID，不是 SiYuan Block ID。该 diff 没有 Block ID、插入锚点、内容前置条件或操作顺序，不能替代 Patch；未来可以用它检查一次批量 push 实际影响的文件集合是否超出 Commit 预期。

状态机不会自动调用高影响的 `checkoutRepo`。恢复必须由用户确认目标快照和影响范围后显式执行，再重新 pull。

## 当前实现边界

- pull 的 refs 转换是原子的，多文件物化则通过 WorkingTreeSnapshot、内容前置条件和 PullOperationState journal 实现可重放收敛；底层文件系统仍不提供跨多个 Markdown、metadata 和 `state.json` 的共同原子替换。
- 多文档 push 没有 Kernel 级跨文档原子性；实现通过 DocumentPatch 子状态、历史检查点、canonical 回读和已完成前缀保存完成范围，再用 `pull` 对账和 `push --continue` 收敛，不能整体回滚已经验证的前缀。
- 标准 Kernel 没有条件 mutation 或外部写租约。稳定 preflight 能在任何副作用前发现已经存在的冲突，操作前复检能缩小竞争窗口，但其他客户端仍可能在最后一次检查与无条件 mutation 之间写入同一对象。
- `remote-missing` 只根据 inventory 缺失判断，无法区分真正删除、notebook 关闭和映射范围变化；后续需要 tombstone/out-of-scope 和显式 untrack 模型。
- 同一位置存在无法整体匹配的并发 Insert 时仍会进入冲突。若要自动区分，需要给远端内容附加可回读的临时 `operationId`，或者让 Kernel 接受请求幂等键，并把相同幂等键的重复请求视为同一次操作。
- 后续故障注入还应继续覆盖 refs 转换、响应丢失、同锚点并发 Insert，以及 Markdown、metadata、`state.json` 各写入边界的磁盘失败。

## 术语与相关文档

### ID 术语

| 名称 | 含义 |
| --- | --- |
| SiYuan Block ID | 由 SiYuan Kernel 分配的块标识，下文简称 Block ID；文档本身也是根 Block，因此 Document ID 指文档根 Block ID |
| provisional ID | 本地新增内容在 push 前使用的临时块标识，不是 SiYuan Block ID |
| Object ID | `siyuan-worktree` 内容寻址对象的 SHA-256 标识 |
| `operationId` | `siyuan-worktree` 为单个 Patch 操作生成的本地稳定标识，不会发送给 Kernel |
| Repository Snapshot ID | SiYuan 数据仓库快照的标识，与 Block ID 无关 |

### 相关文档

- [同步状态机模型](synchronization-state-machine.md)：同步语义、状态关系、设计决策和能力边界。
- [本地映射格式](local-mapping.md)：Markdown 映射、块标记、内部目录和元数据侧车。
- [配置说明](configuration.md)：命令行配置、配置文件字段和修改注意事项。

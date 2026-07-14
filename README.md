# siyuan-worktree

`siyuan-worktree` 将 SiYuan 的 notebook/doc/subdoc 树映射为普通的本地目录和 Markdown，方便编辑器、脚本和 Agent 读取与修改；确认修改后，再通过 SiYuan Kernel API 安全写回。

项目只提供一个命令行程序，不再运行额外的后台服务。SiYuan Kernel 本身就是常驻服务：

```text
Editor / Agent
      |
      v
Local Markdown working tree
      |
      v
siyuan-worktree CLI
      |
      v
SiYuan Kernel HTTP API
```

## 核心原则

- SiYuan Kernel 始终是权威数据源，类似 Git 中的主分支。
- 本地 Markdown 是可自由编辑的 Working Tree，不会直接覆盖 Kernel 数据。
- Object Store 分别保存当前基线（Baseline）和最近一次已接受的远端跟踪快照（Remote Tracking），以及当前 Index、待 push Commit 和 Operation 所需的临时快照；稳定且无冲突时二者指向同一 WorkspaceTree。HEAD/Index/IndexPatch/Remote/Operation refs 原子指向这些对象；其中 `refs.remote` 不是实时 Kernel 的别名。
- 项目借用 Git 的状态面、快照和原子 ref 模型，但不维护长期 Commit 历史且无分支管理功能；稳定同步后旧快照和事务对象会进入 best-effort GC 回收范围。
- `add` 同时生成 Index WorkspaceTree 与暂存 Patch，`commit` 冻结完整快照并引用对应 Patch；一个 Commit 只有一个 Patch，Patch 内按文档组织块级操作。
- `push` 只调用块级 Kernel API，不使用 `updateBlock(documentID)` 重写整篇文档。
- `push` 重新读取受影响文档的 Kernel 规范化结果，生成 canonical Result Snapshot 后，才会物化 Markdown 和元数据并统一推进 refs；该结果不表示整个 Kernel 在同一全局时刻的快照。
- 无法确定操作安全性时拒绝写入，由用户检查并解决冲突。

本地流程与 Git 类似：

```text
Working Tree -> add -> Index -> commit -> Local Commit -> push -> SiYuan Kernel
```

这里的 Commit 是 `siyuan-worktree` 自己保存的待同步记录，不是 Git commit。

## 当前能力

| 能力 | 状态 |
| --- | --- |
| notebook/doc/subdoc 映射为目录和 Markdown | 支持 |
| pull/add/commit/push 使用内容寻址快照和原子 refs | 支持 |
| 仅保留当前 Baseline、Remote Tracking 和当前事务对象 | 支持，通过 refs 可达性 GC 回收旧对象 |
| staged Patch 与 Commit Snapshot 相互校验 | 支持 |
| pull 开始时冻结 Working Tree，并记录多文件物化进度 | 支持，通过 WorkingTreeSnapshot 与 PullOperationState 恢复 |
| pull/普通 status 连续稳定读取远端 | 支持，完整映射范围连续两轮版本集合 + 每篇文档结构前后复核；持续变化时安全停止 |
| push 进度写入不可变 OperationState | 支持，通过 `refs.operation` 恢复 |
| push 在任何历史或 mutation 前预检全部待修改文档 | 支持，预检快照写入 PushOperationState |
| push 前创建并搜索 SiYuan 文档历史 | 支持，记录为 `verified` 或 `unverified` |
| 修改已有顶层块 | 支持，生成 `update` 操作 |
| 在文档开头、中间或末尾新增顶层内容 | 支持，生成 `insert` 操作 |
| 删除已有顶层块 | 支持，由具体 Commit 批准 |
| 本地和远端修改不同顶层块时自动 rebase | 支持 |
| 同一块被双方修改时保存三方冲突快照 | 支持 |
| 块属性保护 | 支持；属性保持只读，Update 前后验证，发现变化时停止而不主动覆盖 |
| pull/push 后刷新 SiYuan Block ID 和元数据侧车 | 支持 |
| 顶层块移动或重排 | 暂不支持，安全拒绝 |
| 本地新建、删除、改名或移动文档文件 | 暂不映射为 SiYuan 文档树操作 |
| 修改属性视图、嵌入查询等复杂块 | 暂不支持，保持只读 |

## 命令

| 命令 | 用途 |
| --- | --- |
| `clone URL [DIRECTORY]` | 从 SiYuan Kernel URL 创建工作树并完成首次 pull |
| `init` | 初始化本地工作树 |
| `pull` | 稳定观察 SiYuan（fetch），再合并或重放本地变化（merge/rebase）；存在活动 push 时只读取远端并对账已执行前缀与剩余意图 |
| `status` | 比较最近远端跟踪快照、Working Tree 和本次稳定读取的 SiYuan 状态；若 pull/push 尚未完成，则像 Git sequencer 一样显示阶段、进度和下一条恢复命令 |
| `diff` | 查看 Working Tree 相对 Index Snapshot 的变化 |
| `diff --staged` | 查看已暂存的变化 |
| `add` | 将指定工作区变化加入 Index |
| `commit` | 将 Index 冻结为本地 Commit |
| `reset` | 取消暂存，并放弃尚未 push 的 Commit |
| `restore --ours/--theirs` | 选择冲突中的本地或 SiYuan 版本 |
| `log` | 查看当前等待 push 的 Commit；干净状态返回空列表 |
| `push` | 将已 Commit 的 Patch 应用到 SiYuan；存在活动事务时自动从已证明进度继续 |
| `push --continue` | 显式继续活动 push；没有活动 push 时拒绝执行 |
| `version` | 查看版本 |

命令集刻意与 Git 保持一致。安全校验由 `add` 完成；Working Tree、Index 和当前待 push Commit 分别通过 `diff`、`diff --staged` 和 `log` 查看；冲突包含在 `status` 中。`log` 不是长期版本历史。

## 快速开始

### 1. 构建

要求 Go 1.24 或更高版本。

```bash
go build -buildvcs=false -o siyuan-worktree ./cmd/siyuan-worktree
go test ./...
```

Windows：

```powershell
go build -buildvcs=false -o siyuan-worktree.exe ./cmd/siyuan-worktree
go test ./...
```

### 2. Clone SiYuan

确保 SiYuan 正在运行，并先设置 API Token。Token 不会写入配置文件：

```bash
export SIYUAN_TOKEN='Settings -> About 中的 API Token'
```

PowerShell：

```powershell
$env:SIYUAN_TOKEN = 'Settings -> About 中的 API Token'
```

像 `git clone` 一样直接传入 SiYuan Kernel URL：

```bash
siyuan-worktree clone http://127.0.0.1:6806 my-siyuan-worktree
cd my-siyuan-worktree
```

如果省略目录名，CLI 会根据 URL 生成目录名，例如 `127.0.0.1-6806`。

默认映射所有已打开的 notebook。可以重复传入 `-notebook` 只选择指定 notebook；选项要写在 URL 前：

```bash
siyuan-worktree clone \
  -notebook 20210817205410-2kvfpfn \
  -notebook 20210808180117-czj9bvb \
  http://127.0.0.1:6806 \
  my-siyuan-worktree
```

`clone` 相当于初始化配置并执行第一次 `pull`。如果需要创建配置但暂不连接 Kernel，也可以使用：

```bash
siyuan-worktree init -endpoint http://127.0.0.1:6806 my-siyuan-worktree
```

### 3. 拉取

```bash
siyuan-worktree pull
```

pull 开始时会冻结一次本地工作副本，并记录多文件更新计划。若进程在更新文件期间中断，再次执行同一命令会从已记录进度继续；若编辑器在计划生成后修改了尚待处理的文件，pull 会停止而不会覆盖该编辑。

Kernel 的多个读取 API 不构成原子查询。`pull` 会先通过读取屏障等待调用前已经排队的 transaction 完成，再连续采集整个映射范围：每轮都读取 inventory，并为每篇文档采集块关系、Kramdown 和属性，文档结构在单轮采集前后还必须一致。只有连续两轮得到相同的 inventory 和完整文档版本集合，结果才会被接受。若 SiYuan 正在持续编辑，命令会重试；仍无法稳定读取时返回 `remote unstable`，不会物化尚未通过验证的结果。这是乐观稳定性检查，不是 Kernel 全局锁或线性一致快照。

命令默认在当前目录读取 `.siyuan-worktree/config.json`。如果不想切换目录，仍可使用 `-root DIR`。

### 4. 编辑、暂存和提交

编辑 Markdown 后查看状态和差异：

```bash
siyuan-worktree status
siyuan-worktree diff
```

如果 pull 或 push 曾经中断，`status.activeOperation` 会显示当前事务的 `phase`、已完成文档数、当前文档、错误或冲突，以及 `nextAction`。为保证 SiYuan 离线或持续变化时仍能查看恢复步骤，活动事务中的 `status` 只读取本地 sequencer 证据，不开始新的远端观察；此时 `documentComparisonDeferred` 为 `true`，`documents` 暂时为空。活动 push 对账前提示 `pull`，对账完成后提示 `push --continue`。

暂存全部已跟踪文档的变化：

```bash
siyuan-worktree add -A
siyuan-worktree diff --staged
```

也可以只暂存指定文档或目录：

```bash
siyuan-worktree add notes/工作笔记/项目规划/技术设计.md
```

创建本地 Commit。该命令不会写入 SiYuan：

```bash
siyuan-worktree commit -m "更新设计文档"
siyuan-worktree log
```

`add` 负责构建 Index Snapshot、Patch 和安全校验。和 Git 一样，`commit` 冻结的是 Index；`add` 后继续编辑只会形成新的 Working Tree 变化，不会改变已经暂存的内容。删除操作会明确出现在 staged diff 和 Commit 中，不需要额外的删除开关。

### 5. 推送到 SiYuan

```bash
siyuan-worktree push
```

`push` 只应用待 push Commit 中经过快照校验的 Patch，不会隐式包含未暂存内容。当前 Commit 后如果继续编辑对应文件，`push` 会拒绝，避免 Kernel canonical 回读覆盖新的 Working Tree 修改；Commit 本身仍然有效，可以 reset 后重新组织后续修改。

在创建第一篇文档历史或执行任何块写入前，`push` 会先稳定读取并预检 Commit 涉及的全部文档。这样后续文档中原本就存在的冲突不会等到前面文档已经写入后才被发现。每个操作执行前仍会再次检查目标 Block 或插入位置，写入后再稳定回读。

当前安全实现一次只允许一个待 push Commit。完成 push 或 reset 后，才能继续创建下一个 Commit。

push 成功后，受影响文档的 Kernel 规范化回读结果会更新本地 Baseline 和 Remote Tracking；已完成的用户 Commit、OperationState 和被替代的本地快照变为不可达并进入 best-effort GC 回收范围，因此随后执行 `log` 返回空列表。长期恢复由 SiYuan 文档历史负责；数据仓库快照可作为显式的高风险操作保护层，但普通 push 当前不会自动创建它。

如果多文档 push 只完成了前面一部分，或者某个 Update、Delete、Insert 的返回结果尚未持久化，先执行：

```bash
siyuan-worktree pull
```

此时 `pull` 仍遵循 `fetch + merge/rebase`：先读取 SiYuan 当前状态，再把已保存的执行证据、已经完成的文档前缀和剩余待 push 意图与远端对齐。这个过程不会产生新的 Kernel 写入：

- `pushReconciliation: "ready"`：已完成前缀有效，剩余 Patch 可以在当前远端上继续或安全 rebase。
- `pushReconciliation: "applied"`：当前 in-flight 操作的目标状态已经存在，记录为完成并跳过重复写入。
- `pushReconciliation: "not-applied"`：远端仍是操作前状态，清除 in-flight 后可以安全重试。
- `pushReconciliation: "conflict"`：远端既不是可信前态也不是目标结果，或者 Insert 无法唯一对应，需要检查冲突材料。

对账成功后运行：

```bash
siyuan-worktree push --continue
```

普通 `push` 在存在活动事务时也会继续执行；`--continue` 用于明确表达恢复意图。已经验证完成的文档不会再次写入，剩余文档按原 Commit 顺序继续，安全的远端无关变化会被吸收到最终 canonical 结果中。

### 6. 放弃暂存或待推送状态

```bash
siyuan-worktree reset
```

`reset`：

- 不修改 Working Tree 中的 Markdown。
- 取消暂存，使 Index 回到 HEAD 所指向的 Baseline。
- 丢弃待 push Commit 和 OperationState；相关对象变为不可达并进入 best-effort GC 回收范围。

如果 OperationState 表明 push 已经或可能执行文档内容 mutation，普通 reset 会保留恢复证据并拒绝；应优先执行 `pull` 对账，再通过 `push --continue` 收敛。只有在确认远端状态后，才使用：

```bash
siyuan-worktree reset --force
```

`reset --force` 只丢弃本地 Commit 与 Operation 恢复证据，不会撤销 Kernel 已完成的 mutation，也不会还原 Working Tree。执行后必须重新运行 `status` 和 `pull`，确认并建立新的可信基线。

## 详细文档

- [本地映射格式](docs/local-mapping.md)：目录结构、块标记、新增内容和内部元数据。
- [同步状态机模型](docs/synchronization-state-machine.md)：基线、工作副本、暂存、提交意图、Patch、pull/push 状态转换和冲突恢复。
- [同步机制实现说明](docs/synchronization-implementation.md)：内容寻址对象、原子状态更新、SiYuan Kernel 接口和中断恢复实现。
- [配置说明](docs/configuration.md)：命令行配置方式、内部字段和高级修改注意事项。

## 已知限制

- 本地映射目录采用单写者假设：pull/push 物化文件期间，不支持编辑器、网盘同步或其他程序同时写入映射的 Markdown；检测到变化时命令会停止。
- 普通 `status` 和 `pull` 都需要遍历选中的 notebook，超大知识库后续需要增量优化；活动事务中的 `status` 只读取本地 sequencer 状态，不重新遍历远端。
- 顶层块 move/重排尚未开放，不会自动转换成 delete+insert。
- 本地 Markdown 文件的新建、删除、改名和移动暂不映射为 SiYuan 文档树操作。
- 属性视图、嵌入查询等复杂块保持只读。
- 本地新增内容只有在 Kernel 插入并完成规范化回读后，才拥有正式的 SiYuan Block ID；push 前不能引用尚未在 SiYuan 中创建的块。
- SiYuan 没有供外部客户端持有的临时全局写锁，也没有带 expected hash/revision 的条件块写接口。编辑器只读设置只限制官方 UI，transaction flush 只是读取屏障，`/api/transactions` 也只在执行单笔事务时串行 mutation。因此 push 会通过稳定预检、操作前复检和写后回读发现大部分并发变化，但其他客户端仍可能恰好在最后一次检查与 Kernel 无条件写入之间修改同一对象。
- Remote Tracking 当前持久化映射 inventory 和文档内容树；完整 Kernel metadata 仍是可重建的侧车 projection，尚未全部进入内容寻址基线。
- Kernel 的混合块操作没有整体事务；多文档 push 通过已完成前缀、逐操作对账和 `push --continue` 收敛。Insert 响应丢失时仍只能在锚点和内容能够唯一匹配时自动判定，否则需要人工解决冲突。
- pull 使用 WorkingTreeSnapshot 和 PullOperationState 记录多文件物化进度，但文件系统仍不能把所有 Markdown、metadata 和内部状态作为一个共同的原子替换；中断后通过 journal 重放并收敛。
- remote-missing 尚未用 tombstone 区分真正删除、notebook 关闭和映射范围变化。
- v3 内部同步状态（`state.json`、对象与 refs）不兼容早期工作树；遇到版本不支持或内部状态无效错误时应重新 `clone`。`config.json` 的独立配置 schema 仍为 v1。
- Linux/macOS 上 refs 更新包含文件和父目录同步；Windows 可正常构建和运行，但平台不提供同等级的目录 `fsync` 崩溃耐久保证。

# 本地映射格式

本文说明 SiYuan notebook/doc/subdoc 在本地工作树中的目录结构、块标记格式和内部元数据目录。

## 文档树映射

首次 pull 后的工作树类似：

```text
my-siyuan-worktree/
├── .siyuan-worktree/
└── notes/
    ├── 工作笔记/
    │   ├── 项目规划/
    │   │   ├── _index.md
    │   │   ├── 需求分析.md
    │   │   └── 技术设计.md
    │   └── 每日笔记.md
    └── 知识库/
        └── LLM.md
```

- notebook 映射为顶层目录。
- doc/subdoc 映射为 Markdown 文件和子目录。
- 有子文档的文档正文映射为 `文档名/_index.md`。
- 同一层级的重名文档会附加文档根块的 SiYuan Block ID 后缀。

## 已有块

已有顶层块使用隐藏注释标记边界，并在内容中保留 SiYuan IAL：

```markdown
<!-- siyuan-worktree:block 20260714120200-hijklmn type=p -->
正文内容。
{: id="20260714120200-hijklmn"}
<!-- /siyuan-worktree:block 20260714120200-hijklmn -->
```

边界注释用于生成文档 Patch 中的块级操作，IAL 中的 `id` 保存 SiYuan Block ID，其他字段保存引用和属性。不要让 Markdown 格式化器删除、伪造或重排这些标记。第一阶段所有 IAL 属性保持只读；本地修改已有 Block 的属性会在 `add` 时被拒绝，属性书写顺序变化不算修改。

## 新增块

新增内容直接写普通 Markdown，不要伪造 SiYuan Block ID 或边界标记：

```markdown
<!-- siyuan-worktree:block 20260714120200-hijklmn type=p -->
已有内容。
{: id="20260714120200-hijklmn"}
<!-- /siyuan-worktree:block 20260714120200-hijklmn -->

这是本地新增的 Markdown，暂时没有 SiYuan Block ID。
```

`add` 会把未标记区域转换为文档 Patch 中的 `insert` 操作。push 时，SiYuan Kernel 会创建对应 Block 并分配正式的 SiYuan Block ID；成功后 CLI 会重新读取规范化结果，用该 Block ID 更新 Markdown 和元数据侧车。

## 工作树内部目录

```text
my-siyuan-worktree/
├── .siyuan-worktree/
│   ├── config.json
│   ├── state.json
│   ├── lock
│   ├── objects/sha256/
│   ├── refs/state.json
│   ├── meta/documents/
│   └── conflicts/
└── notes/
```

`lock` 只在命令执行期间存在，用于阻止两个 CLI 进程并发修改同一个工作树。它不限制 SiYuan GUI、插件、同步任务或其他 API 客户端写入 Kernel。如果进程崩溃留下锁文件，并且确认没有命令仍在运行，可以手工删除该文件。下一次成功取得工作树锁时，CLI 会检查 `refs/state.json.lock`：generation 连续、对象完整且状态不变量成立时完成提交，否则丢弃无效或半写文件。

`objects/sha256/` 是不可变的内容寻址对象数据库，保存 BlockSnapshot、DocumentTree、WorkspaceTree、WorkingFileContent、WorkingTreeSnapshot、Patch、CommitObject，以及 pull/push 的 OperationState。`refs/state.json` 使用 generation 原子记录 HEAD、Index、IndexPatch、最近一次经过重复读取验证并接受的远端跟踪快照（Remote Tracking）和当前 OperationState。`refs.remote` 不是实时 Kernel 的别名；普通 `status` 和 `pull` 会重新建立临时远端观察（Observed Remote）并与它比较，活动事务中的 `status` 则只读取本地恢复状态。

WorkingTreeSnapshot 是一次稳定读取本地 Markdown 得到的短期快照。每条记录包含文档根 Block ID、路径、文件是否缺失、大小、修改时间、内容哈希和 WorkingFileContent 引用。相同 Markdown 会复用同一个内容对象；快照只在当前 pull 恢复期间保持可达，不形成长期工作副本历史。

对象数据库不保存长期版本历史。状态转换完成后，CLI 按 refs 可达性保留当前 Baseline、最近一次接受的 Remote Tracking 和当前事务对象；旧远端跟踪快照、已完成 Commit、旧 OperationState、临时 WorkingTreeSnapshot 与其他不可达对象进入 best-effort GC 回收范围。Index、待 push Commit 和 pull/push journal 都直接从对象与 refs 恢复，不再维护重复的可变缓存目录。

Remote Tracking 当前持久化的是映射 inventory 和文档内容树。`meta/documents/` 中的完整 Kernel metadata 是从稳定观察生成的可重建 projection，尚未作为独立内容寻址对象进入 Baseline/Remote Tracking 哈希。

文档元数据侧车位于：

```text
.siyuan-worktree/meta/documents/<document-id>.json
```

其中记录文档属性和已知块的 `id/type/subType/parentId/previousId/hash/attrs`；`id`、`parentId` 和 `previousId` 均表示相应的 SiYuan Block ID。

`state.json` 保存文档根 Block ID 到本地路径等物化信息；`meta/documents/` 保存 Kernel 块属性侧车；`conflicts/` 保存当前三方冲突材料。它们不是版本历史。

除 `notes/` 中映射出的 Markdown 外，`.siyuan-worktree/` 中的文件都是同步层内部状态，不应手工编辑。对象文件和 refs 使用受限权限，并通过同步临时文件和 rename 写入。当前内部同步状态（`state.json`、对象与 refs）为 v3；早期工作树必须重新 `clone`，不会由新代码静默迁移。`config.json` 使用独立的 v1 配置 schema。

对象哈希、refs 原子更新和 GC 的实现细节见[同步机制实现说明](synchronization-implementation.md#对象存储与引用)。

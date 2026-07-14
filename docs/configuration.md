# 配置说明

`siyuan-worktree` 的配置方式参考 Git：普通用户通过命令创建和使用工作树，不需要直接维护内部配置文件。

## 推荐方式

使用 `clone` 创建工作树：

```bash
siyuan-worktree clone http://127.0.0.1:6806 my-siyuan-worktree
```

`clone` 会自动：

1. 创建工作树目录。
2. 生成 `.siyuan-worktree/config.json`。
3. 连接指定的 SiYuan Kernel。
4. 执行第一次 `pull`。

如果只想初始化配置而不立即连接 Kernel，可以使用：

```bash
siyuan-worktree init -endpoint http://127.0.0.1:6806 my-siyuan-worktree
```

## 命令行选项

`clone` 支持：

| 选项 | 含义 |
| --- | --- |
| `-notebook NOTEBOOK_ID` | 只映射指定的 SiYuan notebook ID，可以重复使用 |
| `-output DIR` | 设置工作树中的 Markdown 输出目录，默认为 `notes` |
| `-token-env NAME` | 设置读取 API Token 的环境变量名，默认为 `SIYUAN_TOKEN` |

`init` 还支持：

| 选项 | 含义 |
| --- | --- |
| `-endpoint URL` | 设置 SiYuan Kernel URL，默认为 `http://127.0.0.1:6806` |

例如：

```bash
siyuan-worktree clone \
  -notebook 20210817205410-2kvfpfn \
  -output notes \
  -token-env SIYUAN_TOKEN \
  http://127.0.0.1:6806 \
  my-siyuan-worktree
```

API Token 本身不会写入配置文件，应通过对应环境变量提供：

```bash
export SIYUAN_TOKEN='your-token'
```

PowerShell：

```powershell
$env:SIYUAN_TOKEN = 'your-token'
```

## 内部配置文件

配置保存在：

```text
.siyuan-worktree/config.json
```

当前格式如下：

这里的 `version: 1` 只表示配置 schema；它与内部同步状态使用的 v3 `state.json`、对象和 refs 相互独立。

```json
{
  "version": 1,
  "endpoint": "http://127.0.0.1:6806",
  "tokenEnv": "SIYUAN_TOKEN",
  "notebookIds": [],
  "outputDir": "notes"
}
```

字段含义：

| 字段 | 含义 |
| --- | --- |
| `version` | 配置格式版本，由程序管理 |
| `endpoint` | SiYuan Kernel HTTP 地址 |
| `tokenEnv` | 保存 API Token 的环境变量名 |
| `notebookIds` | 需要映射的 SiYuan notebook ID；空数组表示所有已打开的 notebook |
| `outputDir` | Markdown 输出目录，必须是工作树内的相对路径 |

## 手工修改注意事项

配置通常不应手工修改。如果确实需要修改：

- 确认没有其他 `siyuan-worktree` 命令正在操作该工作树。
- 不要修改 `version`。
- 不要把 API Token 直接写入配置。
- `endpoint` 必须是完整的 HTTP 或 HTTPS URL，不能包含 query 或 fragment。
- `outputDir` 必须位于工作树内部。
- 已经执行过 pull 后，不要直接修改 `outputDir`；当前版本不会自动迁移已有 Markdown 和内部映射。
- 扩大 `notebookIds` 范围后应执行 `pull`。当前缩小范围不会自动解除已有文档跟踪，而会把它们报告为 `remote-missing`；在 untrack/out-of-scope 模型完成前，建议为新的范围重新 `clone`。

除 `config.json` 外，`.siyuan-worktree/` 中的内容均由程序维护，不应手工编辑。内部目录说明见 [本地映射格式](local-mapping.md)。

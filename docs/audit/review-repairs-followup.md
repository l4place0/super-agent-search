# 恢复保护补测与消融复验

针对[首轮实验](review-repairs-ablation.md)的四个存活变异，增加恢复边界用例：

- inspect 返回空列表、多个卷、错误卷名时，拒绝恢复且不停止/移除当前容器。
- `search.db`、`master.key`、`credentials.json` 分别缺失或为空，在预检查阶段拒绝恢复，保持容器和数据。
- 模拟预检查通过后备份失效：同样的三文件、缺失/为空组合，在最终恢复阶段拒绝清空目标目录。
- 有效快照正例必须替换原有占位数据并完整复制三份文件，防止“始终报错”的 mock 使负例假通过。

新增 16 个场景（3 个异常 inspect 响应、12 个文件状态/时序组合、1 个完整恢复正例）。文件用例通过 `recovery_shell_helper.py` 在临时目录构造实际文件，执行被测恢复函数生成的原始 shell 命令。只映射 `/backup` 与 `/target` 到测试临时目录，不连接 Docker 或真实数据卷。早期/晚期时序由两个 shell 调用分别构造对应文件状态，未测试真实 Docker 挂载与并发改卷。

Windows PowerShell 5.1、Windows PowerShell 7 和 Linux PowerShell 7 使用同一套恢复断言；文件沙箱要求 Python 3 与 sh。全矩阵复验仍执行原来的七条离线测试链，包含 Go race、两种 PowerShell mock（Windows 5.1 含 DPAPI）、Node、sh 语法、Go vet 和 Python 单元测试。本轮两个临时副本可并行；前后基线串行，报告逐项保留哈希、日志和失败归因。

复现（WSL，依赖准备同首轮）：

```sh
PATH=/tmp/uas-experiment-pwsh-7.5.4:$PATH python3 scripts/ablate_review_repairs.py \
  --report docs/audit/review-repairs-followup.json \
  --scratch-root /mnt/c/Users/19065/AppData/Local/Temp --workers 2
```

## 结果

补测后完成 **17 个版本 × 7 条完整离线测试链 = 119 条阶段记录**。14 个有害变异全部检出，零存活；前后完整基线与行为等价负对照均通过。没有跳过、运行器错误、编译失败、超时或无关失败计入检出。两个基线的 Go race 测试均有 123 个 pass 事件（含父/子测试），无 skip。

| 上轮存活项 | 首轮 | 补测后 | 新增见证 |
| --- | --- | --- | --- |
| 卷名/响应数量判断 | 存活 | 检出 | 异常 inspect 响应被接受 |
| 清空前文件复检 | 存活 | 检出 | 预检查有效、恢复时备份失效仍被接受 |
| master.key 检查 | 存活 | 检出 | 缺失或为空的主密钥未被拒绝 |
| credentials.json 检查 | 存活 | 检出 | 缺失或为空的凭据文件未被拒绝 |

四项均由 **Windows PowerShell 5.1 和 Linux PowerShell 7 两条测试链分别检出**，对应 `Invalid backup accepted:` 断言；不是由无关测试失败归因。未移除保护时，全部早期/晚期文件场景和有效恢复正例通过。

### 完整矩阵

| 版本 | 结果 |
| --- | --- |
| `baseline_before` | 通过 |
| `without_restore` | 检出 |
| `without_dedupe` | 检出 |
| `without_hidden` | 检出 |
| `without_restore_dedupe` | 检出 |
| `without_restore_hidden` | 检出 |
| `without_dedupe_hidden` | 检出 |
| `without_restore_dedupe_hidden` | 检出 |
| `without_inspect` | 检出 |
| `without_volume_identity` | 检出 |
| `without_preflight` | 检出 |
| `without_recheck` | 检出 |
| `without_file_db` | 检出 |
| `without_file_master` | 检出 |
| `without_file_credentials` | 检出 |
| `negative_control` | 通过 |
| `baseline_after` | 通过 |

[完整 JSON](review-repairs-followup.json) 和[原始日志压缩包](review-repairs-followup-logs.zip)记录每个版本的测试链、失败用例、归因和哈希。实验完成后已核对生产修复文件、Go 回归测试、PowerShell mock、文件沙箱 helper 及实验运行器与开始快照的 SHA-256 一致。

### 边界

这是有限消融清单的一轮复验，支持“新增测试能辨认此前四个覆盖空缺”的结论，不等于穷尽所有程序变异。文件缺失/为空不涵盖 SQLite 损坏、密钥与数据库不匹配、真实 Docker 挂载、真实磁盘故障或并发改卷；未执行线上恢复演练。

# 本轮三个审查修复的全量消融实验

## 范围

实验对象是 HEAD `6fd8ee2` 上的未提交修复，包含 `review_regression_test.go` 和新增恢复断言；JSON 保存所有输入文件的 SHA-256，不能把结果归给未包含修复的 HEAD 提交。

全量指三个修复的全部 2³ 种开关组合（含完整基线），另外单独消融备份卷 inspect、返回卷身份判断、预检查、删除前复检，以及数据库、主密钥、凭据三个文件检查。加一个行为等价负对照和结束基线，共 17 次完整测试链。有限清单不等于所有代码变异或线上恢复演练。

每个版本均从同一个工作区快照复制到独立临时目录，所有变异仅发生在副本中。每个版本执行：

1. `go test -race -json -count=1 -timeout=150s ./...`，禁止将 skip、编译或超时计为检出。
2. Windows PowerShell 5.1 完整 WSLC mock（含 DPAPI 往返）。
3. Linux PowerShell 7 完整 WSLC mock。
4. Node 全部 DOM/API 合约测试。
5. sh 客户端语法检查。
6. `go vet ./...`。
7. Python 全部 `test_*.py` 单元测试。

Go 变异归因要求 `TestReplacementCredentialUniqueness` 或 `TestReadHTMLHiddenAttributes` 失败；恢复变异归因要求新增 Invalid backup 断言失败。无关失败与运行器错误另列，不算检出。每个版本一次运行，无性能或统计显著性结论。

## 运行环境与复现

Go 1.22.2 / Linux amd64，Linux PowerShell 7.5.4，Windows PowerShell 5.1。Windows 子进程显式使用其自身模块目录，避免 WSL/PowerShell 7 模块路径干扰。准备 Go、gcc、node、jq、curl、sh、Windows PowerShell 及 Linux pwsh 后，在 WSL 仓库目录执行：

```sh
PATH=/tmp/uas-experiment-pwsh-7.5.4:$PATH python3 scripts/ablate_review_repairs.py \
  --report docs/audit/review-repairs-ablation.json \
  --scratch-root /mnt/c/Users/19065/AppData/Local/Temp
```

[原始 JSON](review-repairs-ablation.json) 保存完整矩阵与每条测试链状态；同名目录保存每条命令的完整日志。

## 结果

已完成 **17 次完整测试链**（每次 7 条，共 119 条阶段记录）：14 个有害变异中 **10 个检出、4 个存活**；前后两个完整基线和一个行为等价负对照全部通过。没有 skip、编译失败、超时或无关失败计入检出。测试阶段累计耗时约 19.6 分钟。

三个修复宏观开关的 7 个非基线组合全部检出，完整基线的 Go race 测试各有 123 个 pass 事件（包含父/子测试）。这说明新增测试能够辨认三个原始缺陷，但恢复流程仍有四个组件级覆盖空缺。

| 版本 | 结果 | 归因 |
| --- | --- | --- |
| 开始完整基线 | 通过（存活） | — |
| 移除整套恢复保护 | 检出 | Invalid backup accepted: |
| 移除凭据查重 | 检出 | TestReplacementCredentialUniqueness |
| 恢复旧 hidden 判断 | 检出 | TestReadHTMLHiddenAttributes |
| 恢复保护＋查重 | 检出 | Invalid backup accepted:；TestReplacementCredentialUniqueness |
| 恢复保护＋hidden | 检出 | Invalid backup accepted:；TestReadHTMLHiddenAttributes |
| 查重＋hidden | 检出 | TestReadHTMLHiddenAttributes；TestReplacementCredentialUniqueness |
| 三个修复全部移除 | 检出 | Invalid backup accepted:；TestReadHTMLHiddenAttributes；TestReplacementCredentialUniqueness |
| 移除卷 inspect 与响应检查 | 检出 | Invalid backup accepted: |
| 移除卷名/响应数量判断 | 通过（存活） | — |
| 移除恢复前文件检查 | 检出 | Invalid backup touched current deployment |
| 移除清空前文件复检 | 通过（存活） | — |
| 移除 search.db 检查 | 检出 | Invalid backup accepted: |
| 移除 master.key 检查 | 通过（存活） | — |
| 移除 credentials.json 检查 | 通过（存活） | — |
| 等价比较顺序负对照 | 通过（存活） | — |
| 结束完整基线 | 通过（存活） | — |

### 存活变异的含义

- `without_volume_identity`：mock 只返回匹配的卷名，未覆盖空响应、多个响应或错误卷名。
- `without_recheck`：mock 未模拟预检查通过后、清空目标数据前备份发生变化。仅预检查还足以通过现有测试，不能据此删除复检。
- `without_file_master` 与 `without_file_credentials`：现有错误注入仅针对 `test -s /backup/search.db`，未分别模拟缺失/空主密钥及凭据文件。

这些存活项表明测试覆盖不足，不是相关保护无用。数据库检查的检出也限于命令字符串故障注入；本轮没有真实文件系统或 Docker 卷恢复演练，不能由 mock 通过推出真实快照完整性。

### 归因和限制

本轮只覆盖列出的有限组件清单与全部三个宏观开关组合；未单独消融事务原子性、并发序列或所有可能的错误分支。每个版本一次运行，未做性能/检索质量比较。

运行前两次基线准备曾因 Windows 子进程的 PowerShell 模块路径受到环境影响而失败，未启动有害变异、不计入最终矩阵。正式运行显式把 Windows PowerShell 模块路径设为其 `$PSHOME/Modules`，前后基线均通过。

[完整日志压缩包](review-repairs-ablation-logs.zip) 保留每条阶段的输出；JSON 保存变异文件哈希、失败用例、见证断言和零跳过记录。实验结束已确认四个被测源文件与开始快照的 SHA-256 一致，消融未改动工作区修复代码。

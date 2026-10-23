# A1–A8 全清单、全测试链消融实验

## “全量”的准确范围

本轮将有限消融清单扩展到 **30 个版本：28 个有害变异 + 2 个行为等价负对照**，覆盖 A1–A8。**每个版本都执行全部离线测试链**，不使用定向 Go `-run`：

1. `go test -race -json -count=1 -timeout=150s ./...`，含 Linux SIGTERM 真实子进程、sh/pwsh 实际客户端 → mock HTTP、credential 格式测试。
2. Windows PowerShell 5.1 的完整 WSLC mock，含 DPAPI 往返。
3. Linux PowerShell 7 的完整 WSLC mock（Linux 不执行 Windows DPAPI 分支）。
4. Node 全部 DOM/API 合约测试。
5. sh 语法检查。

“全量”指上述**全清单 × 全测试链**，不是枚举所有可能的程序变异或 2^28 种开关组合，也不是完整安全认证。每个变异一次有效测量，环境抖动项单独复测；没有性能显著性或检索质量结论。

## 主轮结果

基线：[`0b7ac35`](https://github.com/l4place0/uni-agent-search/commit/0b7ac35)。[完整记录](ablation-full-0b7ac35.json)保存每个版本的五条测试链结果、Go 失败用例、变异后文件 SHA-256、负对照和复测历史。

| 编号 | 机制 | 有害变异数 | 主轮检出 |
| --- | --- | ---: | ---: |
| A1 | 成功不抹冷却、失败不缩短冷却 | 2 | 2 |
| A2 | release revision 条件、管理 revision 提升、lease 单次释放 | 3 | 3 |
| A3 | 写缓存代际校验、invalidate 提升 epoch | 2 | 2 |
| A4 | request gate 等待 handler、超时取消并关连接 | 2 | 2 |
| A5 | DB readiness、入口记录、Key 写失败停调度、panic 脱敏 | 4 | 4 |
| A6 | 实际挂载备份、实例身份、失败恢复、外来容器保护、ensure 不自动重建 | 5 | 5 |
| A7 | DNS 整组验证、IP 固定、DoH 上限、重定向预算、特殊用途 CIDR | 5 | **4** |
| A8 | Key 轮换、nonce 新鲜度、认证失败拒绝、链接协议、引用警告 | 5 | 5 |
| 合计 | | **28** | **27** |

两个等价负对照均通过完整测试链：

- 将相同失败策略复制给手动 Key 测试，取消逻辑复用。
- 将 KeyPool 文件的类型和函数内联回 store.go，保持行为相同。

这支持“测试能够区分行为退化与等价组织变化”，**不证明拆分模块会提高性能，也不否定其维护收益**。

主轮前后两个完整基线均通过；每次基线 Go JSON 中 118 个 pass 事件（包含父测试与子测试），无 skip。全清单执行完成，无未完成/编译错误/超时版本计入检出。

## 不把环境失败算作检出

最初两 worker 的长任务在完成 28/30 时达到工具 30 分钟上限。检查点保留后，剩余项和待归因项通过 `--resume --workers 1` 完成，最后再次执行完整基线。

部分版本伴随非目标测试失败：

- 原手动/搜索一致性测试比较两次动态 `Retry-After: 60` 的绝对时间，只容忍 1 秒。并发重负载下，创建第二个 app 的时间可能超过这个窗口。
- pwsh CLI 测试原有 20 秒进程预算，重负载下可能被测试进程超时终止。

例如 DoH 上限变异初次发生的是一致性/CLI 失败，**DoH 断言没有失败，不能记为 DoH 防线被检出**。记录保留了该次历史；串行跑完整测试链后，该变异全部通过，最终明确计为存活。

实验器现在同时记录“任何测试链失败”和“对应机制的 witness 失败”。编译错误、runner 故障、缺依赖导致的 skip、外部超时都标为无效/未完成，不算检出。witness 归因是在观察到非目标失败后强化的规则，并非预注册统计实验；[归因单元测试](../../scripts/test_ablate_full.py)防止将非目标失败再算成 kill。

## 唯一存活项：DoH 大小限制测试被解析错误掩盖

变异将 DoH 响应上限从 `64 KiB` 放宽到 `4 MiB`。旧 oversized fixture 是一大串非法 JSON；即使不触发原大小限制，后续 JSON 解析也会失败，所以测试仍然通过。

新增 [doh_limit_test.go](../../internal/app/doh_limit_test.go) 使用**合法 JSON**，精确隔离边界：

- 响应恰好 65,536 字节：必须接受。
- 响应 65,537 字节：必须由 size guard 拒绝。

补充提交：[`2b27afa`](https://github.com/l4place0/uni-agent-search/commit/2b27afa)。同时修正测试时间基准：Retry-After 使用共享绝对日期，500 冷却在各自请求的分类时间窗口内校验；CLI 测试进程预算改为 60 秒。**没有修改生产网络上限或供应商逻辑，也没有放宽业务断言。**

### 补充复核结果

[DoH 完整测试链复核记录](ablation-full-doh-followup.json)：

- 新测试基线之前/之后：全部测试链通过，Go 121 个 pass 事件、零 skip。
- 同一个上限放宽变异：`TestDoHRejectsOversizedValidJSON/over_limit` 失败，其余测试链通过。

准确结论是 **主轮 27/28，唯一存活项补充回归后 1/1 检出，两个负对照 2/2 通过**。补充复核只重跑这一项及前后基线，**不是对修改测试后的 revision 再跑一遍全部 30 项**。

## 环境、复现与安全边界

- WSL Ubuntu：Go 1.22.2、Python 3.12、Node 24.22、便携 PowerShell 7.5.4；另调用 Windows PowerShell 5.1 测试 DPAPI。
- 便携 pwsh 仅安装于本次实验的 `/tmp` 目录，没有修改全局 PowerShell 或服务配置。下载测试依赖与运行实验分离，实验内 HTTP/DNS/WSLC 都是离线 fixture。
- 从指定 Git archive 创建独立临时副本；不复制私有 local 文件。每个任务独立数据目录，不接触线上容器、数据卷或真实 provider。
- 并行仅发生在独立临时副本之间。报告原子落盘支持续跑；中断遗留的本次临时副本需清理。
- 程序未进行穷尽变异、全组合测试、真实 WSLC 验收、真实数据卷恢复、生产负载或外网渗透。

```sh
# Linux/WSL，需准备 Linux pwsh 和 Windows PowerShell 5.1。
python3 scripts/ablate_full.py --revision 0b7ac35 \
  --powershell /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe \
  --client-pwsh /tmp/uas-experiment-pwsh-7.5.4/pwsh \
  --scratch-root /mnt/c/Users/YOU/AppData/Local/Temp \
  --workers 2 --report /tmp/uas-full.json

# 在相同报告上断点续跑，序列化待归因项。
# 在上条命令追加 --resume --workers 1（workers 参数仅保留一个）。

# 补充复核：使用新测试 revision，只测试 DoH 这一项；仍跑全部测试链。
# 在新报告路径上使用 --revision 2b27afa --case doh_response_limit_relaxed。

# 仅验证归因实验器，不启动任何消融任务。
python3 -m unittest discover -s scripts -p 'test_ablate_full.py'
```

默认 CI 只运行正式回归和实验器归因单元测试，不运行故意失败且耗时较长的消融矩阵。

# 新增模块的单因素消融实验

## 范围和方法

基线：[`9dfb269`](https://github.com/l4place0/uni-agent-search/commit/9dfb269)。在 Git archive 创建的临时副本中，一次只关闭一项机制，运行与基线相同的定向测试；不修改工作区中的生产实现。共 9 个有害变异和 1 个行为等价负对照。

这是**机制消融 / 定向变异测试**，衡量所选回归是否能识别机制失效，不是检索质量、延迟、吞吐或费用 benchmark。没有随机重复、负载对照或全量变异采样；不能将结果推广为完整安全性保证。结构拆分自身的维护收益也不能用这些行为测试证明。

- Go 测试运行于 WSL Ubuntu，启用 race，禁用测试缓存。
- 部署测试调用 Windows PowerShell 5.1 的 mock；没有真实 WSLC 调用。
- DNS/dialer、HTTP 和供应商均为离线 fixture；没有访问真实供应商或线上服务。
- 编译错误、runner 错误不算“成功检出”。初版 hostname 变异产生 unused variable，实验器拒绝了该运行；修正为可编译的等价循环后重新完成整轮，以下仅记录完整有效运行。
- [机器可读记录](ablation-9dfb269.json)保存退出码、失败测试、耗时及补充探针对照。耗时包含编译、race、SQLite 和进程启动，**不得解释为模块运行开销**。

## 结果

| 被消融的机制 | 原回归结果 | 含义 |
| --- | --- | --- |
| readiness 的数据库探测，只保留 KeyPool fault 检查 | **未检出** | 原测试把两种故障混在一起，存在测试盲点 |
| HTTP 入口记录和计数 | 检出 | 缓存/抓取失败等入口计数断言失败 |
| Key 状态写失败后的 fail-closed | 检出 | 写失败后仍可调度，被回归拒绝 |
| panic 日志脱敏，改为记录原始 panic 值 | 检出 | 假敏感值出现在日志，被脱敏测试拒绝 |
| 实际挂载备份，退回固定 `sas-data` | 检出 | mock 识别备份源不再是自定义卷 |
| 健康检查实例身份比较，只看 ready 状态 | 检出 | mock 识别错误实例被接受 |
| 整组 DNS 公网校验 | 检出 | 混合地址/下一次私网解析进入 dialer，被拒绝 |
| 固定 IP 拨号，改回 hostname 拨号 | 检出 | IPv4 fallback/IPv6 固定地址断言失败 |
| 供应商失败后的 Key 轮换 | 检出 | 策略表和限流后轮换的功能测试失败 |
| **负对照**：复制同样策略给手动测试使用，取消逻辑复用 | **通过，符合预期** | 共享函数不是当前功能正确性的必要条件；其主要价值是减少后续策略漂移 |

原回归检出 **8/9 个有害变异**；等价负对照保持通过。样本为有意选择的关键路径，不能当作仓库总体 mutation score。

## 实际发现与修补

原 `TestReadinessAndStorageFailure` 先触发 KeyPool sticky fault，再关闭数据库。因此，即使完全删除数据库探测，readiness 仍因 KeyPool fault 返回 503，测试会通过。

追加一个独立探针，只关闭数据库，并明确确认 KeyPool fault 为 false：

- 原始实现 + 新探针：通过。
- 删除 DB probe 的消融实现 + 同一新探针：失败，失败项为 `TestReadinessReportsUnavailableDatabase`。

探针现已保留为常规回归：[readiness_test.go](../../internal/app/readiness_test.go)。这次发现的是**测试覆盖盲点**，不是现有生产 readiness 的运行缺陷；生产防线未被关闭。

另外，搜索和手动测试如果一起失去轮换标志，彼此的“状态一致性”测试仍可能通过；真正将该变异检出的是纯策略表和“限流 Key → 可用 Key”的功能测试。不同层次测试不能相互替代。

## 复现

在安装 Go 和 pwsh 的 Linux 环境：

```sh
python3 scripts/ablate.py --revision 9dfb269 \
  --report /tmp/uas-ablation.json
```

本次 WSL + Windows PowerShell 5.1 的命令：

```sh
python3 scripts/ablate.py --revision 9dfb269 \
  --powershell /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe \
  --scratch-root /mnt/c/Users/19065/AppData/Local/Temp \
  --report /tmp/uas-ablation.json
```

第二条命令需将 scratch-root 改为实际的 Windows 临时目录，以便 powershell.exe 能读取脚本。参数路径只是运行位置，不包含服务连接或凭据。临时副本自动删除；实验只从指定 Git revision 导出已跟踪文件，不复制工作区中的私有 local 文件。不要将故意失败的消融版本提交进生产代码或默认 CI。

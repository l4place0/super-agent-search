# Handoff：Uni Agent Search 架构审计后续修复

## 给接手会话的任务

用户希望在新会话中继续修复本次架构审计的问题。初始交接记录上下文与实施计划。**当前 A1–A5 已修复并验证；A6 已实现并通过离线 mock/DPAPI 验证（真实 WSLC 未验证）；A7 本轮网络边界回归已补齐；A8 本轮渐进整理已完成（非全量架构重写）**。请先复核当前工作区，再按各批进度从剩余项开始，分批提交，不要同时进行大范围架构重写或线上迁移。

### 当前剩余事项与验收边界

- 代码提交：A5 `9c3314e`，A6 `0f6e393`，A7 `7b89e41`，A8 `87e546a`，前端纯格式化 `da39317`；错误传播/文档收尾 `0dd5e8e`，mock exit code 隔离 `02b1c1c`。均已推送 `origin/main`。
- [GitHub CI run 39958270219](https://github.com/l4place0/uni-agent-search/actions/runs/39958270219) 全部通过，包含 Go/race、vet、真实 sh/pwsh CLI 对 mock HTTP、Node DOM 合约、pwsh 编排 mock、递归 PowerShell parser。Windows 5.1 mock 和 DPAPI roundtrip 亦在本机通过。
- app statements 覆盖率 80.9%，credential 89.5%；diagnose 仍无命令级测试，不能声称实际诊断流程已验证。
- 没有连接真实 WSLC，没有部署、停止服务或迁移线上数据。需要隔离会话验证 WSLC preview 对 mount、stop-timeout、image ID、inspect 格式的兼容，以及真实卷备份/恢复。
- A6 update/recreate 现在保留实际配置和容器名，不再接受 Bind/Proxy/DoH 覆盖；ensure 不再因健康失败自动重建。恢复计划使用 Windows 当前用户 DPAPI，不能换机器/用户解密；恢复会丢弃升级后的写入。
- A5 新增 `/readyz`；Key 状态写失败 fail closed，排除故障后需重启。`/healthz` 仍为 liveness。
- A8 明确保留的耦合及后续方向见 [代码边界](code-boundaries.md)；完整浏览器 E2E、Windows 信号行为、外网渗透与出站防火墙仍未验证。
- 消融进度：首轮[定向实验](audit/ablation.md)补齐 readiness 隔离回归；后续[A1–A8 全清单 × 全测试链实验](audit/ablation-full.md)执行 30 个版本（28 个有害 + 2 个等价负对照）。主轮检出 27/28；唯一存活的 DoH 上限变异已通过合法 JSON 大小边界回归补齐并完成单项全链复核。两个负对照均通过。生产实现没有被关闭；没有在新测试 revision 重跑全部 30 项。
- `2b27afa` 增加 DoH 边界用例并稳定时间敏感测试（绝对 Retry-After、独立的 500 冷却时间窗口、CLI 测试进程 60 秒预算）。`scripts/ablate_full.py` 支持检查点续跑、机制 witness 归因；非目标测试失败不能算防线被检出。
- 下文 P1/P2 计划保留为历史验收清单，不表示仍需从 A1 重做。最新状态和回归链接以审计报告“修复进度”为准。

先读：

1. 本文件。
2. [完整架构审计](architecture-audit.md)。问题编号 A1–A8 以该报告为准。
3. [确定性复现用例](audit/repro_test.go.txt)。前三个用例断言的是正确行为，在审计基线上会失败。
4. [改名迁移说明](rename-migration.md)。改名兼容边界与部署注意事项。

## 仓库与已完成事项

- 上游已改名：https://github.com/l4place0/uni-agent-search
- 分支：`main`。
- 改名与审计提交：`bc4bc19358cec2cc3ef8a0b608414e2212b39ef3`（简称 `bc4bc19`），已推送。
- 该提交的 GitHub CI 全部通过：run `39946359645`，https://github.com/l4place0/uni-agent-search/actions/runs/39946359645 。
- 写本交接前工作区干净，与 origin/main 一致。新会话应重新检查，不能假定用户未做其他修改。
- 已更新 Go module、界面、User-Agent、镜像/容器默认名称、仓库 skill 与本机已安装 skill。
- Skill 名称：`uni-agent-search`，脚本 `scripts/uas.sh` / `scripts/uas.ps1`。
- 本机安装位置：`C:\Users\19065\.agents\skills\uni-agent-search`。该安装版 SKILL.md 含本机连接配置，不能用仓库版盲目覆盖。
- 本机旧 skill 目录已移走；新会话应重新加载 skill 元数据。

### 唯一未完成的改名步骤：本地仓库目录

初始交接时旧目录被占用；接手时已确认目录为 `C:\Users\19065\Projects\uni-agent-search`，改名已完成，无需重复操作。

关闭占用旧目录的会话/终端后，在父目录执行：

```powershell
cd C:\Users\19065\Projects
Rename-Item -LiteralPath .\super-agent-search -NewName uni-agent-search
```

然后从 `C:\Users\19065\Projects\uni-agent-search` 开新会话。若用户已经完成，勿重复操作。所有下文命令路径须按实际目录调整。

## 不可破坏的边界

- 这是个人、单用户、单实例服务。保留单体架构，不引入微服务、多副本共享 SQLite 或分布式调度。
- 线上容器、数据卷、计划任务尚未改动。本交接不授权自动部署、停止服务或迁移数据。
- `SAS_*` 环境变量继续使用；没有实现 `UAS_*` 别名。
- `sas-data` 数据卷、`sas-server` 构建产物名、`sas-image-*.tar`、`sas-admin` 会话键等刻意保留兼容；不要机械替换所有 sas 字符串。
- API、端口、数据库/凭据文件格式未因改名变化。版本仍为 `0.2.4`。
- 不读取/打印私有 token、provider secret、master.key；不要提交 `*.local*`、数据库、凭据、`dist/` 历史产物或本机连接信息。
- 用 httptest、临时数据库、假 Key 测试；默认不访问真实供应商、不产生付费调用。
- upstream transport 允许管理员指定内网 SearXNG；public fetch 才执行公网 SSRF 边界。不要把前者误判为客户端漏洞或为了测试关闭后者防护。
- `basis=model_citation` 不能改成“已验证来源正文”。

## 实施计划与验收

### 第一批：A1 + A2，Key 调度一致性（P1）

读：`internal/app/store.go`（Key、迁移、acquire/release）、`http.go:updateKey`、`key_test_call.go:testKey`、`search.go:providerSearch`。

已复现：

- A1：同一 Key 两个请求并发，先返回 429 设置冷却，后返回 200 无条件清掉冷却；失败间也可缩短冷却。
- A2：在途请求持有旧 secret；管理员替换同一 ID 的 secret 后，旧请求的 401 会停用新 secret。

建议：

- 冷却更新单调延长，成功不抹掉其他请求产生的有效冷却；last_error 的保留/清除也应有一致语义。
- Key 引入持久 revision，获取时返回 ID + revision 的 lease；释放时只允许当前 revision 更新当前凭据状态。
- 明确 secret 替换、reset、启用/停用对 revision 的影响。旧 lease 必须释放并发占用，不能泄漏 slot 或减成负数。
- 手动 Key 测试路径也要使用同一租约和状态规则，不能只修 providerSearch。
- revision 迁移必须保留现有 secret、计数、cooldown 和用户设置；测试旧库升级与重启。
- 注意 SQLite 只有一个连接：不要在占用未关闭 rows/事务时又通过 db 发起嵌套查询，避免自锁。

验收：A1/A2 复现转成正常回归测试并通过；补充两种完成顺序、多次失败、reset/替换期间在途成功与失败、手动测试、删除后释放、并发限制与持久化测试；全套 race 通过。

第一批进度：A1/A2 已修复并验证，参见审计报告“修复进度”和 `internal/app/key_lease_test.go`。revision additive migration 保留旧库数据；旧 lease 不污染新状态且可靠释放 ID 级占用。WSL 全套 race、vet、build 通过；未上线。以下保留第二批实施计划与验收历史。

### 第二批：A3，缓存配置代隔离（P1）

读：`store.go:invalidate`、`search.go:search`、`http.go` 各配置 mutation。

已复现：清空 map 时在途搜索未结束，随后仍把旧结果写回。配置更新同样受影响。

建议：引入配置/缓存 epoch，确保 provider 快照、读缓存与写缓存的代际一致性。尤其防止“先读旧 provider 快照、再读取新 epoch”的组合；仅在写入前比较一个晚取的版本不够。不要为了简单持有全局锁跨越网络请求。

验收：A3 用例转成正常回归并通过；新增禁用 provider、修改 endpoint、清缓存与在途搜索交错的 API 级测试。固定 channel/barrier 控制顺序，避免靠 sleep。singleflight 可后续做，不是这一批的必要前置。

第二批进度：A3 已修复并验证。provider 快照与 epoch 原子读取，命中/回填验证同代；API 级 barrier 测试覆盖 clear、endpoint 修改、禁用 provider。全套 race、vet、build 通过。以下保留第三批实施计划与验收历史。

### 第三批：A4，真正等待优雅停机（P1）

读：`cmd/server/main.go`、`App.Close` 与 HTTP client 生命周期。

静态确认：Shutdown goroutine 关闭 listener 后，ListenAndServe 返回，main 未等待排空就关闭数据库并退出。

建议：等待 shutdown 完成；确定排空超时后的取消/关闭策略，先结束请求再关闭 DB/transport。当前排空预算 10 秒小于搜索预算 90 秒，需明确产品取舍；仅增加一个 channel 等待不等于超时后请求已收尾。

验收：真实子进程 SIGTERM 集成测试，覆盖正常排空、超时强制退出、无在途请求退出及监听启动失败；确认关闭顺序且不会挂死。测试放在 Linux/WSL/CI 执行，Windows 不支持的信号用明确的平台限制处理。不要只用 httptest 声称已验证进程行为。

第三批进度：A4 已修复并验证。main 等待 10 秒排空，超时取消请求/关连接，再给 5 秒收尾预算；handler 结束后才关闭 transport/DB，无法收尾则非零退出且不抢关数据库。Linux 真实子进程测试覆盖 SIGTERM 正常排空、超时取消、空闲退出、监听失败和 DB 尾部日志。全套 race、vet、build 通过；Windows 信号行为与线上运行未验证。

P1 收尾时的记录（后续 P2 已按文首状态实施）：A1/A2 提交 `63132e9`，A3 提交 `8a03f24`；A4 提交 `edc4ef0`。三批已推送 `origin/main`，GitHub CI run `39949541164` 全部通过；没有部署。用户已授权验证后提交并推送，但没有授权部署。

### P2 原实施计划（历史清单，当前进度见文首与审计报告）

| 编号 | 内容 | 建议验收 |
| --- | --- | --- |
| A5 | readiness 与 liveness 分离、SQL 错误处理、入口级请求观测、脱敏 panic 日志 | DB 故障不伪装就绪；缓存命中和 fetch 失败有准确计数；日志不泄密 |
| A6 | WSLC inspect 驱动配置保真、实际卷备份、身份健康检查和恢复流程 | 自定义卷/Bind/Proxy/DoH 不丢；失败可恢复；先 mock/本地验证，不直接操作线上 |
| A7 | SSRF/DoH/DNS rebinding/redirect/IPv6 边界测试 | 注入 resolver/dialer，无外网稳定复现；这是覆盖缺口，不是已验证绕过 |
| A8 | 渐进明确 Repository/KeyPool/Provider/Search/Fetcher 边界，减少失败分类重复 | 行为与 API 不变；先抽取共同语义，避免纯形式拆包；前端格式化独立提交 |

A6 历史基线只有旧名称识别；当前已改为 inspect 保真、实际挂载备份和恢复计划。不要再使用旧的固定卷/默认参数升级流程；新流程仍需真实 WSLC 隔离验证。

## 已知验证状态与环境

- 宿主 Windows Git Bash 找不到 go/pwsh；WSL `Ubuntu` 有 Go、gcc、jq，可执行 race 测试。
- 上次 WSL `go version` 为 `go1.22.2 linux/amd64`；CI 为 Go 1.25.x，两边正常测试均通过。
- 核心包 `internal/app` statements 覆盖率 71.5%；cmd/server 与 cmd/diagnose 为 0%。
- `go vet ./...`、server 构建、sh 语法及 help 均通过。
- 宿主 PowerShell 5.1 parser 语法检查通过；GitHub CI 的 pwsh 语法检查也通过。没有验证 PowerShell 7 客户端端到端 HTTP 行为。
- A1–A3 的不变量测试在审计版本上全部按预期失败；race detector 没有报内存竞争。这些是业务状态竞态，不是 race detector 能保证排除的问题。
- 尚未做外网渗透、负载测试、依赖 CVE 扫描或线上恢复演练。

### 新会话起步命令

```sh
git status --short --branch
git remote -v
git log -3 --oneline
```

在 WSL 项目目录（改名成功后）：

```sh
cd /mnt/c/Users/19065/Projects/uni-agent-search
go test -race -cover ./...
go vet ./...

# 先复现，预期失败；随后保留并整理为修复提交中的常规测试。
cp docs/audit/repro_test.go.txt internal/app/audit_repro_test.go
go test -race ./internal/app -run TestAudit -count=1 -timeout=30s
```

仅做复现、不开始修复时，删除临时 `.go` 副本；不要把故意失败的用例意外提交到 main。修复完成后，应把已修项用例转成正常 Go 测试，避免永远藏在 txt 中。

每批至少执行：

```sh
go test -race -count=1 ./...
go vet ./...
go build -o /tmp/uni-agent-search-check ./cmd/server
sh -n skills/uni-agent-search/scripts/uas.sh
git diff --check
```

改 ps1 时执行 parser 检查；修改 CLI 行为则补实际客户端/mock HTTP 测试，不能只检查语法。

推送说明：前次普通 git push 因凭据流程超时，以下单次命令成功，不需要修改全局凭据设置或显示 token：

```sh
GIT_TERMINAL_PROMPT=0 git -c credential.helper= \
  -c 'credential.helper=!gh auth git-credential' \
  -c http.lowSpeedLimit=1 -c http.lowSpeedTime=20 push origin main
```

是否提交/推送遵从新会话用户指示；本文件不是自动部署授权。

## 完成后的交付要求

- 每个问题标明“已修复并验证 / 仅静态修改 / 未处理”，不要把 P2 建议笼统算作已完成。
- 在原审计报告保留发现历史，添加修复提交与对应回归测试链接；更新本交接的剩余清单。
- 报告迁移影响、测试结果、未验证的运行环境以及是否实际推送。
- 默认只修代码与测试，不上线；若用户要求部署，先读迁移说明、确认真实卷与回滚计划。

## 可直接粘贴到新会话的启动提示

> 继续 uni-agent-search 的验收/后续改进。请先读 docs/HANDOFF.md、docs/architecture-audit.md、docs/code-boundaries.md 并核对工作区与提交历史。A1–A5 已修复；A6 已实现离线验证但真实 WSLC/卷恢复未验证；A7 本轮回归已补齐；A8 已完成渐进整理，保留耦合见边界文档。不要重复旧的故意失败步骤，也不要把 mock 当作线上验收。保持单实例与 SAS_* / 既有数据兼容，不读取私有凭据、不调用付费 provider、不直接部署线上。

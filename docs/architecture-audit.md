# Uni Agent Search 架构审计

## 范围与结论

审计基线：`e269b1c`，加上本次品牌/仓库/skill 改名；运行逻辑未进行架构修复。覆盖 Go 服务、HTTP/API、SQLite/Key 池、provider 适配、抓取网络边界、Web 控制台、两种 CLI、Docker/WSLC 脚本及 CI。

**结论：当前模块化单体适合单用户、单实例的个人 Agent 搜索基础设施，无需拆微服务。但有 3 个已用测试复现的状态一致性缺陷，以及停机、部署与可观测性方面的待修项。** 当前不应扩展为多副本共享数据库或直接提供多租户公共服务。本次未发现已验证的无鉴权远程执行/凭据泄漏问题；这不等于完整安全认证。

未进行线上容器迁移、付费 provider 调用、外网渗透、负载测试或依赖 CVE 扫描。WSLC 的实际运行行为、PowerShell 7 HTTP 行为及各供应商当前协议兼容性不在动态验证范围。

## 结构与信任边界

```text
调用 Agent（分解问题、判断证据、综合答案）
  └─ skill CLI（sh / PowerShell；Client Bearer token）
      └─ HTTP API（鉴权、64 KiB JSON、16 个执行槽）
          ├─ search：并行 provider → Key 池 → HTTP → 解析 → 融合 → 内存缓存
          └─ fetch：公网地址校验/连接绑定 → 有限抓取 → HTML 提取/字符分页
管理浏览器（Admin Bearer token）
  └─ provider/key 配置、手动测试、缓存清理、日志
存储：SQLite WAL（单连接）+ master.key + credentials.json
出站：可信管理员配置的 upstream transport / 非可信 URL 的 public transport
```

关键边界合理：

- `internal/app/net.go` 将可信管理员指定的 upstream 与客户端可指定的 public fetch 分开；后者关闭环境代理、逐地址检查并连接到已验证 IP，重定向重新检查。前者禁止带凭据跟随重定向；内网 SearXNG 是明确的管理权限功能，不应误报为客户端 SSRF。
- Client token 不能访问管理 API；常量时间比较、随机令牌、严格 JSON、响应大小与请求超时已实现。
- Key 使用 AES-GCM；密钥与数据库同卷，主要保护单独泄露的数据库，不防主机/整卷失陷。文档已明确。
- Web 使用 `textContent`、链接协议限制和 CSP；检索结果区分 `search_extract` / `model_citation` / `fetched_page`，skill 明确外部内容不能改变任务指令。
- 有全局执行槽、每 Key 并发、最多 3 次选 Key、429 冷却、401/403 停用、有限缓存和日志保留。适合个人使用，但不是请求速率或费用预算系统。

## 发现（按优先级）

优先级：P1 应在下一次可靠性迭代优先修复；P2 中期改进。下列项均未在本次改名中修复。

### A1 · P1：并发成功响应会抹掉仍有效的冷却【已复现】

位置：`internal/app/store.go`，`acquire` / `release`。

同一 Key 默认允许两个并发请求。请求 A 返回 429 并设置未来的 cooldown；较早发起但较晚完成的请求 B 返回 200，`release` 无条件执行 `cooldown_until=0,last_error=''`，立即恢复调度。失败响应间也可能用更短 cooldown 覆盖更长 cooldown。

影响：违反 Retry-After、增加限流与付费调用压力；互斥锁只能避免数据竞争，不能解决完成顺序导致的逻辑竞态。

建议：成功只累计成功计数，不清除其他请求已建立的有效冷却；失败采用单调延长策略（例如 MAX）。手动 reset 应有明确的新一代状态语义。

### A2 · P1：替换凭据后，旧请求仍能修改新凭据状态【已复现】

位置：`internal/app/http.go:updateKey`、`internal/app/store.go:release`。

管理员就地替换 secret 但保留 Key ID。旧 secret 的在途请求随后返回 401/403，`release` 按 ID 停用记录，结果新 secret 被停用。旧请求也可能重设刚清除的 cooldown/last_error。

建议：Key 记录增加 revision/generation；acquire 返回租约（ID + revision），release 的状态更新用 revision 做条件匹配。占用计数仍必须可靠释放；测试旧请求成功/失败与替换、禁用、reset 的交错顺序。

### A3 · P1：缓存清理不能隔离在途旧配置请求【已复现】

位置：`internal/app/store.go:invalidate`、`internal/app/search.go:search`。

清缓存只替换 map。正在运行的搜索持有旧 provider/key 配置，结束后仍写入新 map。管理配置变更会调用同样的 invalidate，所以变更前的结果可能再次命中长达 5 分钟。默认 providers 请求的 cache key 未包含有效 provider 集合，容易复现禁用/改地址之后返回旧数据。

建议：维护配置/缓存 epoch，在读取 provider 快照时获取同一代版本，缓存 key 和写入都验证版本；防止“旧快照搭配新 epoch”。可再加 singleflight 减少同查询并发付费，singleflight 也必须隔离不同代。

### A4 · P1：main 没有等待优雅停机完成【静态确认】

位置：`cmd/server/main.go`。

信号 goroutine 调用 `s.Shutdown`；它关闭 listener 后 `ListenAndServe` 即返回 `ErrServerClosed`，main 随即执行 `a.Close` 并退出，没有等待 Shutdown goroutine 完成。已接收的搜索可能被截断、数据库提前关闭；声明的 10 秒排空也没有实际等待保证。即使增加等待，10 秒仍小于最长 90 秒搜索预算，需要明确强制取消策略。

建议：main 等待 shutdown done；排空/取消 handler 后再关闭 transport 和数据库。增加真实子进程 SIGTERM 集成测试，不能仅靠 httptest 验证。

### A5 · P2：错误和健康状态难以区分【静态确认】

位置：`internal/app/http.go` 的 healthz、overview、logs、fetchHandler、recover；`store.go:record/release`。

- healthz 永远返回 ok，不检查数据库可用性；应将存活与就绪分开，不必把所有外部 provider 故障都视为不就绪。
- 多处 SQL 读写错误被忽略，recover 不记录堆栈。存储失败时可能对外成功但调度/日志并未持久化。
- 缓存命中直接返回不记请求；fetch 的非 2xx、类型不支持和部分解析失败不记日志。面板“请求次数/成功率”不能代表入口真实流量。

建议：脱敏结构化日志、请求 ID、存活/就绪分离、入口级计数与耗时、cache hit/429/upstream 状态计数；对数据库错误明确分级处理。不要记录 token、secret 或未经处理的供应商错误正文。

### A6 · P2：部署恢复依赖默认配置，缺少配置保真验证【静态确认】

位置：`deploy/update-wslc.ps1`、`ensure-wslc.ps1`、`recreate-wslc.ps1`、`start-wslc.ps1`。

start 支持自定义 DataVolume，但 update 备份固定 `sas-data`，recreate/start 调用链也会回到默认卷。重建还依赖操作者重传 Bind/Proxy/DoH。对自定义部署可能备份错卷或启动空配置。健康检查只确认某地址返回成功，不证明该响应来自预期新实例。ensure 可能把健康检查的临时故障解释为需要重建。

建议：inspect 读取并验证真实 mount、image、labels、网络与允许的环境配置；生成明确的迁移计划，失败保留旧实例/配置和可执行回滚步骤；健康验收包含实例身份/构建版本。此轮新增旧名称迁移参数只解决命名识别，不解决上述通用恢复问题。

### A7 · P2：公网抓取安全测试覆盖还不完整【覆盖缺口，非已验证绕过】

位置：`internal/app/net.go:publicIP/publicDial/resolvePublic`、现有 `TestSSRFAndHTMLReader`。

实现已有关键防线，但测试主要是静态 IP 分类与直连 loopback。尚缺混合公网/私网 DNS 响应、DNS rebinding、重定向到私网、DoH 超限/异常响应与 IPv6 特殊用途地址回归。手写 CIDR 列表不应被文档解读为永远完整的特殊用途地址数据库。

建议：注入 resolver/dialer 做无外网测试；对照明确的特殊用途地址策略维护测试集，按需限制目标端口；辅以运行环境出站防火墙。没有这些测试不能声称所有 SSRF 变体都已排除。

### A8 · P2：单体边界可进一步明确，但不需要分布式化【设计建议】

位置：`internal/app/*`、`internal/app/web/*.js`、`cmd/diagnose/main.go`。

App 同时持有 SQL、密码学、HTTP、调度和缓存；provider 使用 switch 扩展；Key 手动测试和搜索重复了错误/冷却映射；diagnose 复制数据库与解密结构；前端高度压缩的单行代码不利于 review。SQLite 单连接加 poolMu 内 SQL 会串行化调度，且缺少 DB context，当前低负载可接受。

建议在同一进程中逐步划分 Config/Repository、KeyPool、ProviderAdapter、SearchService、PageFetcher 和 HTTP 层；先提取失败分类与租约语义，再做接口，不要为形式拆分。格式化 JS、增添客户端端到端测试。保持单实例约束；多副本需要集中租约/配额和跨实例失效，不是简单扩容容器。

## 动态验证

执行环境：Windows 宿主 + WSL Ubuntu，`go1.22.2 linux/amd64`；CI 配置为 Go 1.25.x。PowerShell 语法检查使用宿主 5.1 parser，不代表 PowerShell 7 网络行为已验证。

| 验证 | 结果 |
| --- | --- |
| `go test -race -cover ./...` | 通过；internal/app statements 71.5%，两个 cmd 包无测试覆盖 |
| `go vet ./...` | 通过 |
| sh 语法检查与 `uas.sh --help` | 通过 |
| deploy/*.ps1 与 uas.ps1 parser 检查 | 通过 |
| A1/A2/A3 定向不变量测试 | 三项均按预期失败，确认缺陷；未观察到 race detector 报告 |

复现文件：[audit/repro_test.go.txt](audit/repro_test.go.txt)。这是待修复不变量测试，不纳入当前 CI，以免把已知缺陷伪装成通过的回归测试。复现不访问真实 provider、不使用私有凭据：

```sh
cp docs/audit/repro_test.go.txt internal/app/audit_repro_test.go
go test -race ./internal/app -run TestAudit -count=1 -timeout=30s
# 当前实现预期 exit 1，分别报告 cooldown 被清除、新凭据被停用、旧缓存重新写入。
rm internal/app/audit_repro_test.go
```

后续修复后应把这些用例转成常规 `.go` 回归测试，并补充 handler/API 层并发变更用例。Race detector 通过不代表没有业务状态竞态。

## 修复进度

### 第一批：A1/A2 已修复并验证

修复提交标题：`Fix key cooldown monotonicity and revision-scoped leases`（可通过 Git 历史定位）。

- 冷却取最大值；并发成功不清除有效冷却，诊断保留至冷却结束后的成功或显式 reset。
- Key 增加持久 revision；替换 secret、reset、启用状态切换提升 revision。仅编辑标签/权重/并发上限不提升。
- 旧 lease 只释放 ID 级占用，不再修改新 revision 的计数、启用、冷却或错误。重复释放无效。
- 手动测试与搜索共享 reservation/release 规则；旧 revision 仍计入同一 ID 并发上限。
- 旧库通过 additive migration 增加 revision，保留原凭据和状态；无需数据重建。

回归：[A1/A2 原始复现](../internal/app/audit_repro_test.go)、[完成顺序/管理变更/手动测试/迁移重启](../internal/app/key_lease_test.go)。WSL Ubuntu 下全套 `go test -race -count=1 ./...`、`go vet ./...`、server build、sh 语法和 diff 检查通过。未访问真实 provider，未部署线上。上面的发现描述保留审计基线历史。

### 第二批：A3 已修复并验证

修复提交标题：`Fence search cache with configuration epochs`。provider 快照和 epoch 在同一 cacheMu 临界区读取；缓存命中与回填均验证 epoch，网络请求不持锁。所有现有配置 mutation 的 invalidate 都会提升 epoch。

回归：[清缓存/改 endpoint/禁用 provider 与在途请求的 API 级交错测试](../internal/app/cache_epoch_test.go)，使用 channel barrier，无 sleep/外网。WSL 全套 race、vet、build、sh 语法和 diff 检查通过。A4–A8 尚未处理。

## 推荐执行顺序

1. 修 A1/A2：统一 Key lease/revision 和单调冷却，补并发回归。
2. 修 A3/A4：缓存 epoch + 正确的停机完成等待。
3. 修 A5/A6：入口可观测性、readiness、部署配置保真和恢复演练。
4. 补 A7 测试与依赖漏洞扫描，再按 A8 渐进提取模块。

不要在品牌改名提交中混入未经验证的调度和存储重构；本报告与改名一起提交，修复建议单独实施。

# 单进程代码边界

本轮 A8 采用渐进整理，不引入微服务、跨实例租约或无用的全量接口层。`App` 仍是生命周期与 HTTP 的组合根，API、SQLite 格式和单实例约束不变。

| 边界 | 当前实现 | 约束 |
| --- | --- | --- |
| 配置 / Repository | `store.go` 的 schema、provider/key 读取、批量写入、revision 条件更新与请求记录 | SQLite 单连接；先关 rows 再进行后续查询。Key 调度 SQL 不散落到适配器中 |
| KeyPool | `key_pool.go` 的独占状态、加权选 Key、租约 reservation/release；`key_test_call.go` 的指定 Key 获取 | `poolMu` 只保护调度与条件持久化，不跨网络请求；旧 lease 只释放 ID 级占用 |
| ProviderAdapter | `search.go:requestProvider` 与 `extended.go` | 负责协议、响应解析和类型化 upstreamError；不直接更新 Key |
| 失败策略 | `provider_outcome.go` | 搜索与手动测试共用状态/冷却/重试分类；只有搜索决定轮换，手动测试只请求指定 Key 一次 |
| SearchService | `search.go:search/providerSearch` | provider 快照与缓存 epoch 同代；并行请求后融合；`basis=model_citation` 不表示已核验正文 |
| PageFetcher / 出站策略 | `http.go:fetchHandler` 与 `net.go` | upstream 信任管理员配置；public fetch 禁用代理、整组 DNS 校验、固定 IP 连接、每次 redirect 重验证 |
| HTTP / 观测 | `http.go`、`observe.go` | 鉴权、JSON/执行槽、状态映射、入口记录、请求 ID、脱敏诊断 |
| 凭据格式 | `internal/credential` | server 和 diagnose 共用 AES-GCM 格式；不负责 I/O，不记录 material |

## 验证层次

- `provider_outcome_test.go`：纯策略表和搜索/手动调用实际状态一致性。
- `key_lease_test.go` 等：调度、迁移、revision、并发不变量。
- `net_boundary_test.go`、`net_transport_test.go`：注入 DNS/dialer/DoH，完全离线验证网络边界。
- `client_e2e_test.go`：真正执行 sh/pwsh CLI，对本地 mock HTTP 验证 UTF-8/JSON/数组/分页/鉴权优先级/失败退出码。本地没有 pwsh 时明确 skip，CI 先检查依赖再运行。
- `web_test/web.test.mjs`：运行真实 JS 的离线 DOM/API 合约测试，覆盖链接协议、模型引用警告、textContent 渲染、令牌失效、Key 控件；不是完整浏览器可访问性/E2E 认证。
- `cmd/server/main_linux_test.go`：真实子进程 SIGTERM 与收尾持久化。
- `deploy/tests/wslc-mock.tests.ps1`：本地编排/恢复 mocks，不代表真实 WSLC 已验收。

## 刻意保留的耦合与后续方向

部分配置 mutation 和 fetch 的 HTTP/业务逻辑仍在 `http.go`；provider 仍通过 switch 分发；diagnose 保留其只读 SQL 查询，但不再复制密码学格式。暂不为这些增加没有第二个实现的接口或做全量拆包。未来有真实扩展需求时，再把 PageFetcher 返回值、Repository 的 context/error 语义和 ProviderAdapter 注册表渐进抽出；不得把这份边界说明误读为所有建议已实现或已具备多租户能力。

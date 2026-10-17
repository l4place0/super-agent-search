# Super Agent Search

面向 Agent 的自托管搜索服务。Go 服务端包含 Web 控制台；客户端是可随 Skill 分发的 sh/pwsh 脚本。

v0.2.4 提供 Bing RSS / DuckDuckGo 免 Key 搜索，Tavily、Brave、Exa API，Grok / xAI 官方 Responses 联网搜索、Grok 兼容中转 Chat Completions，以及自建 SearXNG JSON 搜索。多服务商并行检索，URL 去重并保留排名来源；Bing/DDG 按相关组降低重复加分。API Key 池使用加权轮询，支持并发上限、429 冷却、401/403 停用及单请求最多 3 个 Key 尝试。服务商网络错误和 5xx 不轮番重试全部 Key。

## 本地运行

```sh
go test ./...
go run ./cmd/server
```

打开 `http://localhost:8080`。首次启动在 `data/credentials.json` 生成独立的 `admin_token` 和 `client_token`。`data/master.key` 用于 AES-GCM 加密数据库中的服务商 Key；备份时必须同时保留数据库和 master.key。密钥文件与数据库位于同一信任边界，不能代替磁盘访问控制。

```sh
docker compose up -d --build
```

Compose 默认监听 `127.0.0.1:8098`。WSLC 远程部署方式见 [deploy/README.md](deploy/README.md)。

## 客户端

```sh
export SAS_URL=http://localhost:8098
export SAS_TOKEN_FILE=/path/to/client-token
sh skills/super-agent-search/scripts/sas.sh search "Go graceful shutdown" --max-results 6
sh skills/super-agent-search/scripts/sas.sh fetch https://go.dev/doc/
```

Windows PowerShell 7：

```powershell
$env:SAS_URL='http://localhost:8098'
$env:SAS_TOKEN_FILE='C:\path\client-token'
pwsh -NoProfile -File skills/super-agent-search/scripts/sas.ps1 search 'Go graceful shutdown'
```

sh 客户端依赖 curl、jq。将整个 `skills/super-agent-search` 目录复制到 Agent 的 Skill 目录即可，CLI 使用手册在 [SKILL.md](skills/super-agent-search/SKILL.md)。

## API

| 接口 | 鉴权 | 用途 |
| --- | --- | --- |
| `GET /healthz` | 无 | 健康检查 |
| `GET /v1/capabilities` | Client/Admin Bearer token | 能力与服务商状态 |
| `POST /v1/search` | Client/Admin Bearer token | 搜索与融合 |
| `POST /v1/fetch` | Client/Admin Bearer token | 读取公开 HTML / 文本 |
| `/api/admin/*` | Admin Bearer token | 面板管理接口 |

搜索输入示例：`{"query":"Go HTTP server","max_results":8,"providers":["tavily"],"include_domains":["go.dev"],"no_cache":false}`。不传 providers 时使用所有启用的服务商。没有结果时返回 HTTP 502 和完整尝试记录；部分成功返回结果及 warnings。

读取输入示例：`{"url":"https://go.dev/doc/","focus":"modules","offset":0,"limit":16000}`。正文按字符窗口返回，可用 next_offset 继续读取；每次会重新抓取，页面可能变化。读取请求阻止本地、私网、保留地址和重定向绕过。

管理面板按服务商展开配置接口地址、模型、启停状态和独立 Key 池。支持一行一个 Key 批量添加并去重，修改标签、轮询权重与并发，就地替换 Key、重置冷却、测试指定 Key，并查看最近请求。批量写入为原子操作，重复或非法 Key 会回滚整个请求。连接测试会实际调用上游并可能计费。服务商 Key 不通过 API 返回。

## MVP 边界

- 融合分仅用于排序，未经过真实相关性数据集校准。结果是搜索摘录，不能直接视为事实验证。
- 免 Key 引擎可能限流或返回挑战页；可在面板配置 API 服务商。没有附带用户的搜索服务商 Key。
- 暂不支持 PDF、登录态网页、浏览器渲染、定时研究或服务端 Agent 自主研究循环。
- Key 调度状态保存在 SQLite；执行中的并发和轮询游标为单实例内存状态。部署为一个服务实例；共享账户额度、每分钟配额和计费额度不在 MVP 调度范围。
- 搜索缓存 5 分钟，降级结果 30 秒；最多 200 项。操作记录最多约 2,000 条，并保存问题/URL，不记录 Key 或上游错误正文。
- 管理令牌与调用令牌可分别通过 `SAS_ADMIN_TOKEN`、`SAS_CLIENT_TOKEN` 覆盖；至少 24 字符且必须不同。环境变量覆盖时 credentials.json 保留的是初始令牌。

## 实现参考

架构调研参考 [Smart Search](https://github.com/konbakuyomu/smartsearch) 和 [SearchBoost](https://github.com/Mr-remon219/search-boost)。本项目独立实现，未复制其源码。

## Grok 接入

- **Grok / xAI 官方**（provider ID `xai`）：调用 `/v1/responses`，发送官方 `web_search` / `x_search` 工具，模型与搜索范围可在服务商配置中修改。
- **Grok / 兼容中转**（`grok-compatible`）：调用 `/chat/completions`，显式发送 tools: [{type: "web_search"}]，仅开启网页搜索，要求中转支持此工具。中转没有联网或没有返回可引用来源时，不会把普通聊天回答冒充搜索结果。请求按来源返回 JSON，保留标题、摘要、作者 author、发布时间 published_at；这些内容仍是模型提供的未核验信息（basis=model_citation），不是页面抓取正文。
- 两种 Grok 路由都返回 `basis=model_citation` 的引用候选，不能当成来源正文。普通搜索摘录仍为 `search_extract`，混合结果优先保留实际搜索摘录。
- SearXNG（`searxng`）不需要 Key，需在实例启用 JSON 格式。
- Grok 默认停用，需要用户填写有效 Key 和账户可用模型；默认模型名称可以修改。搜索总体超时 90 秒，客户端超时 100 秒，服务商超时可配置为 2–75 秒。

接口依据：[xAI Web Search](https://docs.x.ai/developers/tools/web-search)、[xAI Citations](https://docs.x.ai/developers/tools/citations)、[SearXNG Search API](https://docs.searxng.org/dev/search_api.html)。

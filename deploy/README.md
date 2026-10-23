# Docker / WSLC 部署

常规部署使用根目录的 `docker compose up -d --build`，默认监听 `127.0.0.1:8098`。跨机器访问时设置 `SAS_BIND`，并在可信网络或 HTTPS 反向代理后提供服务。

## WSLC 首次部署

目标 Windows 机器需要安装 WSLC、存在可用会话，且目标用户已登录桌面。辅助脚本通过 SSH 上传镜像，再用交互式计划任务执行 WSLC 原生端口发布。请先在自己的机器验证 WSLC 命令兼容性。

目标 WSLC 会话内需已有镜像仓库，地址为 `127.0.0.1:5000`；脚本不安装或重建仓库。

在开发机器的 Linux / WSL 环境中构建：

```sh
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o dist/sas-server ./cmd/server
cp /etc/ssl/certs/ca-certificates.crt dist/ca-certificates.crt
docker build -f Dockerfile.prebuilt -t uni-agent-search:0.2.4 .
docker save -o dist/sas-image-0.2.4.tar uni-agent-search:0.2.4
```

在 Windows PowerShell 7 中指定自己的 SSH 目标和 WSLC 会话：

```powershell
pwsh -NoProfile -File deploy/deploy-wslc.ps1 `
  -RemoteHost user@your-server `
  -Session your-wslc-session `
  -RemoteDirectory C:\containers\uni-agent-search\releases\0.2.4
```

脚本导入并推送镜像、创建 `sas-data` 数据卷，通过普通用户交互式计划任务启动容器。默认发布到宿主机 `127.0.0.1:8098`；需要其他地址时传入 `-BindAddress`。已存在源码 staging 目录或同名容器时部署停止。临时任务名输出到终端，验收后可自行移除。

WSLC 的网络登录上下文可能拒绝直接运行容器。脚本使用 `wslc system session run docker` 导入镜像，用交互式用户的 `wslc run` 注册 Windows 端口转发；裸 Docker 的端口映射可能无法替代这一过程。

## 凭据与网络

首次启动生成 `/data/credentials.json`。仅在自己的可信终端读取：

```powershell
wslc --session your-wslc-session exec uni-agent-search cat /data/credentials.json
```

`admin_token` 登录面板；把 `client_token` 单独保存到客户端私有文件并设置 `SAS_TOKEN_FILE`。不要将凭据、数据库或 `master.key` 提交到 Git。备份完整 `sas-data` 数据卷以保留数据库和对应加密密钥。

首次部署可向 `start-wslc.ps1` 传入 `-ProxyURL`、`-DoHURL` 和 `-DataVolume`。后续 update/recreate 从 Docker inspect 保留实际挂载、端口绑定、允许的环境配置（含 Proxy/DoH）、资源限制、标签与重启策略，不再接受会覆盖原设置的 Bind/Proxy/DoH 默认参数。代理地址必须可从容器访问；public fetch 仍禁用代理并校验公开地址。

脚本不再创建包含 native 参数的 transcript，避免代理凭据落入日志。恢复计划的环境项使用 Windows 当前用户 DPAPI 加密的 SecureString，`.clixml` 只能在原机器/用户下读取。不要打印、提交或上传计划文件；备份数据卷仍含数据库与主密钥，必须按敏感数据管理。

## 升级与恢复

先导入并推送新镜像，再在目标机器的交互式 PowerShell 会话中运行：

```powershell
.\update-wslc.ps1 -Session your-wslc-session `
  -Image 127.0.0.1:5000/uni-agent-search:0.2.4
```

升级前 inspect 并验证实际配置，确认新镜像已在本地、备份卷未存在，保存 `recovery-*.clixml`。停止容器时给予 20 秒宽限期，将**实际 `/data` 挂载**（自定义 named volume 或 Linux bind mount）完整复制到新备份卷，再以固定 image ID 和原配置重建。

健康验收同时读取目标容器内 `/readyz` 和 Windows 发布入口，比较实例 ID 与版本；只返回 HTTP 200 不算通过。默认从已保存的首个 Bind 派生 URL，特殊路由可传 `-HealthURL`。镜像必须包含新 `/readyz` 和 Alpine 的 wget/cp/sh 工具；旧镜像仅用于恢复，不可直接作为新健康协议的目标。

任何失败都会尝试恢复旧配置与完整数据快照（清除新 WAL/SHM/残留文件后复制，不做合并），并保留备份。自动恢复失败时，停止客户端、检查容器身份，再由原 Windows 用户运行：

```powershell
.\restore-wslc.ps1 -PlanPath .\recovery-<id>.clixml -ConfirmRestore
```

恢复在停止/删除当前容器前检查备份卷存在，并检查 `search.db`、`master.key`、`credentials.json` 非空；清空目标数据前再次检查这些文件。检查失败时拒绝恢复。此检查不替代备份完整性验证或实际恢复演练。

**恢复会回到升级前快照，丢弃升级后的写入。** 身份变化时恢复脚本拒绝删除容器，需要人工调查。不要删除备份或原镜像；若 WSLC 自身损坏，先恢复 WSLC，再使用计划重试，不要反复运行 update。

支持边界：单一可写 `/data` 挂载、bridge/default 网络、8080/tcp 发布、标准入口和用户、允许的环境项、Memory/NanoCPUs/重启策略。Compose 管理容器、额外挂载、自定义 DNS/权限/入口/CPU quota 等不支持的设置会在停机前拒绝，必须手动迁移，不能假定所有 Docker 配置均可透明重建。

容器使用 Docker `unless-stopped` 策略。WSLC 某些版本可能在 restart 后丢失宿主机端口转发；可通过 `recreate-wslc.ps1 -Session your-wslc-session` 保留数据卷并重新注册端口。`ensure-wslc.ps1` inspect 验证本服务，只在容器已停止时启动原容器；健康/身份失败会报错，**不再自动重建**。确认转发丢失后才显式运行 recreate（同样备份并保真恢复）。它可由用户自行注册为登录任务，不等同于无人登录的系统服务。

验证范围：`deploy/tests/wslc-mock.tests.ps1` 在 Windows PowerShell 5.1 和 CI pwsh 下离线测试实际卷/Bind/Proxy/DoH 保留、身份不符及多阶段失败恢复；parser 检查覆盖全部脚本。新增用例覆盖备份 inspect 空/多条/错误身份，以及三个必需文件缺失或为空的早期/晚期失败。恢复 shell 命令在临时目录中的实际文件上执行，要求 Python 3 和 sh；不会调用 Docker 或访问真实数据卷。未连接真实 WSLC、未做真实卷恢复演练。WSLC preview 的 `--mount`、`--stop-timeout`、image ID 运行与 Docker inspect 形状需先在隔离测试会话验证，不能把 mock 通过当成线上部署成功。

```powershell
wslc --session your-wslc-session list
wslc --session your-wslc-session logs uni-agent-search
wslc --session your-wslc-session inspect --format json uni-agent-search
```
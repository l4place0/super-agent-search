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

如果需要代理或 DoH，可在直接调用 `start-wslc.ps1`、`update-wslc.ps1`、`recreate-wslc.ps1` 时传入 `-ProxyURL` 和 `-DoHURL`，默认均为空。代理地址必须可从容器访问。正文读取仍校验目标为公开地址，阻止私网和保留地址。

## 升级与恢复

先导入并推送新镜像，再在目标机器的交互式 PowerShell 会话中运行：

```powershell
.\update-wslc.ps1 -Session your-wslc-session `
  -Image 127.0.0.1:5000/uni-agent-search:0.2.4
```

升级脚本先停止本服务，备份 `sas-data` 到带时间戳的新卷，再替换容器并检查健康接口。它不自动回滚；失败时保留旧镜像和数据备份。自定义端口或绑定地址时，同时传入 `-Bind` 和对应 `-HealthURL`。

容器使用 Docker `unless-stopped` 策略。WSLC 某些版本可能在 restart 后丢失宿主机端口转发；可通过 `recreate-wslc.ps1 -Session your-wslc-session` 保留数据卷并重新注册端口。`ensure-wslc.ps1` 先探测健康接口，再启动或重建容器；它可由用户自行注册为登录任务，不等同于无人登录的系统服务。

```powershell
wslc --session your-wslc-session list
wslc --session your-wslc-session logs uni-agent-search
wslc --session your-wslc-session inspect --format json uni-agent-search
```
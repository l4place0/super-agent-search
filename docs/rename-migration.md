# Uni Agent Search 改名迁移

## 改名范围

- GitHub 上游：`l4place0/uni-agent-search`，本地 origin 指向新 URL。
- Go module：`uni-agent-search`；产品名称及 User-Agent：Uni Agent Search / UniAgentSearch。
- Skill：`skills/uni-agent-search`，frontmatter `name: uni-agent-search`。
- Skill CLI：`scripts/uas.sh` / `scripts/uas.ps1`。旧脚本路径不再提供，请更新调用方并重新加载 skill 列表。
- 新镜像和 WSLC 容器：`uni-agent-search`；新部署目录默认 `C:\containers\uni-agent-search`。

## 刻意保留的兼容名称

这次是品牌与仓库改名，不是存储格式或 API 版本升级：

- 所有 `SAS_*` 环境变量保持有效，当前并未新增 `UAS_*` 配置。
- `sas-data` 数据卷、`sas-backup-*` 备份卷、`sas-server` 二进制/构建路径和 `sas-image-*.tar` 文件名保留。
- `sas-admin` 浏览器会话存储键保留。
- 本机已安装 skill 的旧私有配置目录保留，只更新 skill 目录和脚本名；令牌不复制进仓库。
- 版本暂留 `0.2.4`；必须重建新品牌镜像，不可把历史 tar 文件当作新镜像包直接上传。
- API 路径、端口、数据库格式、主密钥和令牌均不变。

不要在这次改名中删除数据卷、重新生成 master.key 或 credentials.json。`dist/` 内的旧归档是历史产物，不作为当前版本发布。

## 本地仓库和客户端

```sh
git remote set-url origin https://github.com/l4place0/uni-agent-search.git
```

仓库目录应为 `uni-agent-search`。如果 Windows 编辑器/终端占用旧目录导致无法移动，关闭相关进程，从父目录执行：

```powershell
Rename-Item -LiteralPath .\super-agent-search -NewName uni-agent-search
```

复制完整 `skills/uni-agent-search` 到 Agent 的 skill 目录，移除旧 skill 注册，保留自己的服务器地址与令牌文件配置。更新所有脚本路径后开启新 Agent 会话；已有会话中的 skill 元数据不会自动变化。

## Compose：避免改名后创建空卷

Compose 默认以目录名作为 project name。改目录后直接运行 `docker compose up`，可能创建新的 `<project>_search-data` 卷，看起来像丢失配置。先检查旧容器的 `com.docker.compose.project` 标签和 `/data` 挂载的实际卷名。

已有实例应继续使用**旧 project name**（以下只是默认示例）：

```sh
docker compose -p super-agent-search up -d --build
```

也可以在本机私有 `.env` 设置 `COMPOSE_PROJECT_NAME` 为已确认的原值。不要执行 `down -v`。新部署不需要沿用旧 project name。

## WSLC：旧容器到新容器

本次代码更新不自动操作线上实例。迁移前：

1. 检查现有容器确实为本服务，确认 `/data` 挂载的是 `sas-data`。目前升级脚本只支持这个默认卷；自定义卷不要直接使用。
2. 按 `deploy/README.md` 重新构建、导入并推送 `uni-agent-search:0.2.4` 镜像；记录旧镜像引用。
3. 在维护窗口执行升级。脚本会停旧实例、备份同一个 `sas-data` 卷、删除旧容器、用新名称启动容器。第一次从旧名称迁移时显式传参：

```powershell
.\deploy\update-wslc.ps1 -Session your-wslc-session `
  -ContainerName super-agent-search `
  -Image 127.0.0.1:5000/uni-agent-search:0.2.4
```

自定义监听、健康检查地址、代理和 DoH 时，必须补上原配置的 `-Bind`、`-HealthURL`、`-ProxyURL`、`-DoHURL`。后续升级使用默认容器名即可。

4. 更新旧计划任务/看门狗指向新发布目录中的脚本，避免旧脚本重新创建旧容器。
5. 验证 health、Client capabilities、一次搜索、面板中原 provider/key 数量和日志；不要打印凭据。
6. 升级脚本不自动回滚。失败时保留备份，停止新容器，使用旧脚本和旧镜像恢复；需要恢复卷时先停机并完整恢复数据库及 master.key。禁止两个实例同时使用该卷提供服务。

只给旧容器执行 `docker rename` 不足以完成迁移：其 `app` label 仍是旧值，新升级脚本会拒绝它。使用上述备份重建流程。

# ARTEX 本地 Docker 部署与开发指南

首次部署：2026-10-13。文档整理及运行状态复核：2026-10-15。

本文记录 Apple Silicon 上已经走通的原生 arm64 部署过程、构建问题、数据保留边界和 fork 开发方式。它是部署记录，不代表模型调用、扫描效果或生产环境已验收。

## 1. 仓库与分支约定

| 名称 | 用途 |
| --- | --- |
| `origin` | 个人 fork：`https://github.com/CharonNana/ARTEX.git` |
| `upstream` | 官方仓库：`https://github.com/mhtsec/ARTEX.git` |
| `main` | 官方基线，只接收上游更新 |
| `charon/main` | 个人开发主分支，保存部署文档和后续改造 |

`charon/main` 是个人开发的长期分支，不代表修改 GitHub 仓库的默认分支。后续功能可以从它创建 `feature/<name>` 分支，再合并回来。

本文档提交只包含文档。安装时的 Dockerfile 修正、`Dockerfile.dockerignore` 和 `docker-compose.override.yml` 目前仍是本地未提交改动；从远程重新克隆后，需要按下文准备这些内容，不能假定它们已在远程代码中。

## 2. 已确认的环境与部署选择

- 本机是 Apple Silicon，Docker 服务端运行 `linux/arm64`。
- 2026-10-15 复核：Docker 客户端与服务端版本均为 `29.7.2`，Compose 为 `v5.3.1`。
- 2026-10-13 检查的 `mianhuatang11/artex:latest` 镜像仅提供 amd64，因此选择源码构建原生 arm64 镜像；这是当时的镜像快照，不保证后续 tag 不变。
- 应用镜像命名为 `artex:local-arm64`，数据库使用 `postgres:16-alpine`。
- 8787 是网页/API，8788 是流量录制代理；两者均只绑定 `127.0.0.1`。
- 前端构建、Go 编译、工具及浏览器安装全部在 Docker 内完成，宿主机无需安装 Go 或 Node.js。本文用于生成本地配置的辅助命令使用宿主机 Python 3。

## 3. 首次部署步骤

### 3.1 获取源码并检查环境

```bash
git clone https://github.com/CharonNana/ARTEX.git
cd ARTEX
git remote add upstream https://github.com/mhtsec/ARTEX.git
git switch charon/main

docker version
docker compose version
uname -m
lsof -nP -iTCP:8787 -iTCP:8788 -sTCP:LISTEN
```

以上克隆命令用于新机器；现有安装不要再次克隆、创建 remote 或切换部署目录。`charon/main` 需要已发布到 fork 后才能在新机器检出；尚未发布时，可先执行 `git switch -c charon/main`。

### 3.2 准备数据库密码和环境变量

仅在 `.env` 不存在时执行，避免覆盖已有数据库密码：

```bash
python3 - <<'PY'
from pathlib import Path
import os
import secrets

target = Path('.env')
body = Path('.env.example').read_text().replace(
    'POSTGRES_PASSWORD=change-me-please',
    'POSTGRES_PASSWORD=' + secrets.token_hex(24),
)
fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as out:
    out.write(body)
PY
```

不要输出或提交密码。当前 Compose 把密码直接拼进 PostgreSQL 连接 URL，十六进制密码可以避免 `@`、`:`、`/` 等字符的 URL 转义问题。

首次安装可以暂不填写 LLM API Key，服务可以启动；实际使用模型前，需要在界面或环境变量中配置。已有数据库卷时，不要把修改 `.env` 当作数据库密码轮换：PostgreSQL 初始化变量不会自动修改现有数据库账号密码。

### 3.3 准备本地 Compose 覆盖配置

创建 `docker-compose.override.yml`：

```yaml
services:
  artex:
    image: artex:local-arm64
    platform: linux/arm64
    ports: !override
      - "127.0.0.1:8787:8787"
      - "127.0.0.1:8788:8788"
```

Compose 会自动合并该文件与仓库的 `docker-compose.yml`。`!override` 用于完全替换原端口列表，避免保留原来的 `8787:8787` 对外监听；本机 Compose 已验证支持该标记。

继续保留官方配置中的 PostgreSQL 数据卷、`./data` 和 `./skills` 挂载，不在宿主机映射数据库端口。

### 3.4 修正 Dockerfile 的两个已知构建问题

**本地 norma 模块必须先复制模块清单。** 根 `go.mod` 使用 `replace github.com/Autumn-27/norma => ./norma`，所以 Go 构建阶段需要采用以下顺序：

```dockerfile
COPY go.mod go.sum ./
COPY norma/go.mod norma/go.sum ./norma/
RUN go mod download
COPY . .
```

**浏览器使用 Playwright 官方默认下载源。** 将原来的：

```dockerfile
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_DOWNLOAD_HOST=https://cdn.npmmirror.com/binaries/playwright
```

改为：

```dockerfile
ENV npm_config_registry=$NPM_REGISTRY
```

npm 仍使用配置的镜像源；取消浏览器下载地址覆盖后，Playwright 使用自身的官方下载地址。

### 3.5 排除构建上下文中的密钥

创建 `Dockerfile.dockerignore`，先复制仓库 `.dockerignore` 的全部规则，再追加以下规则：

```text
.env
.env.*
docker-compose.override.yml
```

Dockerfile 专用的忽略文件会替代根 `.dockerignore` 的规则，因此不能只写追加的三行。保留原规则，确保 `.git`、`data/`、`node_modules` 等仍被排除。

原 `.dockerignore` 没有排除根 `.env`。虽然最终运行镜像没有复制整个源码目录，但中间 Go 构建阶段会执行 `COPY . .`，所以必须防止密钥进入中间镜像和构建缓存。

### 3.6 构建、启动与验收

```bash
docker compose config --quiet
docker compose pull postgres
docker compose build artex
docker image inspect artex:local-arm64 --format '{{.Os}}/{{.Architecture}}'
docker compose up -d --pull never

docker compose ps
docker compose exec -T postgres pg_isready -U artex -d artex
curl --fail http://127.0.0.1:8787/api/health
curl --fail http://127.0.0.1:8787/api/auth/status
curl --fail -o /dev/null http://127.0.0.1:8787/setup
docker inspect artex-artex-1 artex-postgres-1 \
  --format '{{.Name}} status={{.State.Status}} restarts={{.RestartCount}}'
```

镜像架构应为 `linux/arm64`，PostgreSQL 应 healthy/accepting connections，接口应返回 HTTP 200。首次访问 `http://localhost:8787/setup` 设置管理员密码；已有管理员时，`/api/auth/status` 返回 `initialized: true` 是正常状态，不要重新初始化账号。

`/setup` 返回 200 只证明返回了 HTML，不等同于完成浏览器交互验证。查看应用日志时，注意不要把包含密钥、模型配置或任务敏感内容的日志直接公开。

## 4. 实际踩过的坑

| 问题 | 实际表现 | 处理方式 |
| --- | --- | --- |
| 预构建镜像架构不匹配 | 当时的 `latest` 只有 amd64，本机为 arm64 | 使用原生源码构建，明确指定 `linux/arm64` |
| norma 复制时机错误 | `go mod download` 报 `/src/norma/go.mod: no such file or directory` | 在下载依赖前复制 norma 的 `go.mod` 和 `go.sum` |
| Debian 软件源连接超时 | Playwright 安装系统依赖时无法连接 `deb.debian.org:80` | 利用 Docker 已完成层的缓存重试；当次重试成功，没有改软件源 |
| Playwright 镜像源缺文件 | arm64 Chrome 下载反复返回 404 / `NoSuchKey` | 移除 `PLAYWRIGHT_DOWNLOAD_HOST`，官方下载成功 |
| `.env` 未排除 | 密码存在被 `COPY . .` 带入编译层的风险 | 使用保留原规则的 `Dockerfile.dockerignore` 排除密钥 |
| 原网页端口对外绑定 | `8787:8787` 未限定宿主机地址 | 用 `!override` 将端口替换为 `127.0.0.1` 绑定 |
| 重启容器不等于更新代码 | `docker compose restart` 不会重新编译源码 | 先 build，再 up 更新应用容器 |

不要默认使用 `--no-cache`，否则已成功的前端、Go 和工具层也要重跑。下载失败要先看具体错误，网络超时与镜像源 404 的处理方式不同。

## 5. 修改源码后的部署与数据保留

数据库已经运行时，只更新应用：

```bash
docker compose build artex
docker compose up -d --no-deps --pull never artex
docker compose logs --tail=100 artex
```

旧容器在新镜像构建期间可以继续运行；执行 `up` 更新容器时会短暂中断服务和运行中的任务，应选择空闲时间。数据库未运行时，先启动 PostgreSQL，或不加 `--no-deps` 让 Compose 处理依赖。

| 数据 | 当前实际存储位置 | 重建应用容器后 |
| --- | --- | --- |
| PostgreSQL 数据，含管理员密码哈希、LLM 配置等 | Docker 数据卷 `artex_pgdata` | 保留，前提是卷未删除且新代码未破坏数据 |
| 工作目录数据 | 宿主机 `ARTEX/data` → `/app/data` | 保留 |
| Skills | 宿主机 `ARTEX/skills` → `/app/skills` | 保留；文件直接挂载，无需重新打包才能读取新内容 |
| JWT 签名密钥 | 容器内 `/app/jwt.key` | 当前未持久化，重建后会重新生成，原登录会话失效 |
| 其他未挂载路径中的临时文件和手工安装内容 | 容器可写层 | 不保留 |

普通停止可用 `docker compose stop`。保留数据时，不执行 `docker compose down -v`，不删除 `artex_pgdata` 或宿主机 `data/`。保持 Compose 项目名称和部署目录稳定；改名或用不同的 `-p` 可能连接到另一个空数据卷。

数据库卷保留不等于任意源码改动都不会影响数据。应用启动会执行数据库初始化/迁移逻辑；修改表结构、迁移、清理或删除逻辑前必须备份并验证兼容性。回退镜像不会自动回退数据库。

JWT 密钥未持久化是已知限制，不代表管理员密码丢失：密码哈希在 PostgreSQL 中，通常只需重新登录。后续如修复密钥持久化，应挂载到工作区之外，不能简单放回可浏览下载的 `/app/data` 中。

### 更新前备份建议

以下是建议流程，本文整理时未实际执行备份或恢复演练。仅在没有运行中任务、数据库已经启动时使用；任一步失败都应停止并检查，不要继续部署。

```bash
set -e
umask 077
backup_dir=$(mktemp -d "$HOME/artex-backup.XXXXXX")
docker compose stop artex
docker compose exec -T postgres pg_dump -U artex -d artex -Fc > "$backup_dir/postgres.dump"
tar -czf "$backup_dir/local-files.tar.gz" \
  .env data skills docker-compose.override.yml Dockerfile.dockerignore
docker cp artex-artex-1:/app/jwt.key "$backup_dir/jwt.key"
chmod 600 "$backup_dir"/*
docker compose start artex
```

这些备份包含数据库、密钥及可能的任务敏感数据，应保存在仓库之外，不提交、不公开。此流程假定 PostgreSQL 用户和库名仍为默认 `artex`；若已改过 `.env`，需相应调整。备份失败时应用可能仍处于停止状态，确认原因后可以使用 `docker compose start artex` 恢复原容器。

## 6. 个人开发与官方更新同步

开始同步前先提交开发分支的修改；本机部署配置和密钥不要用 `git add .` 顺带提交。

```bash
git fetch upstream
git switch main
git merge --ff-only upstream/main
git push origin main

git switch charon/main
git merge main
# 有冲突时先解决冲突，再验证相关功能。
git push -u origin charon/main
```

`main` 保持官方基线，个人提交保留在 `charon/main`。使用普通 merge 保留双方历史，不需要 rebase 或强制推送。Git 合并并不会自动部署，验证通过后再构建和更新 Docker 容器。

准备发布时逐个指定文件执行 `git add`，检查 `git diff --cached`。任何 API Key、`.env`、JWT 密钥、数据库备份和任务数据都不应进入 Git。

GitHub MCP 的 `push_files` / `create_or_update_file` 会在远程创建新提交，不能直接上传本地已有的 commit。要让本地和远程保留同一个提交 SHA，应使用普通 `git push`，再通过 GitHub MCP 或远程引用回读核验；MCP 登录成功、能读仓库，不等同于已验证 Git 推送权限。

## 7. 验证记录与边界

2026-10-13 的部署验收记录：

- 原生镜像 `linux/arm64` 构建成功，PostgreSQL healthy，两个容器重启次数均为 0。
- `/api/health`、`/api/auth/status`、`/setup` 返回 HTTP 200，当时管理员尚未初始化。
- 容器内 Chromium 无头启动测试通过，版本 `156.0.8078.4`。
- 当次未配置模型、未执行扫描，也未执行数据恢复演练。

2026-10-15 的只读复核：

- 两个容器仍 running，PostgreSQL healthy，重启次数均为 0；镜像仍为 `linux/arm64`。
- 三个 HTTP 地址返回 200，管理员状态已为 `initialized: true`。
- 实际挂载仍为 `artex_pgdata`、`./data` 和 `./skills`；JWT 路径仍未挂载。
- 本次仅整理文档和 Git 分支，没有重建、重启或修改应用数据；未重新验证 Chromium、模型调用或扫描效果。

代码依据：`docker-compose.yml`、本地覆盖配置、`Dockerfile`、`Dockerfile.dockerignore`、`server/auth.go`、`cmd/artex/main.go`、`db/db.go` 及 `db/schema.sql`。历史构建问题来自首次部署日志，当前状态来自只读容器及 HTTP 检查。

# ARTEX 本地 Docker 无损升级手册

整理日期：2026-10-15。适用环境：本机 Apple Silicon、现有 Compose 项目 `artex`、个人开发分支 `charon/main`。初装步骤见 [本地 Docker 部署与开发指南](本地Docker部署与开发指南.md)；其中 JWT 未持久化、备份未演练的描述是升级前的历史状态，后续升级以本文为准。

## 1. 适用范围与保留边界

本文用于更新 Go 程序和内嵌前端，同时保留 PostgreSQL、工作目录、skills、JWT 密钥以及旧容器中手工安装的 Python 包和其他文件。流程为：**在线编译 → 确认空闲 → 停应用 → 一致备份 → 独立恢复验证 → 基于旧容器环境替换程序 → 切换应用 → 核对数据**。

这不是零停机升级：编译时旧服务继续运行，备份至切换期间应用暂停。整个过程中数据库保持运行，禁止同时从其他窗口操作应用、修改数据、提交代码或部署其他版本。

| 内容 | 存储位置 / 处理方式 |
| --- | --- |
| PostgreSQL | 保留原容器和 `artex_pgdata` 卷，同时 `pg_dump -Fc` 备份 |
| 工作目录、流量、会话文件 | 保留 `/Users/cc/code/ARTEX/data`，停应用后归档及计算哈希 |
| Skills | 保留 `/Users/cc/code/ARTEX/skills`，同时归档 |
| JWT | `/Users/cc/.local/share/artex/jwt.key` → `/app/jwt.key`，只读挂载 |
| 容器可写层中的工具及文件 | `docker commit` 保存原环境，新镜像只替换 `/app/artex` |
| 数据库账号、密码、LLM 配置 | 保留 `.env` 和原数据库内容，不重新初始化 |

仅替换程序的方式不会应用 Dockerfile 运行阶段的新依赖、新启动脚本或其他运行环境改动。若本次涉及这些内容、数据库 schema/启动迁移、存储路径或 shared contract，先单独评估兼容性，不能直接套用下面的切换步骤。镜像回退不会撤销已执行的数据库迁移。

备份目录和快照镜像含有数据库密码、JWT 密钥及业务数据，只保存在本地，不上传 Git 或镜像仓库。不要把备份和密钥放到可通过应用浏览的 `data/` 中。

## 2. 准备变量与检查部署来源

以下命令在**同一个 Bash 终端**中按顺序执行。命令失败即停止，不越过失败步骤继续；若失败发生在停应用之后、切换之前，可按第 10 节启动原容器恢复服务。新开终端时先重新设置变量，不要误用新时间戳寻找旧备份。

```bash
set -euo pipefail
umask 077
export ARTEX_ROOT=/Users/cc/code/ARTEX
cd "$ARTEX_ROOT"
test "$(git branch --show-current)" = charon/main
git status --short --branch
git log -3 --oneline
python3 - <<'PY'
import subprocess
assert not subprocess.check_output(["git", "diff", "--cached", "--name-only"]), "暂存区非空，先处理"
changed = subprocess.check_output(["git", "diff", "--name-only"], text=True).splitlines()
assert set(changed) <= {"docker-compose.override.yml"}, "存在未提交源码改动，停止部署"
assert not subprocess.check_output(["git", "ls-files", "--others", "--exclude-standard"]), "存在未跟踪文件，先核对构建上下文"
PY
docker compose config --quiet
docker compose ps

export ARTEX_REV=$(git rev-parse HEAD)
ARTEX_SHORT=$(git rev-parse --short=12 HEAD)
ARTEX_STAMP=$(TZ=Asia/Shanghai date +%Y%m%d-%H%M%S)
export ARTEX_BACKUP="/Users/cc/artex-backups/$ARTEX_STAMP"
export ARTEX_BUILD="artex:upgrade-build-$ARTEX_SHORT-$ARTEX_STAMP"
export ARTEX_ROLLBACK="artex:rollback-$ARTEX_STAMP"
export ARTEX_UPGRADE="artex:upgrade-$ARTEX_STAMP"
export ARTEX_VERIFY="artex-upgrade-verify-$ARTEX_STAMP"
export ARTEX_APP=$(docker compose ps -q artex)
export ARTEX_PG=$(docker compose ps -q postgres)
test -n "$ARTEX_APP"
test -n "$ARTEX_PG"
export ARTEX_PG_USER=$(docker exec "$ARTEX_PG" printenv POSTGRES_USER)
export ARTEX_PG_DB=$(docker exec "$ARTEX_PG" printenv POSTGRES_DB)
mkdir -p "$ARTEX_BACKUP"
chmod 700 "$ARTEX_BACKUP"

# 输出文件含容器环境变量，不能直接公开。
docker inspect "$ARTEX_APP" "$ARTEX_PG" > "$ARTEX_BACKUP/containers-before.json"
docker inspect "$ARTEX_APP" --format '{{.Image}}' > "$ARTEX_BACKUP/original-image-id"
docker inspect "$ARTEX_PG" --format '{{.Id}}' > "$ARTEX_BACKUP/postgres-container-id"
git rev-parse HEAD > "$ARTEX_BACKUP/source-commit"
docker image inspect "$(cat "$ARTEX_BACKUP/original-image-id")" \
  --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'
```

核对当前改动：允许已审查的本机 `docker-compose.override.yml` 配置差异，应用源码必须与目标提交一致；暂存区应为空。不要自动 stash、切分支、拉取或合并代码。拉取和解决冲突应在部署前完成，部署流程不负责同步上游。

检查实际挂载仍是本机 `data/`、`skills/` 和 `artex_pgdata`，8787/8788 仍仅监听 `127.0.0.1`。若原镜像有提交标签，用它与 `$ARTEX_REV` 比较改动，特别检查 `Dockerfile`、`start.sh`、`db/`、认证和数据存储逻辑。无提交标签时人工确认原版本，不能推断兼容。

## 3. 在线编译新版程序

```bash
# 此时不停止应用；只构建 Go + 内嵌前端，不重复安装运行阶段的工具。
docker build --target gobuild --platform linux/arm64 \
  -t "$ARTEX_BUILD" --progress plain .
docker image inspect "$ARTEX_BUILD" --format '{{.Os}}/{{.Architecture}}'
test "$(git rev-parse HEAD)" = "$ARTEX_REV"
```

架构应为 `linux/arm64`。构建过程应完成前端构建、TypeScript 检查和 Go 编译。不要默认加 `--no-cache`；不要直接拉取 `latest` 覆盖个人代码镜像。

构建前核对 `Dockerfile.dockerignore` 排除了 `.env`、`.env.*`、data 和其他本地密钥。Dockerfile 专用忽略文件会替代根 `.dockerignore`，不能只保留追加规则。JWT 位于仓库外，不进入构建上下文。

## 4. 准备可重复使用的核对工具

将以下代码保存到本次备份目录。它只读取数据库/文件，并输出计数与哈希，不输出业务原文。需要 Python 3.11+。数据库哈希用于比较快照内容，不作为安全签名。

```bash
cat > "$ARTEX_BACKUP/snapshot.py" <<'PY'
import hashlib
import json
import os
import pathlib
import subprocess
import sys

kind, target, output = sys.argv[1:]
if kind == "db":
    prefix = ["docker", "exec", "-i", target, "psql", "-X", "-v", "ON_ERROR_STOP=1",
              "-U", os.environ["ARTEX_PG_USER"], "-d", os.environ["ARTEX_PG_DB"], "-At"]
    tables = subprocess.check_output(prefix + ["-c", "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename"], text=True).splitlines()
    queries = []
    for table in tables:
        assert table.replace("_", "").isascii() and table.replace("_", "").isalnum()
        queries.append(f"SELECT json_build_object('table','{table}','count',count(*),'hash',md5(coalesce(string_agg(h,'' ORDER BY h),'')),'normalized_hash',md5(coalesce(string_agg(n,'' ORDER BY n),''))) FROM (SELECT md5(row_to_json(t)::text) h, md5((to_jsonb(t)-'updated_at')::text) n FROM public.\"{table}\" t) q;")
    sql = "BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;\n" + "\n".join(queries) + "\nCOMMIT;"
    lines = subprocess.check_output(prefix, input=sql, text=True).splitlines()
    rows = [json.loads(line) for line in lines if line.startswith("{")]
    result = {row["table"]: row for row in rows}
    assert len(result) == len(tables)
elif kind == "files":
    root = pathlib.Path(target)
    result = {}
    for folder in ["data", "skills"]:
        assert (root / folder).is_dir()
        for path in sorted((root / folder).rglob("*")):
            name = str(path.relative_to(root))
            if path.is_symlink():
                result[name] = {"link": str(path.readlink())}
            elif path.is_file():
                with path.open("rb") as source:
                    digest = hashlib.file_digest(source, "sha256").hexdigest()
                result[name] = {"sha256": digest, "size": path.stat().st_size}
else:
    raise ValueError("kind 必须为 db 或 files")
path = pathlib.Path(output)
path.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
path.chmod(0o600)
print(f"{kind}: {len(result)} 项，快照已保存")
PY
```

## 5. 确认空闲，停应用并取得一致备份

数据库中的运行状态检查不能替代界面检查：还需确认没有进行中的对话、主 Agent 操作和流量录制，暂停外部流量客户端，备份窗口内不要再操作应用。

```bash
ARTEX_BUSY=$(docker exec "$ARTEX_PG" psql -X -v ON_ERROR_STOP=1 \
  -U "$ARTEX_PG_USER" -d "$ARTEX_PG_DB" -At -c "
SELECT (SELECT count(*) FROM tasks WHERE status='running' OR queued)
     + (SELECT count(*) FROM side_question_requests WHERE status='running')
     + (SELECT count(*) FROM finding_retests WHERE status IN ('pending','running'))
     + (SELECT count(*) FROM task_archives WHERE state NOT IN ('archived','restored','failed','cancelled'));")
test "$ARTEX_BUSY" = 0

docker exec "$ARTEX_APP" python -c 'import importlib.metadata,json; print(json.dumps(sorted((d.metadata["Name"],d.version) for d in importlib.metadata.distributions())))' \
  > "$ARTEX_BACKUP/python-packages-before.json"
docker cp "$ARTEX_APP:/app/jwt.key" "$ARTEX_BACKUP/jwt.key"
test "$(wc -c < "$ARTEX_BACKUP/jwt.key" | tr -d ' ')" = 32
docker compose stop -t 60 artex
test "$(docker inspect "$ARTEX_APP" --format '{{.State.Status}}')" = exited
test "$(docker inspect "$ARTEX_APP" --format '{{.State.ExitCode}}')" = 0

python3 "$ARTEX_BACKUP/snapshot.py" db "$ARTEX_PG" "$ARTEX_BACKUP/database-before.json"
python3 "$ARTEX_BACKUP/snapshot.py" files "$ARTEX_ROOT" "$ARTEX_BACKUP/files-before.json"
docker exec "$ARTEX_PG" pg_dump -U "$ARTEX_PG_USER" -d "$ARTEX_PG_DB" -Fc \
  > "$ARTEX_BACKUP/postgres.dump"
docker exec "$ARTEX_PG" pg_dumpall -U "$ARTEX_PG_USER" --globals-only \
  > "$ARTEX_BACKUP/postgres-globals.sql"
tar -czf "$ARTEX_BACKUP/local-files.tar.gz" \
  .env data skills docker-compose.yml docker-compose.override.yml Dockerfile Dockerfile.dockerignore
tar -tzf "$ARTEX_BACKUP/local-files.tar.gz" > /dev/null

# commit 不包含挂载卷，所以数据库、data 和 skills 必须单独备份。
docker commit "$ARTEX_APP" "$ARTEX_ROLLBACK"
docker image save "$ARTEX_ROLLBACK" > "$ARTEX_BACKUP/rollback-image.tar"
shasum -a 256 "$ARTEX_BACKUP/postgres.dump" "$ARTEX_BACKUP/local-files.tar.gz" \
  "$ARTEX_BACKUP/rollback-image.tar" > "$ARTEX_BACKUP/SHA256SUMS"
```

不要只备份当前镜像 tag：手工 pip 安装及其他容器可写层改动不在原镜像里。`docker commit` 加 `docker image save` 才保存了这部分环境；数据库逻辑备份和文件归档则保存挂载内容。

## 6. 验证数据库能恢复，文件归档内容正确

使用原 PostgreSQL 镜像启动一个无网络、不映射端口、数据位于 tmpfs 的独立容器。恢复目标必须是 `$ARTEX_VERIFY`，不能填现有 PostgreSQL 容器。

```bash
ARTEX_PG_IMAGE=$(docker inspect "$ARTEX_PG" --format '{{.Image}}')
docker run -d --name "$ARTEX_VERIFY" --network none \
  --tmpfs /var/lib/postgresql/data:rw \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -e POSTGRES_USER="$ARTEX_PG_USER" -e POSTGRES_DB="$ARTEX_PG_DB" \
  "$ARTEX_PG_IMAGE"
ARTEX_READY=0
for attempt in $(seq 1 30); do
  if docker exec "$ARTEX_VERIFY" pg_isready -U "$ARTEX_PG_USER" -d "$ARTEX_PG_DB" > /dev/null 2>&1; then
    ARTEX_READY=1
    break
  fi
  sleep 1
done
test "$ARTEX_READY" = 1
docker exec -i "$ARTEX_VERIFY" pg_restore --exit-on-error --no-owner --no-acl \
  -U "$ARTEX_PG_USER" -d "$ARTEX_PG_DB" < "$ARTEX_BACKUP/postgres.dump"
python3 "$ARTEX_BACKUP/snapshot.py" db "$ARTEX_VERIFY" "$ARTEX_BACKUP/database-restored.json"
cmp "$ARTEX_BACKUP/database-before.json" "$ARTEX_BACKUP/database-restored.json"
docker stop "$ARTEX_VERIFY"

python3 - <<'PY'
import hashlib, json, os, pathlib, tarfile
backup = pathlib.Path(os.environ["ARTEX_BACKUP"])
expected = json.loads((backup / "files-before.json").read_text())
checked = set()
# 顺序读取 gzip，避免对每个文件反复从头解压。
with tarfile.open(backup / "local-files.tar.gz", "r|gz") as archive:
    for member in archive:
        if member.name not in expected:
            continue
        item = expected[member.name]
        if "link" in item:
            assert member.issym() and member.linkname == item["link"], member.name
        else:
            assert member.isfile(), member.name
            with archive.extractfile(member) as source:
                assert hashlib.file_digest(source, "sha256").hexdigest() == item["sha256"], member.name
        checked.add(member.name)
assert checked == expected.keys(), "归档缺少原文件"
print(f"归档校验通过：{len(checked)} 个文件及链接")
PY
shasum -a 256 -c "$ARTEX_BACKUP/SHA256SUMS"
```

恢复验证结束后停止临时容器，tmpfs 数据随之释放；保留停止的容器不影响正式数据。后续清理应单独确认具体对象，不执行全局 prune。`pg_restore --list` 只能说明备份目录能读取，不能替代真实恢复和内容比较。

## 7. 保留原 JWT 并确认挂载

当前本机已完成本节。后续升级仍须比较密钥；新机器或旧配置才需要补挂载。

```bash
python3 - <<'PY'
import os, pathlib
old = pathlib.Path(os.environ["ARTEX_BACKUP"]) / "jwt.key"
target = pathlib.Path("/Users/cc/.local/share/artex/jwt.key")
assert len(old.read_bytes()) == 32
target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
target.parent.chmod(0o700)
if target.exists():
    assert not target.is_symlink() and target.read_bytes() == old.read_bytes(), "已有密钥不同，停止，不能覆盖"
else:
    with target.open("xb") as out:
        out.write(old.read_bytes())
target.chmod(0o600)
PY
```

在 `docker-compose.override.yml` 的 `services.artex.volumes` 下保留以下条目，合并现有配置，不覆盖其他挂载或端口设置：

```yaml
volumes:
  - type: bind
    source: /Users/cc/.local/share/artex/jwt.key
    target: /app/jwt.key
    read_only: true
    bind:
      create_host_path: false
```

`create_host_path: false` 可以避免文件缺失时 Docker 自动创建同名目录。不要重新生成密钥；原 JWT 不变才能保留已有登录会话。容器重建后挂载密钥应与备份逐字节相同。

## 8. 保存回退命令，打包并切换应用

在停机窗口内构建一个很小的新镜像层：以刚才的容器快照为基底，从编译镜像中复制新程序。不使用标准运行阶段覆盖旧环境。

```bash
mkdir "$ARTEX_BACKUP/image-context"
cat > "$ARTEX_BACKUP/image-context/Dockerfile" <<EOF
FROM $ARTEX_ROLLBACK
COPY --from=$ARTEX_BUILD /out/artex /app/artex
LABEL org.opencontainers.image.revision="$ARTEX_REV"
EOF
docker build --platform linux/arm64 -t "$ARTEX_UPGRADE" \
  --progress plain "$ARTEX_BACKUP/image-context"
docker image inspect "$ARTEX_UPGRADE" --format '{{.Os}}/{{.Architecture}}'

# 将本次变量写入回退脚本，后续无需重新计算时间戳。
cat > "$ARTEX_BACKUP/rollback.sh" <<EOF
#!/bin/sh
set -eu
cd "$ARTEX_ROOT"
tar -xOf "$ARTEX_BACKUP/local-files.tar.gz" docker-compose.override.yml > docker-compose.override.yml
docker tag "$ARTEX_ROLLBACK" artex:local-arm64
docker compose up -d --no-deps --no-build --pull never artex
EOF
chmod 700 "$ARTEX_BACKUP/rollback.sh"
sh -n "$ARTEX_BACKUP/rollback.sh"

# 确认备份后没有其他写入或源码切换。
python3 "$ARTEX_BACKUP/snapshot.py" db "$ARTEX_PG" "$ARTEX_BACKUP/database-pre-switch.json"
python3 "$ARTEX_BACKUP/snapshot.py" files "$ARTEX_ROOT" "$ARTEX_BACKUP/files-pre-switch.json"
cmp "$ARTEX_BACKUP/database-before.json" "$ARTEX_BACKUP/database-pre-switch.json"
cmp "$ARTEX_BACKUP/files-before.json" "$ARTEX_BACKUP/files-pre-switch.json"
test "$(git rev-parse HEAD)" = "$ARTEX_REV"
docker compose config --quiet

docker tag "$ARTEX_UPGRADE" artex:local-arm64
docker compose up -d --no-deps --no-build --pull never artex
ARTEX_HEALTHY=0
for attempt in $(seq 1 30); do
  if curl --fail --silent --max-time 2 http://127.0.0.1:8787/api/health > "$ARTEX_BACKUP/health-after.json"; then
    ARTEX_HEALTHY=1
    break
  fi
  sleep 1
done
test "$ARTEX_HEALTHY" = 1
docker compose ps
```

`--no-deps` 保留数据库容器，`--no-build --pull never` 防止 Compose 临时换成其他镜像。健康检查失败时不宣告完成，先检查本地日志；符合第 10 节条件时回退。

## 9. 核对数据、文件、认证和运行环境

```bash
ARTEX_NEW_APP=$(docker compose ps -q artex)
test "$(docker compose ps -q postgres)" = "$(cat "$ARTEX_BACKUP/postgres-container-id")"
docker inspect "$ARTEX_NEW_APP" "$ARTEX_PG" \
  --format '{{.Name}} status={{.State.Status}} restarts={{.RestartCount}}'
docker image inspect artex:local-arm64 \
  --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'
docker exec "$ARTEX_NEW_APP" cat /app/jwt.key | cmp - "$ARTEX_BACKUP/jwt.key"
docker exec "$ARTEX_NEW_APP" python -c 'import importlib.metadata,json; print(json.dumps(sorted((d.metadata["Name"],d.version) for d in importlib.metadata.distributions())))' \
  > "$ARTEX_BACKUP/python-packages-after.json"
cmp "$ARTEX_BACKUP/python-packages-before.json" "$ARTEX_BACKUP/python-packages-after.json"
python3 "$ARTEX_BACKUP/snapshot.py" db "$ARTEX_PG" "$ARTEX_BACKUP/database-after.json"
python3 "$ARTEX_BACKUP/snapshot.py" files "$ARTEX_ROOT" "$ARTEX_BACKUP/files-after.json"

python3 - <<'PY'
import json, os, pathlib
p = pathlib.Path(os.environ["ARTEX_BACKUP"])
before = json.loads((p / "database-before.json").read_text())
after = json.loads((p / "database-after.json").read_text())
assert before.keys() == after.keys(), "数据库表集合改变，需人工检查"
changed = {name: {"before_count": old["count"], "after_count": after[name]["count"],
                  "only_updated_at_changed": old["normalized_hash"] == after[name]["normalized_hash"]}
           for name, old in before.items() if old != after[name]}
print("数据库差异：", json.dumps(changed, ensure_ascii=False))
unexpected = [name for name, diff in changed.items()
              if not ((name in {"agents", "mcp_servers"} and diff["only_updated_at_changed"] and diff["before_count"] == diff["after_count"])
                      or (name == "server_logs" and diff["after_count"] >= diff["before_count"]))]
assert not unexpected, f"出现本次流程之外的数据变更，停止验收并检查：{unexpected}"
files_before = json.loads((p / "files-before.json").read_text())
files_after = json.loads((p / "files-after.json").read_text())
missing_or_changed = [name for name, old in files_before.items() if files_after.get(name) != old]
print("新增文件：", sorted(files_after.keys() - files_before.keys()))
assert not missing_or_changed, f"原文件消失或改变，需检查：{missing_or_changed}"
(p / "database-differences.json").write_text(json.dumps(changed, ensure_ascii=False, indent=2) + "\n")
PY
curl --fail --silent http://127.0.0.1:8787/api/auth/status
curl --fail --silent http://127.0.0.1:8787/ > "$ARTEX_BACKUP/index-after.html"
```

不能只比较总行数。逐项解释数据库差异：此次实测仅 `agents`、`mcp_servers` 的 `updated_at` 更新，以及 `server_logs` 新增启动日志；其他表完整哈希不变。对新增日志，比较升级前日志集合是否全部保留，不能仅凭日志数量增加推断无丢失。`normalized_hash` 排除 `updated_at`，仅用于识别更新时间变化，不能代替完整哈希。

若本次仍只新增日志，可用下列命令确认原日志完整保留；若日志清理策略或排序规则变更，应停止沿用此比较方式并单独核对：

```bash
python3 - <<'PY'
import json, os, pathlib, subprocess
p = pathlib.Path(os.environ["ARTEX_BACKUP"])
old = json.loads((p / "database-before.json").read_text())["server_logs"]
sql = "SELECT json_build_object('count',count(*),'hash',md5(coalesce(string_agg(h,'' ORDER BY h),''))) FROM (SELECT md5(row_to_json(t)::text) h FROM (SELECT * FROM server_logs ORDER BY id LIMIT " + str(old["count"]) + ") t) q;"
actual = json.loads(subprocess.check_output(["docker", "exec", os.environ["ARTEX_PG"], "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", os.environ["ARTEX_PG_USER"], "-d", os.environ["ARTEX_PG_DB"], "-At", "-c", sql]))
assert actual["count"] == old["count"] and actual["hash"] == old["hash"], "原日志集合不一致"
print("原日志完整保留")
PY
```

原文件哈希严格检查失败时先查原因，不自动删除 WAL/SHM 或覆盖数据库。应用启动可能创建或修改 SQLite 运行时文件；本次实测仅新增 `data/traffic/_index/index.sqlite-wal` 和 `index.sqlite-shm`，原文件均未变。未来若 SQLite 文件本来就存在、启动 checkpoint 改变内容，必须另查 SQLite 逻辑数据，不能将哈希变化直接标为通过。

完成前还需检查：应用实际镜像 ID 等于 `$ARTEX_UPGRADE` 的 ID，提交标签等于 `$ARTEX_REV`；数据库卷和全部挂载正确，JWT 为只读挂载，端口仍仅本地开放；健康接口 `ok=true`，已有管理员仍 `initialized=true`。使用升级前已登录的浏览器确认认证仍有效，并读取任务、统计等受保护接口。网页 HTML 标题只能证明返回内容，不能替代真实浏览器交互和视觉验收。

将本次检查结论、差异解释和未执行项写入备份目录的验收记录，再恢复正常使用。保留本次备份、提交号和镜像 tag；不要只保留会被下次升级覆盖的 `artex:local-arm64`。

## 10. 失败恢复与回退

**切换前失败、旧容器仍存在时**：先保存错误信息，再启动原容器；不要执行 `up` 意外重建。

```bash
docker compose start artex
curl --fail http://127.0.0.1:8787/api/health
```

**切换后需要回退应用时**：确认没有新增运行中的任务、没有不兼容的数据库迁移，并已保存失败现场，再执行本次备份目录里的脚本：

```bash
sh "$ARTEX_BACKUP/rollback.sh"
curl --fail http://127.0.0.1:8787/api/health
```

若旧快照镜像被手动清理，先恢复镜像：

```bash
docker image load -i "$ARTEX_BACKUP/rollback-image.tar"
sh "$ARTEX_BACKUP/rollback.sh"
```

回退脚本恢复升级前的 Compose override 和旧应用环境，不恢复数据库或 data。数据库/文件覆盖恢复可能丢掉备份之后的新数据，须另行核对、停写并明确确认后处理，不纳入自动回退。禁止使用 `docker compose down -v`、删除 `artex_pgdata`、清空 data 或全局清理代替回退。

## 11. 本次实测记录与踩坑

以下是 **2026-10-15 首次无损升级** 的历史证据，不代表未来任意提交均已验收：

| 项目 | 本次结果 |
| --- | --- |
| 部署来源 | `charon/main@5cf11b3f7c7aeb19e06b82bb03caa3fab4f65ec5` |
| 升级镜像 / 回退镜像 | `artex:upgrade-20261015-112211` / `artex:rollback-20261015-112211` |
| 备份目录 | `/Users/cc/artex-backups/20261015-112211` |
| 数据库恢复 | 51 张表行数及完整内容哈希全部一致 |
| 业务数据 | 4 个任务、129 个资产、21 个漏洞、28 个会话、1 份 LLM 配置保留 |
| 文件 | 原 1917 个文件及链接不变，仅新增 SQLite WAL/SHM |
| 数据库差异 | agents、mcp_servers 仅更新时间变化；server_logs 从 210 增至 214，原日志内容哈希一致 |
| 认证 | 原 JWT 密钥保留且只读挂载，升级前签发的测试令牌升级后仍有效 |
| 环境 | 原 Python 包清单完全一致，数据库容器及卷未更换，两个服务运行且重启计数 0 |
| HTTP | 健康、认证状态、任务、统计及首页均返回 200；任务接口结果完全一致，HTML 标题为 CTEST |
| 未执行 | LLM 调用、实际扫描、浏览器交互及视觉验收 |

历史原始校验记录位于该备份目录的 `upgrade-verification.json`、`restore-verification.json`、`file-backup-verification.json`、`database-before.json` / `database-after.json`、`files-before.json` / `files-after.json`，不提交其敏感内容。

| 问题 | 经验 |
| --- | --- |
| 普通重建丢掉手工安装的包 | 从旧容器 commit 的快照替换程序，不从原始运行镜像重建 |
| JWT 原来只在 `/app/jwt.key` | 复制原密钥并在可浏览工作区之外持久化，不能生成新密钥 |
| `docker restart` 后代码没更新 | restart 不会编译源码；需先构建，再 up 替换应用 |
| Buildx 写活动记录报 `operation not permitted` | 本次由执行沙箱限制 `~/.docker/buildx/activity` 写入，获准后重试同一构建；不修改全局权限或重装 Docker |
| gzip 归档逐文件随机读取很慢 | 本次中断了低效校验，改用 `tarfile.open(..., "r|gz")` 顺序校验，减少停机耗时 |
| 提前访问接口得到 Connection refused | 应用仍停止时属于预期现象；等待切换及健康检查成功后再验收，不能把提前检查算作新版故障或通过 |
| 备份成功就认为可恢复 | 真实恢复到独立数据库，比较每张表行数和内容哈希 |
| 镜像回退被误当成数据恢复 | 明确区分应用回退与数据库/文件恢复，禁止自动覆盖新数据 |

本文示例是在本次实际流程上参数化整理的手册，文档编写阶段只执行静态检查及只读核查，没有再次完整执行升级或回退。后续使用者必须按当次数据、版本和差异逐项验收。

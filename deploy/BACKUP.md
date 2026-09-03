# 备份与恢复操作手册（LiveTranslate Server · Go）

本手册配合 `deploy/backup.sh` 与 `deploy/restore.sh` 使用。**本轮交付未执行任何备份或恢复演练**——脚本与流程等待后续统一验证。

## 原则

1. 备份文件包含用户的课堂转写内容（俄语原文/中文译文）与账号元数据，**按敏感数据对待**：目录权限 `0700`，文件 `0600`，禁止写入 Git 目录或任何 Web 静态目录（脚本会拒绝这些路径）。
2. 备份与恢复都必须**显式指明目标**（数据库名 + 文件/目录），脚本拒绝宽泛路径与省略参数。
3. 每次备份后脚本用 `pg_restore --list` 自检；自检失败的备份视为无效。

## 备份

```bash
mkdir -p /var/backups/livetranslate && chmod 0700 /var/backups/livetranslate

export PGHOST=127.0.0.1 PGPORT=5432 PGUSER=livetranslate   # PGPASSWORD 或 ~/.pgpass
deploy/backup.sh --database livetranslate --output-dir /var/backups/livetranslate
```

- 输出：`livetranslate-<db>-<UTC时间戳>.dump`（pg_dump custom 格式，可并行恢复、可选择性恢复对象）。
- 保留策略：`--retention-days N`（默认 14 天，按 mtime 清理同名前缀备份）。

### 加密存储建议

备份明文落盘即等于数据库泄露。任选其一：

```bash
# 方案 A：GPG 对称加密（脚本输出末尾有提示命令）
gpg --symmetric --cipher-algo AES256 "$FILE.dump" && rm "$FILE.dump"

# 方案 B：整个备份目录放在加密卷（LUKS / FileVault）上。
# 方案 C：加密对象存储（如 rclone crypt 到 S3 兼容后端）。不要把明文
#         .dump 直接放入公开可读的 bucket。
```

### 定时任务示例（cron，每台服务器一份）

```cron
# /etc/cron.d/livetranslate-backup —— 每日 03:17 备份（避开整点高峰）
17 3 * * *  postgres  /opt/livetranslate/deploy/backup.sh \
            --database livetranslate \
            --output-dir /var/backups/livetranslate \
            --retention-days 14 \
            >> /var/log/livetranslate-backup.log 2>&1
# 每周日 04:23 做一次异机/异介质拷贝（示例：rclone 到加密远端）
23 4 * * 0  postgres  /usr/local/bin/rclone copy \
            /var/backups/livetranslate remote:livetranslate-backups \
            >> /var/log/livetranslate-backup.log 2>&1
```

systemd timer 亦可（`OnCalendar=*-*-* 03:17:00` + `Persistent=true`），结构同上。

## 恢复

```bash
# 0. 先查看备份里有什么（不写库）
deploy/restore.sh --list livetranslate-livetranslate-20260101T031700Z.dump

# 1. 恢复到不存在的数据库（安全路径）
deploy/restore.sh --backup FILE.dump --database livetranslate_restore

# 2. 确认无误后，替换现库（破坏性，需要 --confirm-drop 显式确认）
deploy/restore.sh --backup FILE.dump --database livetranslate --confirm-drop
```

### 恢复后检查（必做，按顺序）

1. **Migration 步骤**：备份可能早于新迁移。运行 `livetranslate-server migrate`（幂等）把 schema 补齐。
2. **Sequence 检查**（关键——导入/恢复后自增序列可能落后于行数据）：
   ```sql
   SELECT last_value, (SELECT max(change_sequence) FROM sync_changes) FROM sync_changes_change_sequence_seq;
   SELECT last_value, (SELECT max(id) FROM processed_operations) FROM processed_operations_id_seq;
   -- 若 last_value < max：重置
   SELECT setval('sync_changes_change_sequence_seq',
     coalesce((SELECT max(change_sequence) FROM sync_changes), 1));
   SELECT setval('processed_operations_id_seq',
     coalesce((SELECT max(id) FROM processed_operations), 1));
   ```
3. **游标检查**：iOS 客户端按本地保存的 `change_sequence` 游标增量拉取。若恢复出的变更日志比部分客户端的游标更旧（比如回滚到了旧备份），这些客户端将拉不到任何新数据——让受影响用户退出账号再登录（触发全量首传），或观察 `/v1/sync/status` 的 `changeLogTail` 与客户端游标对比。
4. **启动前冒烟**：先在**非生产端口**起一个实例连恢复库，`GET /ready` 应返回 ready，再用测试账号登录一次；确认无误再切换。
5. **备份轮换**：恢复演练完成后记录时间与结果，旧的损坏备份立即销毁。

## 与 SQLite 导入的顺序关系

从 Python 服务切换到 Go 服务的完整顺序（均属后续统一验证范畴）：

```bash
livetranslate-server import-sqlite --source python.sqlite --dry-run   # 1. 预检
livetranslate-server import-sqlite --source python.sqlite \
    --apply --report import-report.json                               # 2. 导入
deploy/backup.sh --database livetranslate --output-dir ...            # 3. 立即备份一次
```

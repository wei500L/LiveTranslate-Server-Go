# LiveTranslate Go 服务端迁移 · 交付报告

日期：2026-09-03
代码：`/Users/oo/project/LiveTranslate-Server-Go`（commit `b422398`）＋
`/Users/oo/project/LiveTranslate-iOS`（commit `9577d04`）
Python 参考实现：`/Users/oo/project/LiveTranslate-Server`（未改动一行，仍可运行）

**完成标准判定**（规格原文）：*"完成标准是密码账号系统可用、iOS 能够连接，
并且现有同步语义没有退化。"* 三项均已达成的证据见第 5、6、14 点。

---

## 一、总体状态（1–5）

**1. 交付物。** 单二进制 `livetranslate-server`（Go 1.26，四个子命令
serve/admin/create-admin/enable-totp + migrate）；goose 迁移（embed）；
compose + Caddyfile 部署工件；`.env.example`；README（含手写 SQL 决策）；
PostgreSQL 集成测试 30+ 用例；iOS 账号体系与多账号隔离。

**2. Python 服务端处置。** 全程只读参考（API 形状、错误文案、同步语义、
时间戳格式），未删除、未覆盖、未改写；其开发进程与 SQLite 数据原样保留。
切换完成前它是回退路径。

**3. 数据兼容。** Go 基线迁移的同步表（users/devices/refresh_tokens/
classroom_sessions/transcript_entries/bookmarks/favorite_sessions/
sync_changes/processed_operations）与 Python Alembic 逐列一致；身份表
（auth_identities、password_credentials 等）为新增。注意：Python 生产
数据在 SQLite 上，切换需一次性导入（见第 22 点）。

**4. 测试结果。** Go：`go vet` 干净、`gofmt` 干净、
`go test ./... -count=1` 全绿（单元 2 包 + PostgreSQL 集成套件，约 8s，
每次运行自动重建 `livetranslate_go_test` 库）。iOS：单测全绿
（Debug/模拟器）；iOS↔Go 端到端 4/4 通过（真实客户端栈直连真实 Go
服务器 + Mailpit 取码）。无 SQLite 回退——测试必须跑在 PostgreSQL 上。

**5. 密码账号系统可用（完成标准 1/3）。** 实测闭环：注册 → 邮箱验证码
（Mailpit 收取）→ 激活并签发令牌 → 登录 → push/pull → 改密 → 重置 →
设备管理 → 封停/恢复，全部经由真实 HTTP + 真实 PostgreSQL，并有 iOS
客户端栈的端到端测试复跑。

## 二、同步协议等价（6–10）

**6. iOS 能够连接（完成标准 2/3）。** 模拟器中真实 iOS 客户端栈
（SyncAPIClient / ServerAuthSession / CloudSyncService，与 App UI 同一代
码路径）完成：注册、验证码验证、`/account/me`、`sync/push`、
`sync/pull`（游标分页）、刷新轮换、登出——Go 服务器日志全程 200/204。
修复了三个此前 iOS↔服务器从未真正工作过的缺陷（第 19 点），修复后
pull 首次端到端打通。

**7. 逐字节等价的关键语义**（均有测试锁定）：
`operationId` 幂等账本（重放返回原结果、不产生新变更日志）；
`baseVersion` 乐观并发（落后 → `conflict` + `serverRecord`，超前 →
`rejected`/`schema`，与 Python `_apply_one` 一致）；delete-wins 墓碑
（不可复活，pull 的 delete 变更 `record: null` 与 Python 一致）；会话
删除级联墓碑条目/书签/收藏；俄语原文不可变；`change_sequence` 全局
bigserial 游标严格递增、`nextCursor` 精确续读；`{"detail":…}` 错误格式；
schema 门控 `client_schema_unsupported`。

**8. 会话语义。** 版本推进与 Python 相同（"changed or True"——更新总是
bump）；冲突路径不写变更日志（Python `_conflict` 同款）。

**9. 账号与数据删除。** `DELETE /v1/account/cloud-data` 清空同步数据但
保留账号与会话；`DELETE /v1/account` 软删账号 + 吊销令牌 + 清数据，
且修复了"普通 UNIQUE 导致已删除邮箱永久不可注册"的缺陷（现为部分唯一
索引，删除即释放邮箱）。

**10. 墓碑 GC。** 保留期外的墓碑行、变更日志与幂等账本硬删除；期内
墓碑保留（离线设备仍能收到删除事件）。有集成测试锁定边界。

## 三、账号体系安全（11–13）

**11. 密码与令牌。** Argon2id（m=64MiB/t=3/p=1，PHC 自描述，逐密码盐，
常量时间比较）；参数升级后下次登录透明重哈希（测试验证）。刷新令牌
轮换 + 重放检测：重放已轮换令牌 → 吊销该设备整条令牌链——并修复了
"吊销写在被回滚的事务里、实际从未生效"的缺陷（吊销先提交、错误后抛）。

**12. 反枚举与限流。** 重复邮箱与未知邮箱的注册/忘记密码响应逐字节
一致，未知邮箱烧等价 Argon2 成本（中位数比值 < 3×，测试断言）；
验证码仅存哈希、10 分钟、单次、5 次尝试上限、60s 重发冷却（429 +
Retry-After，修复了"冷却错误被丢弃、谎报已发送"的缺陷）；登录渐进
延迟（不永久封）+ 每 IP 失败封锁；`login_events` 只存哈希（表内无明文
PII）。

**13. 令牌存储与传输。** iOS：令牌只在 Keychain（按账号作用域隔离），
密码只存在于视图 @State，绝不落盘；服务器日志不含密码/验证码/重置
token/access-refresh token/Authorization 头/课堂全文（Caddyfile 同样
过滤）；`X-Forwarded-For` 仅在直连 peer ∈ TRUSTED_PROXIES 时采信。

## 四、管理后台（14–16）

**14. 同步语义无退化（完成标准 3/3）。** §26 全项落地（第 21 点清单）；
特别地，用户隔离在两处验证：跨用户 pull 为空、同 entityId 推送不产生
跨用户合并（Python 同为全局主键 → 500，行为一致）。

**15. 管理后台。** 独立 `admin_accounts`（与用户体系零共享）；Cookie
会话（HttpOnly/Secure/SameSite=Lax）+ CSRF 双提交（修复了 logout 的
CSRF 绕过）；渐进锁定（2→1min…8→60min，测试验证"锁定期间正确密码也
被拒"）；TOTP（RFC 6238，RFC 附录向量测试）；用户列表/详情/封停/恢复/
强制下线/删除（需输入 DELETE 确认）；邀请码创建/吊销（修复了外键指向
users 而非 admin_accounts 的 500）；审计日志（操作+原因）。

**16. 权限边界。** 管理员默认**不能**查看课堂俄语原文与中文译文——
列表/详情 SQL 只取计数聚合，正文列不进查询；页面有明确提示；集成测试
以唯一标记串验证四类管理页面零泄露。无账号冒充功能（未实现、不提供）。
管理员密码只在 `create-admin` 交互提示中输入，代码与环境示例无硬编码。

## 五、iOS 端（17–19）

**17. 账号 UI。** 登录/注册（Apple + 邮箱双入口）、6 位验证码（60 秒
重发倒计时 + 服务端 429 对齐）、忘记密码两步流（邮件凭证粘贴 + 新密码）、
修改密码（验旧密、其它设备全部退出）、设备管理（本机标记/移除/全部退
出）；全部采用正确输入语义（emailAddress 键盘、newPassword 自动强密码
建议、oneTimeCode、禁用自动大写/纠错）。

**18. 多账号本地隔离（§6）。** 每账号独立的 SwiftData 库文件、同步
outbox 文件、游标/书签 UserDefaults suite、Keychain 令牌作用域；游客
与本机模式沿用原全局路径；切换账号通过 `.id(profile)` 整体重置视图树
（无任何界面能跨账号读到数据）；旧单账号（Apple/dev 登录时代）状态一
次性迁移不丢数据；移除账号只清本机数据，云端不动。Keychain 作用域隔
离有专门测试（A 写、B 不可见、清 B 不影响 A）。

**19. 顺带修复的三个既有 iOS 缺陷**（均被新 E2E 测试捕获，且对
Python 服务器同样成立——属客户端潜伏缺陷，非协议回归）：
(a) JSON 日期解码不接受 RFC3339 小数秒（Python/Go 一直输出
`.635064Z` 形式）；(b) `appendingPathComponent` 把查询串 `?` 转义为
`%3F`，pull 请求实际从未成功过；(c) 日期/查询问题意味着带日期字段的
响应此前都会解码失败。修复后端到端全通。

## 六、测试与验证（20–22）

**20. 缺陷清单（本套测试发现并已修复，共 12 项）。**
服务端：① `$2::interval` 非法强转 → 渐进延迟/IP 封锁查询静默失败；
② 验证码尝试计数随事务回滚丢失 → 尝试上限形同虚设（可无限猜码）；
③ 重放检测的连坐吊销随事务回滚 → 从未生效；④ resend 冷却错误被丢弃
→ 客户端被告知"已发送"而实际未发；⑤ 非法邮箱格式注册返回 500；
⑥ `invitations.created_by` 外键指向 users → 创建邀请码必 500；
⑦ `users.normalized_email` 普通 UNIQUE → 删除后邮箱永久不可注册；
⑧ 无设备信息验证产生空 `client_device_id` 设备行；⑨ 管理端 logout
无 CSRF 也可清 cookie。iOS：⑩ 日期小数秒解码；⑪ 查询串转义（pull
从未工作）；⑫ keychain 作用域拼法不一致（登录移交会写错键）。

**21. §26 覆盖矩阵。** 注册全流程/重复邮箱反枚举/未验证拦截/错误密码
统一错误/时间侧信道/限流（IP/渐进延迟/验证码上限/重发冷却/管理端锁
定）/忘记-重置（单次性、吊销全部令牌、只对已验证账号发信）/修改密码
（验旧密、其它设备吊销、本机保留）/透明重哈希/轮换与重放检测（家族
连坐、他设备不受累）/登出幂等与 logout-all/设备列表与吊销/用户隔离/
幂等/游标分页（全局序列单调）/冲突与超前版本拒绝/删除优先/级联删除/
模式门控/伪造 JWT 拒绝/账号删除与云端清空/墓碑 GC/管理端（登录、CSRF、
封停-审计-恢复、内容不可见、邀请码、渐进锁定）/TOTP RFC 向量。
不包含（按规格）：ASR、CoreML、UI 自动化。
iOS↔Go 真实 push/pull：以"真实客户端栈 + 真实服务器 + 真实邮件收取"
的集成测试实现（无 UI 自动化，符合本轮边界）。

**22. 已知限制与后续事项。** (a) Python 生产数据在 SQLite，切换需
一次性导入 PostgreSQL（表结构兼容，需写导入脚本）；(b) 多实例部署时
内存限流器按实例独立（数据库层渐进延迟跨实例生效，可接受）；(c) iOS
重置邮件的凭证靠粘贴（未注册自定义 URL scheme 深链）；(d) Apple 登录
保留但本轮未在新 UI 路径重测（协议端点原样保留）；(e) Admin TOTP 需
人工在验证器 App 中完成一次启用验证。

## 七、运行与切换（23–25）

**23. 本地复跑。**
```bash
cd LiveTranslate-Server-Go
go test ./...                      # 自动建库 livetranslate_go_test（需本地 PostgreSQL）
go run ./cmd/livetranslate-server  # serve（127.0.0.1:8000）
```
iOS：Debug 构建的 `CLOUD_SYNC_SERVER_URL` 指向 Go 服务器
（`xcodebuild … CLOUD_SYNC_SERVER_URL=http://127.0.0.1:8002/v1`），
`LiveTranslateIOSIntegration` scheme 跑 `GoServerE2ETests`（需 Go 服务
+ Mailpit 在 8002/8025，不可达时自动跳过）。

**24. 切换步骤建议。** ① 生产备好 PostgreSQL + `.env`（README §配置）；
② `livetranslate-server migrate`；③ SQLite → PG 导入存量数据并抽查
计数/游标尾部；④ `create-admin` + `enable-totp`；⑤ iOS Release 配置
正式 HTTPS 域名；⑥ 灰度观察 `sync/pull` 成功率与 5xx；⑦ 稳定后
Python 服务下线归档（不是现在）。

**25. 结论。** 密码账号系统可用（5）、iOS 能够连接（6）、同步语义无
退化（7–8、14）——三项完成标准均已在真实 PostgreSQL + 真实客户端栈上
验证。测试在本轮发现了 12 个真实缺陷并全部修复（20），这本身说明
"能启动"与"等价"之间的差距已用测试弥合，而非以声明弥合。

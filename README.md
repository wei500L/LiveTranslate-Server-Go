# LiveTranslate Server (Go)

自建云同步服务端的 Go 实现：单二进制、PostgreSQL、无 Node/React。它是
Python 参考实现（`LiveTranslate-Server`，仅作 API/数据库/同步行为的兼容
性参考，切换完成后退役）的功能等价替代，并新增了密码账号体系、管理后台
与审计。

一个二进制，四个入口：

```
livetranslate-server serve          # /v1 API（iOS 客户端）
livetranslate-server admin          # 管理后台 Web UI（独立监听）
livetranslate-server create-admin   # 交互式创建管理员（提示输入密码，绝不硬编码）
livetranslate-server enable-totp <username>  # 为管理员启用 TOTP
livetranslate-server migrate        # 仅执行数据库迁移
```

## 快速开始（本地开发）

```bash
# PostgreSQL（本地 socket 或 docker 均可）
createdb livetranslate_dev

export DATABASE_URL="postgres://$USER@/livetranslate_dev?host=/tmp&port=5432"
export JWT_SECRET="$(openssl rand -hex 32)"
export DEV_MODE=true            # 本地开发：cookie 非 Secure、注册可用 Mailpit/日志投递
export MAILPIT_BASE_URL="http://127.0.0.1:8025"   # 可选：brew install mailpit

go build -o /tmp/lts ./cmd/livetranslate-server
/tmp/lts serve          # 127.0.0.1:8000
/tmp/lts admin          # 另一个终端，127.0.0.1:8081
/tmp/lts create-admin   # 创建第一个管理员
```

或直接 `docker compose up`（含 PostgreSQL、API、admin；参见 compose.yml）。

## 配置

全部通过环境变量，见 `.env.example`（每个变量都有注释）。要点：

| 变量 | 说明 |
|---|---|
| `DATABASE_URL` | pgx 原生 URL。仅支持 PostgreSQL（本服务不提供 SQLite 模式） |
| `JWT_SECRET` | HS256 签名密钥；为占位符时拒绝启动 |
| `TRUSTED_PROXIES` | 仅这些直连 CIDR 的 X-Forwarded-For 会被采信；默认空（谁都不信） |
| `DEV_MODE` | 开发便利；生产必须 false（admin cookie 会因此带 Secure） |
| `DEV_LOGIN_ENABLED` | `/v1/auth/dev` 调试登录；生产必须 false |
| `SMTP_*` | 生产邮件投递；缺 SMTP 且非 DEV_MODE 时注册返回 503 而非静默丢码 |

## 同步协议（v1，与 Python 版逐字节兼容）

iOS 客户端无需任何改动：

- `POST /v1/sync/push`：批量操作，`operationId` 幂等（账本回放）；`baseVersion`
  乐观并发（落后 → `conflict` + `serverRecord`，超前 → `rejected`/`schema`）；
  delete-wins 墓碑；会话删除级联墓碑子实体；俄语原文不可变。
- `GET /v1/sync/pull?cursor=`：`change_sequence`（全局 bigserial）游标增量。
- `GET /v1/sync/status`：计数与游标尾部。
- 错误统一 `{"detail": …}`；schema 门控返回 `errorCode: client_schema_unsupported`。

## 密码账号体系

- Argon2id（PHC 自描述，参数升级后下次登录透明重哈希）。
- 注册 → 邮箱验证码（仅存哈希、10 分钟、单次、尝试上限、重发冷却）→ 激活。
- 反枚举：重复邮箱与未知邮箱的注册/忘记密码响应逐字节一致，未知邮箱烧同样
  的 Argon2 成本。
- 刷新令牌轮换 + 重放检测（重放旧令牌会吊销该设备整条令牌链）。
- 忘记密码令牌单次、短时效，重置后吊销全部刷新令牌；修改密码需验旧密码并
  吊销其它设备。

## 管理后台

- 独立账号表（`admin_accounts`），与用户体系完全隔离；**没有账号冒充功能**。
- Cookie 会话（HttpOnly / Secure / SameSite=Lax）+ CSRF 双提交 + 渐进锁定
  （连续失败临时锁，不用永久封）+ 可选 TOTP（RFC 6238）。
- 功能：用户列表/详情、封停/恢复/强制下线/删除、邀请码、审计日志。
- **管理员默认不能查看课堂俄语原文与中文译文**：所有列表/详情查询只取
  计数聚合，正文列不进 SQL，页面有明确提示。
- 审计：管理员与账号安全事件落 `audit_events`（before/after 仅状态摘要）。

## 安全要点（实现承诺）

生产日志不记录：密码、邮箱验证码、重置 token、access/refresh token、
Authorization Header、完整课堂文本。`X-Forwarded-For` 仅在直连peer 属于
`TRUSTED_PROXIES` 时采信。管理员密码只在 `create-admin` 交互提示中输入，
代码与环境示例中无任何硬编码。

## 测试

测试**必须**跑在 PostgreSQL 上（没有 SQLite 回退——同步语义、事务与
约束就是被测对象）：

```bash
# 默认连接本地 socket 的 livetranslate_go_test 库（自动建库、每次运行前重建）
go test ./...

# 指定库：
LIVETRANSLATE_TEST_DATABASE_URL="postgres://user:pass@host:5432/dbname" go test ./...
```

覆盖面：注册/验证/登录全流程、重复邮箱反枚举、未验证拦截、错误密码统一
错误、登录时间侧信道、限流（IP/渐进延迟/验证码上限）、密码流（忘记/重置/
修改/透明重哈希）、令牌轮换与重放检测、设备管理、用户隔离、幂等、游标
分页、冲突/删除优先/级联、模式门控、账号删除与云端清空、墓碑 GC、管理端
（登录/CSRF/封停/审计/邀请码/锁定/**课堂内容不可见**）、TOTP RFC 向量。

## 设计选择：为什么是手写 SQL

`internal/store` 使用 pgx/v5 + 手写 SQL（`Q` 接口 + 事务内显式查询），
而非 ORM 或 sqlc 生成代码。原因：同步核心的要点恰恰是**事务内多表写入的
精确语义**（账本 + 版本 + 变更日志要么全提交要么全回滚），并且本服务有
两处"必须随事务提交、即使逻辑失败"的写入（验证码尝试计数、重放检测吊销）
——这类语义在手写 SQL 里一目了然，在生成层里反而会被抽象掉。表结构小
（18 张表），SQL 总量可控，可读性优先。

## 部署

见 `compose.yml` 与 `deploy/Caddyfile.example`：API 走公网域名 + 自动
HTTPS；管理后台走独立内网域名 + IP 白名单；PostgreSQL 端口不对外。迁移
在 `serve` 启动时自动执行（幂等），也可先 `livetranslate-server migrate`。

## 目录

```
cmd/livetranslate-server/  入口与子命令
db/migrations/              goose 迁移（embed 进二进制）
internal/config/            环境变量配置
internal/httpapi/           中间件、错误格式、路由挂载
internal/httpapi/auth/      /v1/auth/*、/v1/me/*
internal/httpapi/syncapi/   /v1/sync/*
internal/httpapi/accountapi /v1/account/*
internal/auth/              注册/验证/登录/令牌/密码流服务层
internal/sync/              同步协议服务层 + 墓碑 GC
internal/password/          Argon2id + 密码策略
internal/admin/             管理后台（服务/模板/TOTP）
internal/audit/             审计记录器
internal/store/             手写 SQL 数据层
internal/mail/              SMTP / Mailpit / 日志投递
internal/token/             JWT 与不透明令牌
tests/integration/          PostgreSQL 全栈集成测试
```

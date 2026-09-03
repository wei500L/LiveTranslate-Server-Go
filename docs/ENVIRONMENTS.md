# 环境配置矩阵（development / staging / production）

> 本文档只描述**配置形态**；真实的生产域名、Team ID、SMTP 凭据一律由运维在部署时填入，
> 仓库中不出现任何虚构的生产值。生产配置在启动时经过静态校验（`internal/config/validate.go`），
> 违规组合直接拒绝启动。

## 环境选择

`APP_ENV=development|staging|production`（默认 development）。只有 `production` 触发
严格校验；staging 是"生产形态 + 可放宽的域名/凭据来源"。

## 变量矩阵

| 变量 | development | staging | production | 说明 |
|---|---|---|---|---|
| `APP_ENV` | development | staging | **production** | 触发生产静态校验 |
| `DATABASE_URL` | 本地 socket/容器 | 独立库 | 独立库 | pgx URL；生产禁占位值 |
| `JWT_SECRET` | 随机值即可 | 随机值 | **必须 ≥32 字符随机值**，禁占位符 | `openssl rand -hex 32` |
| `JWT_SECRET_PREVIOUS` | — | 可选 | 可选（轮换期） | 仅用于验证旧 token，不用于签名 |
| `JWT_ISSUER` | livetranslate-server | 同左 | 同左 | 与 iOS 客户端约定一致 |
| `PUBLIC_BASE_URL` | 可空 | staging 域名 | **必须 https:// 正式域名** | 邮件里重置链接的来源 |
| `PASSWORD_RESET_PATH` | /reset-password | 同左 | 同左 | 深链路径，AASA 组件同步声明 |
| `SMTP_HOST/PORT` | 可空（Mailpit/日志） | 真实 SMTP | **必填** | 无 SMTP 时生产拒绝注册而非假装发送 |
| `SMTP_TLS_MODE` | 任意 | starttls/smtps | **starttls 或 smtps**（禁 none） | 587=STARTTLS；465=smtps |
| `SMTP_USERNAME/PASSWORD` | — | 真实凭据 | 真实凭据 | 只放 `.env`/secret，不入库 |
| `SMTP_FROM` | 任意 | 域名内地址 | **域名内地址** | 发件地址 |
| `SMTP_FROM_NAME` | LiveTranslate | 同左 | 同左 | 发件显示名 |
| `SMTP_CONNECT_TIMEOUT/SEND_TIMEOUT` | 15s/20s | 同左 | 同左 | 拨号/发送阶段上限 |
| `MAILPIT_BASE_URL` | 可选（本地捕获） | **空** | **必须为空** | 生产校验直接拒绝 |
| `REGISTRATION_MODE` | open/invite_only/disabled | 同左 | 同左 | **单一事实来源**；管理后台只读显示 |
| `REQUIRE_INVITATION` | 兼容旧变量 | — | — | 已被 REGISTRATION_MODE 取代（显式设置时后者优先） |
| `DEV_MODE` | true 可用 | **false** | **false** | 生产校验拒绝 true |
| `DEV_LOGIN_ENABLED` | 本地可 true | **false** | **false** | 生产校验拒绝 true |
| `TRUSTED_PROXIES` | 空（直连） | 反代内网 CIDR | 反代内网 CIDR | 仅信任的代理可写 X-Forwarded-For |
| `CORS_ORIGINS` | 空或本地 | 管理域（若跨域） | 按需 | iOS 原生流量不需要 CORS |
| `ADMIN_LISTEN_ADDR` | 127.0.0.1:8081 | 内网地址 | **127.0.0.1**（经代理+白名单暴露） | 生产校验要求 loopback |
| `AASA_TEAM_ID` | 空 | 真实 10 位 Team ID | **真实 10 位 Team ID** | 未配置时 AASA 输出空列表并在日志警告，**不编造** |
| `APPLE_BUNDLE_ID` | com.livetranslate.ios | 同左 | 同左 | 与 iOS 工程 PRODUCT_BUNDLE_IDENTIFIER 一致 |
| `TOMBSTONE_RETENTION_DAYS` | 180 | 180 | 180 | 墓碑/变更日志保留期 |
| `LOGIN_EVENTS_RETENTION_DAYS` | 90 | 90 | 90 | 登录事件保留（0=永久） |
| `AUDIT_RETENTION_DAYS` | 365 | 365 | 365 | 审计日志保留（0=永久） |
| `METRICS_ENABLED` | 可 true | 可 true | 可 true（经代理限制访问） | `/metrics` 聚合计数，无 PII |
| `PPROF_ENABLED` | 可 true | 谨慎 | **默认 false** | 开启后仅 loopback 可访问 |
| `LOG_LEVEL` | debug | info | info/warn | 结构化文本日志 |
| `LISTEN_ADDR` | 127.0.0.1:8000 | 127.0.0.1:8000 | 127.0.0.1:8000（经 Caddy 暴露） | — |

## 生产启动校验（APP_ENV=production 时自动执行）

以下任一情况都会让进程**启动失败**并列出全部问题：

1. `JWT_SECRET` 为空 / 占位符（REPLACE_WITH…、CHANGEME、example.com 等） / 短于 32 字符。
2. `DATABASE_URL` 缺失或占位。
3. `DEV_MODE=true` 或 `DEV_LOGIN_ENABLED=true`。
4. `MAILPIT_BASE_URL` 非空（开发捕获通道不得存在于生产）。
5. `PUBLIC_BASE_URL` 缺失或非 HTTPS origin（密码重置链接将无处可指）。
6. `SMTP_HOST` / `SMTP_FROM` 缺失，或 `SMTP_TLS_MODE=none`（生产必须真实发信通道）。
7. `LISTEN_ADDR` 全网卡绑定且既无 TRUSTED_PROXIES 也无 CORS（疑似直接暴露公网）。
8. `ADMIN_LISTEN_ADDR` 非 loopback（管理面必须经带访问控制的代理暴露）。

## Universal Link / AASA 配置入口（部署时填写）

1. Apple Developer 后台拿到 10 位 **Team ID** → `AASA_TEAM_ID`。
2. 确认 `APPLE_BUNDLE_ID`（默认 `com.livetranslate.ios`）与 iOS 工程一致。
3. 服务器自动提供 `GET /.well-known/apple-app-site-association`；部署后在
   `https://<域名>/.well-known/apple-app-site-association` 验证 JSON 含
   `appIDs: ["<TeamID>.com.livetranslate.ios"]` 且组件声明了重置路径。
4. iOS 侧在 Xcode 的 Signing & Capabilities 添加 Associated Domains：
   `applinks:<API域名>`（iOS 仓库 project.yml 已预留说明位）。
5. 以上 3–4 步属于**后续统一验证**范围，本轮未实际执行跳转验证。

## 密钥轮换（JWT_SECRET）

1. 生成新密钥；将**旧**密钥移入 `JWT_SECRET_PREVIOUS`，新密钥写入 `JWT_SECRET`。
2. 重启服务：新签发用新密钥，存量 access token（15 分钟 TTL）继续可用。
3. 等待超过 `ACCESS_TOKEN_TTL_SECONDS` 后清空 `JWT_SECRET_PREVIOUS`。

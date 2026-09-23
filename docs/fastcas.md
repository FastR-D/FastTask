# 可选 FastCAS 接入

FastTask 保留管理员开户及本地登录，不新增公开注册。认证将已有 FastTask 用户关联到 FastCAS，用户 ID、任务数据、角色与设备配置不迁移。FastCAS 管理员也不会自动成为 FastTask 管理员。

## 配置与开发

当前 Go SDK 用 `replace ../FastCAS/sdk/go` 引入同工作区代码，模块 Go 版本为 1.26，toolchain 固定为 1.26.8。原有后端测试已在该 toolchain 通过。

以下四项全部配置才启用；全部留空时本地登录不会访问 FastCAS：

```dotenv
FASTTASK_FASTCAS_ISSUER=https://cas.example.org
FASTTASK_FASTCAS_CLIENT_ID=fasttask
FASTTASK_FASTCAS_CLIENT_SECRET=<应用独立随机密钥>
FASTTASK_FASTCAS_REDIRECT_URI=https://task.example.org/api/v1/auth/fastcas/callback
```

`FASTTASK_PUBLIC_URL` 必须是浏览器实际访问的项目 origin。FastCAS 中登记精确 callback，允许 `openid profile email`，事件地址为 `https://task.example.org/api/v1/auth/fastcas/events`。本机调试可显式启用 `FASTTASK_FASTCAS_LOOPBACK_HTTP=true`，仅接受 loopback HTTP。

SDK 发布前，Docker 使用 BuildKit 的额外本地构建上下文：

```sh
docker build --build-context fastcas-sdk=../FastCAS/sdk/go -t fasttask .
```

2026-09-23 同步远端 PWA/Agent 版本后，镜像采用 Node 22、Go 1.26.8 和 Alpine 3.22，并保留新版入口脚本、时区数据、附件目录和健康检查；FastCAS SDK 由显式 BuildKit 上下文提供。隔离镜像重新构建并启动，实际验证 ready、SPA、OpenAPI 中 10 个 FastCAS 路由、未配置时 `available=false` 和原本地管理员密码登录；Web 阶段需复制同仓库 sidecar 类型源文件，入口脚本需明确执行 FastTask 二进制。测试容器已删除。此演练不连接生产 FastCAS 域名或真实项目数据。

## 用户与回调流程

本地登录后打开“账号认证”，确认本地密码，再前往 FastCAS 登录并授权。新认证先在本地保存 pending，中心激活成功后更新 active；待完成项可从页面确认，之后正常 FastCAS 登录也会恢复中断的激活。

浏览器重定向后，本地 access token 需要由原 refresh token 恢复。服务校验原认证的本地会话族，不将合法刷新视为账号切换；另一账号或新的登录会话族无法完成旧绑定。授权码只经过 HttpOnly 回调 cookie，再由同 origin POST 消费，不将 FastCAS access/refresh token 交给浏览器。

FastCAS 登录后仍签发 FastTask 的 HS256 用户会话，记录上游 SID、绑定 ID/版本和认证来源；刷新保留这些字段。设备令牌、服务身份校验继续独立，用户令牌不能变成服务令牌。解绑与签名撤销事件只终止对应绑定的 FastCAS 来源会话，本地登录继续有效。

## 数据迁移和接口

迁移 9 增量扩展 sessions，并新增 fastcas_transactions、fastcas_links、fastcas_events。本地与 subject 的有效绑定各有唯一索引；事务 state 在 SQLite 事务内一次性消费，事件 ID 去重、绑定版本检查、会话撤销一起提交。远端 6–8 号迁移属于 Agent 线程、harness 和附件；旧本地 FastCAS 构建曾占用 6 号，升级器识别该记录后补跑远端 6–8，再将已有 FastCAS 表登记为 9，不重建本地账号、会话或绑定。真实 SQLite 升级测试验证该路径与重复运行。

接口前缀 `/api/v1/auth/fastcas`：`GET available/status/login/callback`；`POST link/complete/links/:id/reconcile/links/:id/revoke/events`。状态、绑定与管理接口检查本地用户；修改请求要求项目 origin；events 要求签名 `application/jwt`。这些路由已通过 Gin 挂载，并纳入 `/api/v1/openapi.json`。

## 验证与剩余工作

从 FastCAS 目录运行 `FASTCAS_PROJECT_CONTRACT=1 go test ./internal/httpapi -run TestFastTaskAgainstProvider -count=1 -v`，会启动真实 FastCAS HTTP 服务及隔离 PostgreSQL schema，再用 FastTask SQLite 数据库和 Gin 路由验证绑定、刷新后的本地证明、FastCAS 登录及选择性撤销。FastTask auth 单测覆盖并发单次消费、刷新来源保留、服务令牌隔离、事件去重和版本防回退。

前端已增加登录按钮和账号认证页。同步远端 PWA/Agent 后，`npm ci --ignore-scripts --no-audit --no-fund`、`npm run build` 和 `npm test`（25 个文件、169 项）通过，包括 FastCAS 回调和新版离线会话测试。真实 Chrome → FastTask/Gin/SQLite → FastCAS/PostgreSQL 页面契约覆盖本地管理员密码登录、原生授权回调、独立浏览器 FastCAS 登录保持用户/角色、解绑后仅 CAS 会话失效；离线用户资料缓存只含项目用户资料字段，access token 仍仅在内存。全局退出/身份禁用接收端已使用 Go SDK 的 `ginadapter`，并通过真实跨进程投递契约。`/api/v1/openapi.json` 描述全部 10 个 FastCAS Gin 路由的参数、Cookie 回调流程、鉴权边界与通知媒体类型；路由覆盖测试和更新后的 golden 基线通过。容器构建与未配置 FastCAS 的本地登录已实测；正式域名、真实任务数据和服务集成令牌路径仍待发布验收。不能把当前接入视作完整规划交付。

FastCAS 应用登记还需设置 `backchannel_logout_uri` 为 `https://task.example.org/api/v1/auth/fastcas/backchannel-logout`。接收端验证标准签名 `logout_token`，按身份及可选 sid 原子去重，只撤销 FastCAS 来源会话；项目本地登录、账号、内容和权限保持独立。

`events_uri` 同时接收签名 `identity.status_changed` 通知。停用只撤销对应 FastCAS 来源会话及其刷新能力，重新启用不会恢复旧会话；本地管理员登录与业务权限不随中心状态改变。真实提供方到 FastTask 接收端的状态事件契约已通过。

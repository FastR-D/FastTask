# FastTask

FastTask 是面向科研工作的长期目标与每日推进工作台。把目标拆成有具体产出的小步骤，通常每天安排三项，超出的顺延；通过对话调整任务，并用实际成果和专注记录保留推进过程。

服务由 Go、React 和 SQLite 组成，提供网页/PWA、HTTP API，以及墨水屏和 FastRD Panel 的今日任务视图。

## 能做什么

- **管理目标与任务树**：创建目标、里程碑和行动，编辑、移动、暂停、完成、重新打开；删除进入回收站，恢复时保留原有历史。
- **安排今天与未来七天**：围绕主要科研目标安排真实可执行的叶子任务。通常每日三项，八项可形成 3/3/2；不足时说明依赖、预算或状态原因，不复制工作凑数。
- **继续聊天补任务**：助手读取当前任务树后提出增删改或排期建议，用户确认后应用。有空位和剩余预算时可补到今天，已开始或有证据的承诺受到保护。
- **遵守真实依赖**：需要前置产物的任务，必须等待前置 Task 最终完成。一般工作顺序可以用排序表达，不必人为增加硬依赖。
- **分别记录小步骤与整项成果**：今日推进支持结果、步骤、计时和最小行动。今日完成可撤销，原成果事件与实际专注记录保留；整项任务完成需明确确认，也可与今日结果联合提交。
- **自由补充学习**：添加阅读或学习内容，不计入核心三项，也不要求先完成全部核心项。
- **直接进入对应工具**：今日行可打开已配置网页地址的 FastWrite、FastRead、FastNews、FastPPT、FastLab。打开网页不会被视为任务完成。
- **pannel 蓝牙墨水屏**：在 Windows 笔记本上连接 WOLINK 墨水屏，显示头像、天气与当日任务，每天北京时间 14:00、17:30、21:00 自动读取 FastTask 状态并刷新。
- **对话与专注**：流式聊天、可审批的结构化提案、专注计时、语音转写、后台作业与周复盘。未配置真实模型时界面会标识演示模式，手工任务管理和排期仍可使用。

每日预算为 30–480 分钟，默认 120 分钟；30 分钟也可安排三个 10 分钟的真实步骤。自动重排保留有证据的工作。父项推进后再拆分时，孩子默认顺延；用户明确选择不同产出的独立小步骤，可在真实资格满足时安排到今日剩余空位。

排期作业结果未知时，同一页面提供“核对上次安排”：沿用原请求内容和幂等键，或查询已知作业，不另外创建作业。成功后读取当前计划；明确失败或取消后先刷新基线。此恢复状态不承诺跨整页刷新保留。

## 构建与启动

### 依赖

- Go：`go.mod` 声明 Go 1.26.0，工具链为 **Go 1.26.8**。
- Node.js 与 npm：Docker 前端构建使用 **Node 22.22.2**；本地可使用同版本，并通过 `npm ci` 安装锁定依赖。
- C 编译器与 CGO：SQLite 驱动需要 `CGO_ENABLED=1`。Linux 构建环境可安装 `build-essential`、`libsqlite3-dev`、`tzdata`、`ca-certificates`；其他系统使用对应工具包。
- FastCAS Go SDK：本地 `go.mod` 使用 `replace github.com/FastR-D/FastCAS/sdk/go => ../FastCAS/sdk/go`，必须准备相邻目录中的 SDK。

建议目录布局：

```text
workspace/
├── FastTask/
└── FastCAS/
    └── sdk/go/go.mod
```

可从 `FastR-D/FastCAS` 获取 SDK；Dockerfile 当前固定到提交 `676d49b18f700c009f1f491280a93de7da294836`。本地使用同一版本可与容器构建保持一致。SDK 是构建依赖；没有启用 FastCAS 登录时，无需运行 FastCAS 服务。

尚未准备 SDK 时，在 FastTask 根目录执行：

```bash
git clone https://github.com/FastR-D/FastCAS.git ../FastCAS
git -C ../FastCAS checkout 676d49b18f700c009f1f491280a93de7da294836
```

### 本地运行

以下命令使用 POSIX shell；Windows 可在 WSL 中执行。先进入 FastTask 根目录：

```bash
npm --prefix web ci
npm --prefix web run build
mkdir -p bin
CGO_ENABLED=1 go build -trimpath -o bin/fasttask ./cmd/fasttask
./bin/fasttask serve
```

默认访问 `http://127.0.0.1:10000/`。首次启动会自动迁移数据库并初始化管理员；本地开发默认账号为 `admin`，密码为 `fasttask-admin`，仅用于本机开发。

管理员环境变量只用于首次初始化；修改 `.env` 不会重设数据库中已有账号的密码。

`serve` 默认同时启动 Worker 和 Scheduler，后台作业及跨日维护可正常运行。生产运行需要同时保留二进制、`web/dist` 和可写的数据目录；网页资源由服务从磁盘提供。

其他运行命令：

```bash
./bin/fasttask version
./bin/fasttask migrate
./bin/fasttask doctor
./bin/fasttask worker
./bin/fasttask scheduler
```

独立运行 Worker 或 Scheduler 时，可用 `serve --with-worker=false --with-scheduler=false` 关闭服务内相应角色。`migrate`、`doctor` 与服务启动会执行数据库迁移，升级前先备份。

## 配置

配置从进程环境读取，也会加载当前工作目录下的 `.env`；已经设置的进程环境变量优先。不要把 `.env`、数据库、备份或访问凭据提交到仓库。

| 变量 | 默认值或用途 |
| --- | --- |
| `FASTTASK_LISTEN` / `FASTTASK_PORT` | `127.0.0.1` / `10000` |
| `FASTTASK_PUBLIC_URL` | 默认本机服务地址；部署时设为用户实际访问的 URL |
| `FASTTASK_DATABASE` | `data/fasttask.db` |
| `FASTTASK_WEB_DIST` | `web/dist` |
| `FASTTASK_AUDIO_DIR` / `FASTTASK_ATTACHMENT_DIR` | `data/audio` / `data/attachments` |
| `FASTTASK_ENV` | `development`；生产使用 `production` |
| `FASTTASK_ADMIN_USER` / `FASTTASK_ADMIN_PASSWORD` | 首次初始化管理员；生产必须替换默认密码 |
| `FASTTASK_JWT_SECRET` | 用户令牌签名密钥；至少 24 字符，生产使用随机值 |
| `FASTTASK_PANEL_JWT_SECRET` | 服务令牌签名密钥；默认沿用 JWT 密钥，可单独配置至少 24 字符的随机值 |
| `FASTTASK_PROVIDER_ENCRYPTION_KEY` | 生产必须单独设置至少 32 字符的随机值，且不同于 JWT 密钥；用于加密模型和集成凭据 |
| `FASTTASK_TRUSTED_PROXIES` | 可信代理 IP/CIDR，逗号分隔；按实际代理配置，代码默认 `10.22.33.0/24` |
| `OPENAI_API_BASE_URL` / `OPENAI_MODEL` / `OPENAI_API_KEY` | OpenAI-compatible 模型地址、名称与密钥；地址通常以 `/v1` 结尾 |
| `OPENAI_TRANSCRIPTION_MODEL` | 真实音频转写模型；未配置时明确使用演示转写 |
| `FASTTASK_LOG_FORMAT` / `FASTTASK_LOG_LEVEL` | `text` / `info`；可设 `json` 及 `debug/info/warn/error` |
| `FASTTASK_FASTRESEARCH_LOGIN_ENABLED` | 启用 FastResearch 个人 Key 登录；默认关闭 |
| `FASTTASK_FASTRESEARCH_URL` | FastTask 服务端访问 FastResearch 的地址（容器内通常为 `http://host.docker.internal:8787`） |
| `FASTTASK_FASTRESEARCH_PUBLIC_URL` | 浏览器访问 FastResearch 的公开地址；生产环境必须为 HTTPS |
| `FASTTASK_FASTRESEARCH_SSO_SECRET` | 与 FastResearch `FASTRESEARCH_FASTTASK_SSO_SECRET` 相同的共享密钥，至少 32 字符 |
| `FASTTASK_FASTRESEARCH_INGEST_KEY` | FastResearch 服务端读取当日任务的共享密钥；需与 FastResearch 的 `FASTTASK_API_KEY` 一致 |
| `FASTTASK_FASTRESEARCH_LOOPBACK_HTTP` | 生产环境允许回环地址使用明文 HTTP；仅本机开发 |
| `FASTTASK_NOTIFICATION_ENABLED` | 默认 `true`；通道和接收端由后台配置 |

管理员也可在“后台 → 模型管理”配置默认 OpenAI-compatible Provider。服务端保存加密凭据，浏览器和 sidecar 通过 FastTask 代理调用模型，不接收模型 API Key。

生产 `.env` 至少包含以下内容，并替换所有示例值。各密钥分别生成，可用 `openssl rand -hex 32` 生成随机值：

```dotenv
FASTTASK_ENV=production
FASTTASK_PUBLIC_URL=https://task.example.com
FASTTASK_JWT_SECRET=REPLACE_WITH_A_UNIQUE_RANDOM_SECRET
FASTTASK_PANEL_JWT_SECRET=REPLACE_WITH_ANOTHER_RANDOM_SECRET
FASTTASK_PROVIDER_ENCRYPTION_KEY=REPLACE_WITH_A_SEPARATE_RANDOM_SECRET
FASTTASK_ADMIN_PASSWORD=REPLACE_WITH_A_STRONG_INITIAL_PASSWORD
```

限制配置和数据目录的访问权限，以专用普通用户运行服务。公网部署使用 HTTPS，并仅信任实际反向代理；SSE 对话连接应关闭代理缓冲并设置足够的读取超时。浏览器录音和 PWA 安装需要安全上下文，通常为 HTTPS 或 localhost。

启用 FastCAS 登录时，必须同时配置 `FASTTASK_FASTCAS_ISSUER`、`FASTTASK_FASTCAS_CLIENT_ID`、`FASTTASK_FASTCAS_CLIENT_SECRET`、`FASTTASK_FASTCAS_REDIRECT_URI`；全部留空则保留本地账号登录。

启用 FastResearch Key 登录时，必须同时配置 `FASTTASK_FASTRESEARCH_URL`、`FASTTASK_FASTRESEARCH_PUBLIC_URL` 和至少 32 字符的 `FASTTASK_FASTRESEARCH_SSO_SECRET`，并把 `FASTTASK_FASTRESEARCH_LOGIN_ENABLED` 设为 `true`。FastResearch 侧需配置相同的 `FASTRESEARCH_FASTTASK_SSO_SECRET`。普通成员只通过 FastResearch Key 登录；本地账号和密码仅保留给管理员。FastResearch 工作台读取今日任务还需要 `FASTTASK_FASTRESEARCH_INGEST_KEY`，其值与 FastResearch 的 `FASTTASK_API_KEY` 一致。本机以 `FASTTASK_ENV=production` 运行时，如果 FastResearch 公开地址是 `http://127.0.0.1`、`http://localhost` 或 `http://[::1]`，需要同时设置 `FASTTASK_FASTRESEARCH_LOOPBACK_HTTP=true`。

两端当前的部署拓扑、SSO 时序、今日任务接口、密钥对应关系、安全边界和排障方法见 [docs/fastresearch-integration.md](docs/fastresearch-integration.md)。

## Docker 部署

Dockerfile 构建网页与 Go 服务，并下载上述固定版本的 FastCAS SDK。构建需要访问 npm、Go 模块与 SDK 下载源；可通过 `GOPROXY`、`APK_MIRROR`、`FASTCAS_REF` 构建参数调整。使用 BuildKit 的 `fastcas-sdk` 命名构建上下文也可提供本地 SDK，其根目录须包含 `FastCAS/sdk/go`。

准备好生产 `.env`，可另加 `DOCKER_DATA_PATH=./data` 指定宿主数据目录，然后运行：

```bash
docker compose build
# 首次绑定数据目录时修正容器用户权限，完成后退出
docker compose run --rm --user 0 fasttask version
docker compose up -d
docker compose logs -f fasttask
```

镜像默认以普通用户运行，数据保存到 `/var/lib/fasttask`。数据库、音频和附件路径应使用容器内路径，宿主目录由 `DOCKER_DATA_PATH` 挂载。现有 Compose 固定映射容器端口 `10000`，修改端口时须同步调整映射。

Compose 使用 `OPENAI_API_BASE_URL_CONTAINER` 覆盖容器中的模型地址，默认指向宿主机 `http://host.docker.internal:3000/v1`；使用远程模型服务时请明确设置该变量。容器中的 `localhost` 指容器自身。可信代理范围也应按容器实际收到的来源地址配置。

### 可选 Node sidecar

默认对话宿主运行在浏览器 WebAssembly 中，需要浏览器支持 JSPI。缺少该能力的浏览器可使用可选 Node sidecar；手工任务功能不依赖 sidecar。

```bash
npm --prefix web run build:sidecar
```

产物为 `sidecar/dist/host.mjs`。在原生部署中设置 `FASTTASK_SIDECAR_ENABLED=true`，必要时设置 `FASTTASK_SIDECAR_NODE_PATH`、`FASTTASK_SIDECAR_ENTRY`、`FASTTASK_SIDECAR_SOCKET`。默认由 Go 服务启动并管理 sidecar。设置 `FASTTASK_SIDECAR_PREFERRED=true` 后，服务端会把浏览器请求的 `wasm` 运行改由健康的 sidecar 执行，避免旧版或缓存页面没有启动浏览器宿主而超时；sidecar 不可用时仍保留浏览器 WASM 路径。运行环境须保留 `web/node_modules` 中的 `libfx` 及适合当前系统和架构的依赖，不能只复制 `host.mjs`。

若 sidecar 由外部进程管理器启动，Go 端设置 `FASTTASK_SIDECAR_SPAWN=false`；两端使用同一 `FASTTASK_SIDECAR_SECRET`。Node 端的 `FASTTASK_SIDECAR_ENDPOINT=unix://<socket路径>` 对应 Go 端的 socket，`FASTTASK_SERVER_ORIGIN` 指向可达的 FastTask 服务，`FASTTASK_WEB_ROOT` 指向含依赖的 web 目录。现有 Alpine Docker 镜像没有 Node，不包含 sidecar；需要该回退宿主时使用原生 Node 部署。sidecar 只负责对话运行，不直接访问 SQLite。

## 数据备份与恢复

SQLite 使用 WAL。运行中的数据库应使用一致性备份命令，不要只复制主 `.db` 文件：

```bash
./bin/fasttask backup --output backups/manual.db
./bin/fasttask backup --verify backups/manual.db
```

容器中可执行：

```bash
docker compose exec fasttask fasttask backup --output /var/lib/fasttask/backups/manual.db
```

同时备份附件、必要的音频文件及配置密钥，备份文件应离机保存并限制访问。尤其保留 `FASTTASK_PROVIDER_ENCRYPTION_KEY`，否则数据库中的加密凭据无法解密。

恢复前停止所有会写数据库的服务、Worker 和 Scheduler，验证备份，保留原数据库及其 WAL/SHM 文件后再替换为备份；恢复附件目录与原配置密钥，检查文件权限后启动服务。应用没有自动恢复流程。

## API 与设备接入

服务运行后可直接查看完整接口：

- `/api/v1/docs`：API 文档。
- `/api/v1/openapi.json`：OpenAPI 描述。
- `/health/ready`：运行就绪状态。
- `/health/version`：构建与运行版本。

| 操作 | 接口 |
| --- | --- |
| 登录 / 刷新会话 | `POST /api/v1/auth/login`、`POST /api/v1/auth/refresh` |
| 目标与任务 | `/api/v1/goals`、`/api/v1/tasks` |
| 今日完整列表 | `GET /api/v1/daily-plans/current` |
| 七日排期 | `GET /api/v1/schedule` |
| 排期设置 | `GET/PUT /api/v1/planning-settings` |
| 生成或重排作业 | `POST /api/v1/daily-plans/generation-jobs` |
| 查询作业 | `GET /api/v1/agent-jobs/{job_id}` |
| 记录今日推进 | `POST /api/v1/daily-plans/{plan_id}/items/{item_id}/completions` |
| 撤销今日推进 | `POST /api/v1/daily-plans/{plan_id}/items/{item_id}/reopenings` |
| 墨水屏读取 | `GET /api/v1/devices/self/poll` |
| Panel 摘要 / 完整今日列表 | `GET /api/v1/panel/summary`、`GET /api/v1/panel/tasks` |

用户接口使用 `Authorization: Bearer <access_token>`。今日响应含 `plan`、原有 `items`、共享投影 `today_items` 和不足原因 `availability`。从 `today_items` 读取条目 ID、`item_etag`、承诺、状态、能力及工具入口；不要把计划 ETag 当作条目 ETag。

记录小步骤的请求示例：

```http
POST /api/v1/daily-plans/{plan_id}/items/{item_id}/completions
Authorization: Bearer <access_token>
If-Match: <item_etag>
Idempotency-Key: <本次命令的唯一键>
Content-Type: application/json

{"type":"step","summary":"完成一个具体小步骤，并保存可核对的产出"}
```

撤销调用上表的 `/reopenings` 接口，提交 `{"summary":"纠正误标记"}`，并使用最新条目 ETag。一次新命令使用新幂等键；结果未知的重试沿用原键和原请求内容。计时完成还需引用有效 `work_session_ids`。若明确联合确认整项任务，提交 `complete_task:true` 和当前 `task_if_match`；撤销今日推进仍保留独立的整项任务成果。

Windows 笔记本的蓝牙墨水屏入口为“pannel”，自动读取当前登录用户的当日任务和状态，不在此页标记完成。既有 HTTP 设备接口使用独立 `X-Device-Token`，可通过设备 API 管理；Panel HTTP 接口使用用户认证，或代表相应用户且带 `panel:summary:read` / `panel:tasks:read` 范围的服务令牌。设备和 Panel 的只读权限不授予完成或撤销权限；需要写入时使用用户接口和用户授权。

网页、蓝牙墨水屏和 Panel 读取同一份今日安排及共享任务投影；设备与 Panel HTTP 列表支持 `ETag`、`If-None-Match` 和 `304`。外部 Panel 客户端以及五个工具系统的实际业务联调由各部署方完成；FastTask 不把外部系统动作或墨水屏写入自动认定为已完成科研产出。

### Windows 蓝牙墨水屏

实现参考本地 `myled` 项目的 WOLINK ESL-75MR 协议：AES-128-CBC challenge 认证、黑白红双位平面、8192 字节独立 Raw DEFLATE 压缩、238 字节载荷分包、带响应的 GATT 写入及完成通知。

1. 在自己的 Windows 10/11 笔记本上开启蓝牙，按 [Windows 助手说明](windows-panel/README.md) 安装并运行本机助手。默认通过 `127.0.0.1:19743` 与 FastTask 页面通信，蓝牙由 Windows 直接访问，Go 服务与 WSL 不访问蓝牙。
2. 登录 FastTask 后进入“pannel”，上传头像或证件照，设置姓名；顶栏标题等非必要配置在“更多资料设置”内。点击“保存资料”保存。模板参考 `myled/output` 工牌：800×480、黑白红、头像与姓名、当日天气和三行任务表。照片等比完整显示；长任务连续换行分页，超过三行不会被静默截断。画面不显示座位或状态解释，页脚仅保留页码。
3. 天气固定查询武汉市东西湖区，来源为 [Open-Meteo](https://open-meteo.com/en/docs) 模型估算，默认缓存十五分钟；故障时只允许六小时内的当天结果并标注缓存，不能取到时显示不可用。可手动修正显示，墨水屏同时标注“手动”。不会以示例天气替代查询结果。
4. 今日任务按 FastTask 的原顺序完整读取，包括未完成或已跳过项，不需要手动选择或勾选完成；已被新版本替代的项不会重复显示。任务内容和完成情况在原工作台/目标任务模块维护，pannel 不调用任务修改、完成或撤销接口。历史屏上文案覆盖继续兼容，旧选择和排序设置不再隐藏或重排任务。
5. 输入自己墨水屏的蓝牙广播名称，点击“连接设备”，核对名称和对应 MAC 地址。首次连接会创建当前账号的只读配对并默认启用固定时间同步；已有同账号、同来源的有效配对直接复用。需要立即查看效果时可点击“更新墨水屏”。全部写入应答后还须收到准确的 `ff00000000000000` 完成通知才报告成功。超时或断开时查看实机后重试，不自动重复发送。
6. 每天北京时间 **14:00、17:30、21:00** 自动读取真实当日任务、完成状态和天气，按保存的头像与资料重新渲染。这个流程由 Windows 助手执行，不依赖网页持续打开，也没有单独的定时设置卡片。配对凭据只可读取工牌快照，不能修改任务。多页内容默认同步第一页，预览可查看其余页。

电脑必须开机、未休眠、FastTask 与 Windows 助手运行、蓝牙开启、设备在附近。恢复运行后只补当天最近一个遗漏时点；失败会显示原因，不自动重发同一时点。可运行助手的 `install-autostart.ps1` 安装当前 Windows 用户登录自启。退出网页账号不会撤销本机助手的独立只读配对；结束本机助手进程可停止自动同步，切换账号或来源须重新连接配对。

云端部署保留现有 Go/React 服务与数据库方案；Windows 助手仍安装在用户电脑，无需给云服务器增加蓝牙或 Python。使用正式 HTTPS 域名配置 `FASTTASK_PUBLIC_URL`，并在本机执行 `windows-panel/start.ps1 -Origin "https://你的域名"`；登录该云站后，在“连接帮助”允许浏览器访问本机助手并重新配对。云端构建会排除整个 `windows-panel/`，防止本机私密配对配置进入构建缓存。域名迁移、反向代理 CSP 与登录自启步骤见 [助手云端部署说明](windows-panel/README.md#云端部署后的使用)。

浏览器 Web Bluetooth 保留为备用方式；本机 Windows 原生蓝牙已通过实机验证，浏览器 GATT 曾遇到连接失败。使用备用方式时须在 Edge/Chrome 的设备选择器中确认广播名称，能力与用户手势要求参考 [Chrome Web Bluetooth 文档](https://developer.chrome.com/docs/capabilities/bluetooth)。

协议回归可使用支持原生 TypeScript 类型剥离和 `module.registerHooks` 的 Node.js 运行（本地验证使用 Node 25.9.0）：

```bash
npm --prefix web run test:panel
```

测试覆盖参考项目的 AES 向量、位平面、分块解压、分包、设备 ID 筛选、完成通知以及取消/断开；模拟 GATT 测试不能代替真实 Windows 蓝牙适配器和墨水屏联调。

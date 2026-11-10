# FastTask 部署环境

本页记录 2026-11-10 现场核实的部署信息。公网域名的 DNS 地址可能是反向代理地址，不能据此判断应用源站。

## 生产环境

| 项目 | 当前配置 |
| --- | --- |
| 公网入口 | `https://task.fastrd.io` |
| 实际源站 SSH | `ssh root@47.110.133.67` |
| 项目目录 | `/home/dev/FastTask` |
| 配置文件 | `/home/dev/FastTask/.env` |
| 数据库 | `/home/dev/FastTask/data/fasttask.db` |
| 附件、音频 | `/home/dev/FastTask/data/attachments`、`/home/dev/FastTask/data/audio` |
| 应用监听 | `127.0.0.1:10000` |
| 服务管理 | `dev` 用户级 systemd：`fasttask.service` |
| 备份目录 | `/home/dev/FastTask-backups` |
| 当前程序版本 | `0.1.0-gantt`，构建提交 `60ebca7bc86e2782497125d44251f0b0780354d5` |
| 数据库版本 | 24 |

读取服务状态：

```bash
ssh root@47.110.133.67 'runuser -u dev -- env XDG_RUNTIME_DIR=/run/user/$(id -u dev) systemctl --user status fasttask.service --no-pager'
```

生成一致性数据库备份时，使用源站当前版本的程序：

```bash
cd /home/dev/FastTask
runuser -u dev -- ./bin/fasttask backup --output /home/dev/FastTask-backups/<本次备份目录>/production.db
runuser -u dev -- ./bin/fasttask backup --verify /home/dev/FastTask-backups/<本次备份目录>/production.db
```

先创建由 `dev` 用户拥有的备份目录。运行中的 SQLite 使用应用备份命令或 SQLite backup API，不直接复制数据库主文件。

### 2026-11-10 甘特图生产发布

用户完成测试站验收后授权发布。生产于北京时间 11:58:46 开始更新，11:58:47 恢复就绪。源站使用独立工作树构建 Go 程序，网页使用通过测试的构建；随后更新原目录中的程序及网页，保留原生产配置、数据库、附件、音频和 Node sidecar。

发布前发现源站有未提交的 FastCAS 服务鉴权修复，已保留并以 `0363493` 纳入仓库，再与甘特图一同测试发布。原工作树补丁、未跟踪源码及 Git stash 均保留。

本次备份目录：`/home/dev/FastTask-backups/gantt-release-20261110`，仅属主可访问，包含：

- `runtime.tar.gz`：旧程序、网页、生产 `.env` 及 sidecar 构建。
- `production.db`：停服后生成且校验通过的第 23 版一致性数据库。
- `data-files.tar.gz`：同一时点的附件和音频；另备份 systemd 单元及原源码改动。
- `migration-verification.json`、`doctor.txt` 和发布日志：迁移及组件检查结果。
- `rollback.sh`：恢复本次发布前的程序、网页、配置和数据库，并重启服务。

迁移先在副本演练，再对生产执行。服务启动前逐行比较 40 张表的 14,762 行原有数据，内容一致；仅增加第 24 版迁移记录和两个空日期列。完整性检查为 `ok`，外键问题为 0。公网版本、资源哈希、登录入口、鉴权拒绝、浏览器渲染及 sidecar 健康检查通过。

需要立即回退本次发布时，在源站执行：

```bash
bash /home/dev/FastTask-backups/gantt-release-20261110/rollback.sh
```

该脚本恢复到上述备份时点，先把当前数据库及程序移入备份目录下的 `failed-runtime-*`。发布后若已有新业务写入，应先评估数据恢复范围。源码保留在发布提交；旧源码改动可从补丁和保存的 stash 找回。

## 测试环境

| 项目 | 当前配置 |
| --- | --- |
| 公网入口 | `https://task.fast.0x535a.cn` |
| 主机 | 当前工作机 `debian13-sz`，内网 `10.22.33.15` |
| 容器 | `fasttask` |
| Compose 项目 | `fasttask-test` |
| Compose 定义 | `deployments/compose.test.yml` |
| 当前镜像 | `fasttask:workbench-controls-test-20261110`（前端 `a53395b`，沿用甘特图测试后端） |
| 端口映射 | `10.22.33.15:10000 → 容器 10000` |
| 独立数据目录 | `/root/dockerData/fasttask-gantt-20261110` |
| 部署参数 | `/root/repo/FRD/FastTask/backups/workbench-controls-test-20261110/compose.env` |
| 独立环境配置 | `/root/repo/FRD/FastTask/backups/gantt-test-20261110/test.env` |

测试环境使用专用 Compose 配置；它要求显式提供镜像、测试环境文件和数据目录，并固定测试公开地址及关闭通知。根目录 `.env` 中保留了生产配置，不能把它作为测试部署参数使用。

```bash
docker compose \
  --env-file backups/workbench-controls-test-20261110/compose.env \
  -f deployments/compose.test.yml up -d
```

账号和独立测试密码保存在本机 `backups/gantt-test-20261110/access.txt`，该目录不进入版本控制。测试副本使用本地账号密码登录，数据账号保留原标识；测试密码与生产密码独立。

## 主题入口点击修复部署与回滚（2026-11-10）

前端修复提交 `a53395b`，镜像 `fasttask:workbench-controls-test-20261110`。部署前备份位于测试数据目录的 `backups/before-workbench-controls-20261110.db`，已通过应用校验。

真实公网浏览器验证了旧 PWA 收到更新提示并刷新、两个入口的文字与整行空白点击、Enter/Escape 键操作、亮暗切换及经典主题往返。测试服务健康正常，后端及数据版本保持一致。

回退到上一版工作台界面：

```bash
docker compose \
  --env-file backups/workbench-controls-test-20261110/rollback.env \
  -f deployments/compose.test.yml up -d
```

## 首次工作台主题测试部署与回滚（2026-11-10）

本轮仅更新前端，使用 `deployments/docker/web-test.Dockerfile` 和 `web/dist` 构建上下文；镜像 `io.fastrd.fasttask.web-revision` 标签为 `5c54e3c`。基础镜像为 `fasttask:gantt-test-20261110`（镜像 ID `1fa5d5ca20d42361f638aaaad4ba0605ae513419f914ad39d16ccc8a626a34f8`），后端二进制、配置和数据目录保持一致。

部署前通过应用的 SQLite 备份命令生成并校验：

```text
/root/dockerData/fasttask-gantt-20261110/backups/before-workbench-20261110.db
```

回滚前端使用保留的旧镜像和部署参数：

```bash
docker compose \
  --env-file backups/workbench-test-20261110/rollback.env \
  -f deployments/compose.test.yml up -d
```

本轮不涉及数据库迁移，回滚无需恢复数据库，因此可保留验收期间的测试数据。部署后检查测试公网 `/health/ready`、前端资源和实际登录页面；`/health/version` 仍显示基础镜像后端版本 `0.1.0-gantt-test`，前端版本以镜像标签为准。

## 上一轮甘特图测试部署回滚

旧测试容器 `fasttask-before-gantt-20261110` 已停止保留，原数据目录 `/root/dockerData/fasttask` 保留原状。本次使用新的数据目录，无需把已迁移的数据库交回旧程序。

```bash
docker stop fasttask
docker rename fasttask fasttask-gantt-rollback-held
docker rename fasttask-before-gantt-20261110 fasttask
docker start fasttask
```

执行后检查测试公网 `/health/ready` 和 `/health/version`。回滚前先确认保留容器存在，且待使用的容器名称没有冲突。

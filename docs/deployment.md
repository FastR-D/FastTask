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

本次甘特图工作仅在生产源站创建并读取备份，生产程序、配置、数据库版本和运行服务均未升级。

## 测试环境

| 项目 | 当前配置 |
| --- | --- |
| 公网入口 | `https://task.fast.0x535a.cn` |
| 主机 | 当前工作机 `debian13-sz`，内网 `10.22.33.15` |
| 容器 | `fasttask` |
| Compose 项目 | `fasttask-test` |
| Compose 定义 | `deployments/compose.test.yml` |
| 当前镜像 | `fasttask:gantt-test-20261110` |
| 端口映射 | `10.22.33.15:10000 → 容器 10000` |
| 独立数据目录 | `/root/dockerData/fasttask-gantt-20261110` |
| 部署参数 | `/root/repo/FRD/FastTask/backups/gantt-test-20261110/compose.env` |
| 独立环境配置 | `/root/repo/FRD/FastTask/backups/gantt-test-20261110/test.env` |

测试环境使用专用 Compose 配置；它要求显式提供镜像、测试环境文件和数据目录，并固定测试公开地址及关闭通知。根目录 `.env` 中保留了生产配置，不能把它作为测试部署参数使用。

```bash
docker compose \
  --env-file backups/gantt-test-20261110/compose.env \
  -f deployments/compose.test.yml up -d
```

账号和独立测试密码保存在本机 `backups/gantt-test-20261110/access.txt`，该目录不进入版本控制。测试副本使用本地账号密码登录，数据账号保留原标识；测试密码与生产密码独立。

## 本次测试部署回滚

旧测试容器 `fasttask-before-gantt-20261110` 已停止保留，原数据目录 `/root/dockerData/fasttask` 保留原状。本次使用新的数据目录，无需把已迁移的数据库交回旧程序。

```bash
docker stop fasttask
docker rename fasttask fasttask-gantt-rollback-held
docker rename fasttask-before-gantt-20261110 fasttask
docker start fasttask
```

执行后检查测试公网 `/health/ready` 和 `/health/version`。回滚前先确认保留容器存在，且待使用的容器名称没有冲突。

# Agent / Harness 验收记录

日期：2026-10-29 至 2026-10-30。基线 `90d0af7`。生产代码已分批提交推送；全部实施步骤见 [实施记录](implementation-progress.md)。本记录区分自动化、真实模型行为和人工完整联调，不把场景设计当作通过结果。

## 已执行的自动化

- `go test ./...`：通过。
- harness / SQLite 并发与故障组 `-race -count=3`：通过；最终交付复核通过。
- `npm --prefix web test`：55 项通过，0 失败、0 skip。
- `npm --prefix web run build`、`npm --prefix web run build:sidecar`：通过。
- Chromium 156 真实 JSPI / libfx WASM：工具身份与执行、checkpoint、CSP 允许编译 / 禁止编译、在途 owner 切换、停止送达失败提示及重试通过。fixture 通过 CDP 注入，运行的是实际 host / API / driver 代码；不等于整个 React 页面所有操作已联调。
- 真实 libfx/node + shim + HTTP 假模型：事件消费、一次工具执行、checkpoint 保存与恢复通过。
- 数据库：真实版本 21 schema 升级到 23、opaque checkpoint 保留、迁移幂等、新增迁移镜像一致通过。

## DeepSeek 行为评测

用户已明确授权使用当前 `.env` 配置执行完整 20 场景 × 3 次。配置核对为 `api.deepseek.com` / `deepseek-flash`，本地 Provider 表没有覆盖。评测只使用该配置的凭据，新建独立 fixture 数据库；没有对真实用户业务库写入评测数据。

首轮完整 60 次：37 通过、23 未通过。此前首次建方案试验还发现并修复了 DeepSeek thinking 的 reasoning_content 回传问题。原始失败报告与证据保留在本机 `/tmp/fasttask-agent-deepseek-before-reasoning-fix`、`/tmp/fasttask-agent-deepseek-eval`，不把后续修复覆盖成原先就通过。

修复后完整复跑及定向复测的最新每例三次合计 **60 / 60 通过**，硬不变量违反 0 次；这不是一次无失败的完整运行。完整复跑保留 36 次，加上冲突 / 拒绝复测 6 次、恢复 / 历史 / 取消 / owner / 图片 / unknown 复测 18 次。具体来源、评分、证据 SHA256 见 [评测汇总](evaluation-results.json)。其中 57 次调用真实模型执行任务，E16 的 3 次为故意挂起 Provider 的取消故障试验，按真实取消状态与浏览器反馈评分。无效 JSON 评审只在原证据上重评；有效低分没有反复抽样。原始证据含供应商 reasoning 与隔离数据，保存于私有临时目录，不提交 Git。提交的汇总不含密钥、capability、图片 bytes 或完整 reasoning。

评审使用单独的 DeepSeek 请求检查实际数据库前后快照、模型 / 工具 / 审批 trace 和输出。确定性 machine checks 与不变量先约束结果；模型评审不能替代权限、事务与副作用的程序保证。缺证据、评分 JSON 不完整均不能当作通过。无效评审仅可在同一份原始证据上重新请求，不重新抽样有效低分。

夹具修正：E13 原有“数据核对”曾与用户要求新增的“标签核对”完全重叠，现改为数据文件完整性检查，仍要求恰好新增两项；E06 原输入“调整一下”未明确调整内容，现明确“今天只有 60 分钟，三项压入该预算”，使拒绝试验能稳定触发首份合法提案。E03 第二份提案待批时由夹具终止 host，明确记录此切面，禁止自动批准新内容。所有旧输入、失败与修正理由保留，不降低业务硬约束。

## 规范中的覆盖边界

[86 项测试规范](automated-tests.md) 是验收目标。已实现重点包括规范输入 / 图片归属、权威上下文重建、独立取消、执行预算、model attempt 单飞、工具 identity / claim / replay、审批与业务事务、终态 fencing、事件回放 / 快照、升级、双 host SDK 合约、配置和 usage。下列目标仍不能只靠现有自动化签收：

- 全部 QA01–QA18 的完整 React UI、旧页面、新后台组合与登录切换流程；本机只有一个真实 Chromium 版本。
- 真实浏览器原生网络层、断网 / 恢复联网 / Service Worker / 反向代理的完整矩阵；CDP fetch bridge 不证明这些网络部署条件。
- 所有事件边界的崩溃重启 / 磁盘故障穷举；当前使用真实 SQLite trigger 注入结果和终态写失败，另覆盖并发 replay。
- 从不同设备、不同浏览器进行人工最终体验评审。自动模型质量评分也不等于人类可用性结论。

因此，本报告不能签为“86 项逐条和 18 项人工 QA 全部通过”。自动化、真实 SDK 合约和 60 次行为评测提供上述实际完成证据，人工矩阵应按 [QA 手册](qa.md) 继续记录，不填虚构 run ID 或通过勾选。

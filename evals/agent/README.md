# Agent 回归与行为评测

在仓库根目录使用 Go 1.26、Node 24，先执行 `npm --prefix web ci`，浏览器合约另需 Chromium（本次为 156，支持 JSPI）。

```sh
node evals/agent/run.mjs regression
npm --prefix web test
go test ./...
go test -race ./internal/application ./internal/persistence -run 'TestHarness|TestConcurrentEvents|TestApprovalIntervals|TestAttemptClaim' -count=3
```

`regression` 读取真实 `go test -json` 的 pass 事件；缺失、未执行或失败均非零退出。regression-map.json 的 20 个 E 场景只映射相关底层测试，不能证明模型行为质量，质量标为 `not_evaluated`。例如审批服务通过不能证明模型会生成合理提案。

## 真实 DeepSeek 执行

```sh
go test -c -o /tmp/fasttask-agent-live.test ./internal/application
FASTTASK_EVAL_OUTPUT=/tmp/fasttask-agent-new-run node evals/agent/run.mjs live -- node evals/agent/adapter.mjs
```

使用当前 `.env` / 环境中的 DeepSeek 配置，测试显式要求 DeepSeek。会产生真实调用费用。默认 20 例各三次，最多三个并发 trial；每例进程最长五分钟。可用 `FASTTASK_EVAL_CASES=E03,E06` 定向复测，`FASTTASK_EVAL_BINARY` 指定已编译的测试二进制。修改 Go fixture 后必须重新编译。每次使用新的输出目录保留旧失败证据。

runner 向 adapter stdin 提供完整 case、seed_recipe、trial、隔离库要求；adapter stdout 只能输出一个结果 JSON（上限 8MiB）。它不调用 shell 执行模型输出。真实 fixture 位于 `internal/application/agentlive_test.go`，普通 Go 回归会跳过付费用例。

每次创建独立 SQLite 数据库，通过真实应用服务、model proxy、工具 registry、审批事务和 libfx/node + shared driver 执行。种子包含版本冲突、用户拒绝、旧 checkpoint、不同删除批次、跨用户数据、图片和 claimed unknown。E19 第二次使用真实 Chromium libfx/wasm，经 CDP fetch bridge 连接真实 Go fixture。E16 故意挂起测试 Provider，验证取消与无后续请求，属于故障试验；不把缺少正常回答判作质量失败。Worker 的实际 ExecuteRun/sidecar 收尾另有 Go 集成回归。

结果必须包含独立 database_id、database_before / after、trace、全部 state_assertions 的证据、三项硬不变量以及质量评分 / 理由 / 全部 quality_checks。verify.mjs 对缺失、false、假模型标记、评分低于 2 均拒绝。它是可信 adapter 的结构检查器，业务真实性由 fixture 的数据库 / trace 断言保证，不能仅凭模型声称成功签收。确定性状态断言及真实浏览器组件证据优先于评审模型对状态的猜测；质量正文仍单独评审。E16 使用确定性取消合约评分。

独立 DeepSeek 请求核对实际产出。无效 / 缺失 JSON 评审记为不可用，可只对同一份证据重评：

```sh
node evals/agent/rescore.mjs /tmp/fasttask-agent-new-run
```

只接受 rationale 以 `review unavailable:` 开头的记录；拒绝重抽有效低分。旧不可用评审保留。修复产品或夹具后的定向复测应单独保留来源，不能覆盖旧失败并声称原运行通过。

输出默认 `/tmp/fasttask-agent-eval`，目录 0700、证据文件 0600。原始证据含隔离业务数据、供应商 reasoning，不提交 Git；不保存密钥或请求授权头。Git 中仅保留 [脱敏结果与证据摘要](../../docs/agent-harness/evaluation-results.json)。本次最终每例三次共 60 项验收结果来自完整复跑与修复后定向复测，来源及覆盖边界见 [验收记录](../../docs/agent-harness/verification-report.md)。

## 浏览器与人工 QA 边界

真实 Chromium 合约验证 JSPI / WASM 工具、checkpoint、CSP 编译允许与拒绝；另验证实际 host owner 在途交接、停止失败反馈与重试、恢复范围 React 组件。CDP 注入和 fetch bridge 不覆盖浏览器原生网络、部署代理或整个 React 应用全部操作。Node 合约覆盖真实 libfx/node + HTTP 假模型。Chromium 缺失会显式 skip，发布验收须检查未 skip。

QA01–QA18 的完整 UI、历史前后台版本、不同浏览器及断网部署矩阵尚未全部执行，按 [QA 手册](../../docs/agent-harness/qa.md) 继续记录，不能把底层自动化或模型评分等同于人工签收。

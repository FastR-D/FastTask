# 实施记录

2026-10-29 第一批：规范当前输入与 host 无关取消。

已实现：

- grant 从已入库 parent message 提供 prompt，浏览器不再优先使用本地暂存文本；纯图片 prompt 不复用前一轮文字。
- 模型代理以已入库当前消息替换 host 的当前用户输入；当前图片按用户、线程和消息绑定校验后展开。缺失 / 越权附件明确拒绝，图片 bytes 不写回消息。
- sidecar、WASM 的状态均保留 runId；停止按 runId 请求服务端，兼容没有本地实例的页面。停止发送失败显示错误，本地实例尽力中止。
- 新命令的活跃检查包含 cancelling。
- 显式配置的 MaxOutputTokens 接入代理请求 cap；默认配置仍沿用现有行为。

回归：增加三个 Go 服务集成测试和三个前端取消控制测试，覆盖规范文本、图片实际展开、旧输入替换、越权 / 丢失文件、输出 cap、sidecar 状态恢复和取消。Go application / httpapi 测试通过；前端 45 项测试通过。前端生产构建、sidecar 构建与 TypeScript 检查通过；新增 Go 测试以 `-race -count=3` 运行通过。

尚未完成：checkpoint 元数据与 baseline 恢复、执行期间 deadline、model step / attempt 单飞、有序事件事务、所有网络 signal 贯通、Provider 参数适配、共享 driver、完整 86 项用例及真实双 host / 人工 QA。当前不能把本批回归视为整个设计已验收。

规范输入仍通过代理校验最终上游请求；libfx 的真实 resource / checkpoint 合约及完整视觉模型端到端验收留待后续测试任务。没有新增迁移，没有修改业务提案与审批权限。

第二批：权威历史恢复。

采用设计允许的“始终由服务端重建模型上下文”替代 checkpoint 水位 / baseline 双轨方案。每次代理请求按数据库消息、part 次序恢复原角色的文本及工具交互，读取晚到审批结果；host 伪造历史不进入模型。浏览器与 sidecar 不再恢复 opaque checkpoint，历史文件保留兼容，不需要新增水位迁移，也不存在代理 baseline 被新 checkpoint 遗漏的问题。历史材料不再拼入 system，当前输入仍做严格附件校验。读取失败不会静默返回空历史；修正 system UTF-8 上限含标记的计算。

验证：application / httpapi 回归通过；新增 stale / 损坏 checkpoint、晚到结果连续读取、工具前后文本次序、system 角色边界和中英文截断上限测试通过。后续补 token 预算和实际双 host 合约，不能把本步视为整体 QA 完成。

第三批：执行边界与事件一致性（数据库升级至 22）。

模型请求通过数据库 CAS 抢占单次 attempt，失败建连 / 503 也计入独立 attempt 上限；响应头、流空闲与主动运行期限独立约束。独立取消观察器可以打断未返回字节的请求。人工等待按 run 状态区间计时，不再按重叠 long poll 相加，reaper 可在没有轮询时收尾。未知 stop reason / 不完整流不能直接成功。

事件序号与插入、快照 fold、游标更新同一事务提交；过期本地 session 不覆盖事件生成的快照。终态检查、最终事件及 token 撤销同事务，晚到完成不能复活终态；文本 / 工具参数收尾使用独立限时 context，传播持久化错误。

验证：application / persistence / httpapi 回归通过；新增真实 HTTP 假模型的 header / idle / active / heartbeat 超时、无字节取消、503 attempt 上限、模型单飞、不完整流测试；20 路事件并发及回放一致性、20 路 attempt 抢占、审批区间计时、终态 fencing 通过。相关组 `-race -count=3` 通过，两份迁移一致。

第四批：工具上下文与版本前置条件。

新增只读 get_planning_context，在同一 SQLite 事务内读取主目标、任务版本、至多 30 条候选、当日计划及设置。任务树支持 limit / offset，默认仅平铺表示，显式请求才返回 tree；读取的树与 revision 在同一事务。已有树提案必须使用本 run 实际只读结果中的观察版本，显式 0 不再被当成省略；过期或未观察基线要求重读。

系统提示改为按需读取，网页入口不再建议先读全部业务；不声称默认存在未注册材料工具。历史上下文有明确文本字节预算，长工具结果带截断标识和关键版本 / 审批字段；用户约束与审批事实保留，超出不可安全裁剪范围时明确失败，不悄悄丢约束。图像传输 bytes 与文本预算分开计算。

验证：application / httpapi 回归通过；新增分页单一表示、规划快照、未观察 / 已观察 / 显式零版本及约束保留 / 截断标记测试通过。此实现采用确定性裁剪与重读，没有引入额外摘要模型调用。

第五批：共用 driver、网络取消、配置和观测。

browser / sidecar 现在共用 run-driver，统一 heartbeat（等待上一次结束后再排下一次）、manifest、工具执行、审批长轮询、turn 消费、checkpoint 采集及 agent.close；各 adapter 负责最后的服务端完成信号。普通 / 审批网络操作有明确 deadline 并传 signal，cleanup 请求独立于主 signal。新增环境预算配置，reasoning 使用兼容 SDK 实际采用的 reasoning_effort，provider-default 不发送参数。供应商 usage 按 attempt 保存白名单计数，未知保留 known=false，host report 不重复累计。小文本 delta 合并后提交，最终 flush 保留收到的文本。

验证：完整 `go test ./...` 通过；前端 49 项通过；生产前端与 sidecar 构建通过。新增真实 libfx/node + 真实 shim 的工具身份执行、事件消费、checkpoint 保存 / 恢复合约通过（非 fake createAgent）；共用 driver 的输入、token 更新、usage、失败和已取消启动测试通过。1000 个文本 delta 的最终文本 / usage 正确且持久化事件低于 100 条。真实 WASM 浏览器与完整验收矩阵继续执行。

第六批：执行权与审批事务（数据库升级至 23）。

并发 grant 的撤销 / 创建同事务，旧 principal 在模型 attempt 与工具 claim 时再次校验。轮换令牌的后继值使用 AES-GCM 加密保存，30 秒同 generation 宽限可重取相同后继令牌，解决轮换响应丢失；新 grant 仍立即封禁旧 generation。审批在业务写事务内再次检查取消与 pending 状态，业务写入与最小 applied 回执同事务；详细卡片字段随后补充。拒绝及结果状态合并写入，重复回执不能把已批准结果改为冲突。模型完成必须有服务端确认的最终 stop，不能只凭 host 声明或某次已完成工具调用判成功。工具完成后的结果清理使用独立限时 context。

验证：完整 Go 回归通过；新增并发 20 次 grant、旧 principal、轮换响应丢失 / grace 到期 / 新 generation 封禁、approve / reject / revision conflict / cancel / cross-user、两 service 重复审批及回执失败回滚业务测试。真实浏览器合约单独交付并记录容器网络适配方式。

第七批：工具身份回归、结果事务和错误脱敏。

新增完整代理 SSE → finalized call → 真实 registry 执行 → SQLite result 的服务集成测试。覆盖缺失 / 伪造 ID、工具名 / 参数变化、20 路并发、响应丢失后的原结果重放、claimed unknown、取消 / 旧轮次 / 旧 generation 拒绝，以及 host 跳过工具伪报成功和重复终态。工具结果与对应事件 / snapshot 同事务提交，持久化失败返回明确未确认结果。Provider 非成功 body 仅用于能力分类，客户端错误与终态不再保存任意上游 body；避免截断后只剩半个密钥而脱敏失败。host 错误上报同样使用服务端固定说明。

验证：application / persistence 全量通过；新增真实工具身份 / 完成与错误 body 脱敏测试通过。行为评测 runner 与验收报告单独交付，相关底层回归不能替代真实模型质量验收。

第八批：真实双 host 合约、owner 交接和故障验收。

真实 Chromium 使用 JSPI / libfx WASM 执行工具并保存 checkpoint，同时验证现行 wasm CSP 允许编译、移除 wasm-unsafe-eval 后拒绝编译。测试通过 CDP 注入隔离 bundle / fetch 夹具，不依赖容器 Chromium 的网络直连；Node 合约单独覆盖真实 HTTP。浏览器 grant / checkpoint 的异步边界重新核对 owner 和 access subject，模型 / 工具网络入口也复核；旧 owner 返回不再启动新账号 driver 或覆盖通知。未知 SDK stop reason 不默认当成功，清理移除父 signal listener。

新增 HTTP 的 JWT / capability 双向隔离及 run / thread scope、取消后工具拒绝；多个 pending 提案、detached 审批、审批超时 reaper、取消后保存已提交成果、20 路终态争抢；SQLite trigger 注入结果 / 终态事件失败并验证整体回滚、claim 不释放重做。reaper 不再吞掉数据库写失败。旧版本 21 schema 真实升级到 23、checkpoint 保留与迁移幂等测试通过；新迁移 22 / 23 两份 SQL 字节一致（历史版本 1 的两份文件已有排版差异，本批不改旧迁移）。

前端 52 项测试全部通过，其中真实 Chromium 合约实际运行、无 skip；生产前端与 sidecar 构建通过。底层 race 组与最终完整 Go 回归在最后交付记录再次汇总。用户已授权使用当前 DeepSeek 配置运行完整真实模型评测；具体行为结果另行报告，不能从这些底层回归推定模型质量。

第九批：真实 DeepSeek 评测发现并修复的 thinking 工具兼容性。

真实 deepseek-flash 首轮可调用工具，第二轮曾返回 HTTP 400，供应商明确要求回传 reasoning_content。权威上下文现在为 DeepSeek 回传原始已持久化 reasoning；同一模型响应的并行工具调用保持在一个 assistant 消息，再按顺序配对所有结果。普通 Provider 不增加 DeepSeek 字段；关闭 reasoning 持久化或显式 none 时关闭 DeepSeek thinking，避免依赖未保存的 chain。裁剪一次移除整组调用和结果，避免多工具结果悬空。

依据：[DeepSeek Thinking Mode](https://api-docs.deepseek.com/guides/thinking_mode)，tools 模式要求回传前序 reasoning_content。网页核对与实际 HTTP 400 诊断一致。仅上游请求使用这些已有 reasoning 记录，密钥仍不下发。

验证：新增精确 reasoning、并行调用分组、普通 Provider 字段隔离、关闭持久化与整组裁剪测试；完整 Go 回归和相关 `-race -count=3` 通过。修复后真实 DeepSeek 无目标整批提案烟测以 4 次调用 / 4 次 attempt 正常结束并完成一次批准，独立评审 3/3；工作区入口烟测同为 3/3。完整 60 次评测重跑，原失败证据保留，不能把两项烟测作为全套通过。

第十批：完整真实评测后的修复。

首轮完整 60 次试验完成，37 次通过、23 次未通过，原始报告保留。根据实际 trace 修正：只读咨询不自动创建结构提案；用户拒绝后不自动换提案继续；unknown 结果先具体核查；图片无文字时的默认分析提示在 grant 与最终代理请求中使用同一常量。图片已实际到达 DeepSeek，其视觉能力可识别红色斜线；问题是历史数据核对指令压过了未携带默认提示的当前图片输入，并非未传图片。

浏览器 owner 改变后，旧 host 不能覆盖新账号通知，并清理自己的旧 running / runId 状态。停止回调复用可测试的失败反馈函数，取消请求按 owner 绑定，失败保留“未送达”反馈与可重试语义。SDK 心跳遭遇明确 401 / 403 / 409 时中止本地执行，避免服务端终态 / token 撤销后本地仍空重试。

验证：完整 Go 回归通过、前端 53 项全通过、生产前端及 sidecar 构建通过。真实浏览器 owner 在途交接验证：grant=1、模型请求=0、旧 callback=0、状态回到 idle；真实浏览器点击停止的失败 / 重试反馈验证通过。新增心跳撤销后关闭实例的自动化回归通过。部分首轮失败来自夹具：E13 的“原数据核对”原先恰好定义成了用户要求新增的标签核对，评测期待新增两项与语义去重冲突；现改为不重合的数据文件完整性核对，保留原种子和失败报告，不放宽实际新增两项的断言。E03 在第二份提案待批时由夹具主动结束，现明确记录该故障切面供评审，不把没有用户批准后的最终文本判成已批准。

最终版重新执行全部 20 场景 × 3 次，包含真实 WASM 图片 trial、真实 browser owner / stop feedback 证据；最终分布见验收报告。

第十一批：冲突反馈与预算终态收紧。

版本冲突的业务结果、proposal conflict、用户可见的“提案未生效”说明及对应事件现在同事务提交，不依赖模型恰好在下一次工具调用前生成一句解释。写失败向调用者传播，重复审批不会覆盖已完成结果。model turn 上限不再沿用旧的 succeeded 收尾，明确记录 failed / MAX_TURNS，并保留具体 incomplete 原因。

验证：审批冲突的可见说明、并发 / detached 回执、预算不进入 Provider 且不假成功的回归通过。完整 Go 与 race 检查已执行，最终验收记录汇总。行为评测工具及实际汇总后续提交。

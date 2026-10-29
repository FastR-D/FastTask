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

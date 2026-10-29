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

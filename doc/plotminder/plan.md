# P1
P0 技术栈选型：数据库用 **Drift**（SQLite 的响应式 ORM，数据变化自动推送 UI，省去手写 SQL），状态管理用 **Riverpod 2.x**（编译期类型安全，AsyncNotifier 天然处理数据库和 LLM 的异步操作，GetX 有维护危机不碰），路由用 **GoRouter**（Flutter 官方维护，声明式，后续 deep link 和 shell route 无需重构），不可变模型用 **Freezed + json_serializable** 代码生成。项目结构采用 feature-first 分层，核心是五张表：`tasks`、`axes`、`views`、`task_view_coords`（Task × View 的 per-view 坐标，这张表是"多 view 单 task"不变式的物理载体）、`edges`（DAG 依赖）。P0 不做任何真实 UI，只交付跑通的数据层、Repository CRUD、不变式单元测试、JSON 导出，以及一个用于验证数据流的临时 DebugPage。

# P2
????

# P3

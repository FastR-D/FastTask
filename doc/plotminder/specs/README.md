# Plotminder Specifications

本目录包含 Plotminder 项目按 phase 组织的功能规格。所有 spec 均受 [Constitution v1.0.0](../constitution.md) 约束。

## 规格组织

每个 phase 一份 spec,按依赖顺序排列。Spec 描述 **做什么 (what)**,不描述 **怎么做 (how)**——技术方案见对应的 `plan.md`,任务分解见 `tasks.md`(均由后续阶段生成)。

| Phase | 文件 | 里程碑 | 关键能力 |
|-------|------|--------|---------|
| P0 | [p0-foundation.md](./p0-foundation.md) | Alpha | 数据模型 + 本地持久化 + 跨平台骨架 |
| P1 | [p1-mapview.md](./p1-mapview.md) | Alpha | 2D 拖拽地图 + 视觉编码 + 三个用户时刻 |
| P2 | [p2-smart-axes.md](./p2-smart-axes.md) | Closed beta | 多 view + 自定义轴 + LLM 打分 |
| P3 | [p3-path-view.md](./p3-path-view.md) | Closed beta | DAG 依赖 + 路径推荐 + The Decide |
| P4 | [p4-imports.md](./p4-imports.md) | v1.0 public | 双向同步 + Share Sheet/Siri/Widget |
| P5 | [p5-live-mode.md](./p5-live-mode.md) | v1.0 public | 时间属性 + 番茄 + Reflect |
| P6 | [p6-3d.md](./p6-3d.md) | Future | 第三轴 + 3D 可视化 |
| P7 | [p7-cloud-sync.md](./p7-cloud-sync.md) | Future | 多设备同步 + 团队 workspace 雏形 |

## 阅读顺序

1. 先读 [Constitution](../constitution.md) —— 六条核心原则、质量底线、边界、决策启发式。
2. 按 P0 → P7 顺序读 spec。后续 phase 默认假设前置 phase 已完成。
3. 每份 spec 末尾的 **Constitution Alignment** 段落记录该 spec 与宪章可能的张力点,以及处理方式。

## 状态约定

每份 spec 的 `Status` 字段取以下之一:

- `Draft` — 起草中,可能大改
- `In Review` — 等待 review
- `Approved` — 已批准,可进入 plan 阶段
- `Implemented` — 已落地并通过验收
- `Superseded` — 已被新版本替代

## 修订规则

- Spec 的非破坏性修订不需要修宪。
- 当 spec 与宪章冲突时,**先修宪、再改 spec**——绝不反向。
- 任何 spec 提议的新功能落入宪章 [Out of Scope](../constitution.md#第四部分--边界-out-of-scope) 清单时,必须先走宪章修订流程。

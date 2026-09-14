# Feature Spec: P2 Smart Axes

**Phase**: P2
**Milestone**: Closed Beta
**Status**: Draft
**Created**: 2026-05-19
**Constitution Compliance**: v1.0.0
**Dependencies**: P0 Foundation, P1 MapView

---

## 1. 概述

P2 把 Plotminder 从"Eisenhower 矩阵的精美实现"提升到"用户自定义坐标语义的决策画布"。三件事:**多 view 系统、自定义轴、LLM 自动打分**。

P2 之后,Plotminder 才真正与所有竞品(TickTick 四象限、Eisenhower.me、Notion 模板)拉开差距。

成功标志:**一个用户能在同一个 task 集合上配置 3 个不同 view(工作、个人成长、产品决策),且每个 view 上 task 自动获得合理坐标,用户只需轻微拖拽微调**。

---

## 2. 用户故事

- **US-P2-1** 作为用户,我能从预设模板(Eisenhower / Impact-Effort / BCG 风险-收益 / 读书 / 健身 / 投资矩阵 / 自定义)中创建新 view。
- **US-P2-2** 作为用户,我能自定义 view 的两条轴的名称、含义说明、ideal corner 方位(右上/左上/右下/左下)、各轴权重。
- **US-P2-3** 作为用户,我能在 view 之间快速切换(顶栏 tab 或快捷键)。
- **US-P2-4** 作为用户,新建 task 时 LLM 根据当前 view 的轴语义自动推断坐标,并附一句话理由,我接受或拖拽覆盖。
- **US-P2-5** 作为用户,我拖拽覆盖 LLM 建议的次数越多,后续建议越贴近我的判断(few-shot 反馈)。
- **US-P2-6** 作为关心隐私的用户,我能选择 LLM provider(Anthropic / OpenAI / 本地 Ollama),并随时切换。
- **US-P2-7** 作为关心 prompt 透明度的用户,我能查看并编辑 LLM 使用的 prompt 模板。

---

## 3. 功能需求

### 3.1 多 View 系统

- **FR-P2-001** Workspace 可包含多个 view。Free 计划 ≤ 3 个,付费计划无限(具体见定价文档,本 spec 不约束)。
- **FR-P2-002** 每个 view 配置包含:
  - `name`(显示名)
  - `axes`: 数组,每个元素含 `axis_name`(如"重要")、`description`(轴语义说明,用于 LLM prompt)、`weight`(0.0–1.0,默认 1.0)
  - `ideal_corner`: 枚举 `top_right` / `top_left` / `bottom_right` / `bottom_left`
  - `task_filter`: 可选,按 tag / project / 自定义规则筛选哪些 task 出现在此 view
  - `default_zoom`: view 默认缩放级别
- **FR-P2-003** 同一 task 在不同 view 上有独立的 `TaskViewPlacement` 记录(P0 已建模),且 task 的其他属性(title、status、tags 等)跨 view 完全共享。
- **FR-P2-004** View 切换 UI:顶栏 tabs(可拖动重排);快捷键 Cmd/Ctrl+1..9 直达。
- **FR-P2-005** 切换 view 时保留当前 zoom 与 pan 状态(per-view 独立持久化)。
- **FR-P2-006** 删除一个 view 不会删除其包含的 task;只删除该 view 的 `TaskViewPlacement` 记录。

### 3.2 预设 View 模板

- **FR-P2-007** 提供以下预设(具体话术由内容团队后期打磨):
  - **Eisenhower** — 重要 × 紧急 → 右上
  - **Impact-Effort** — 影响 × 工作量 → 左上(高影响低工作量)
  - **BCG 风险-收益** — 风险 × 收益 → 右下(低风险高收益)
  - **想法画布** — 个人喜好 × 商业潜力 → 右上
  - **读书片单** — 难度 × 价值 → 左上
  - **健身目标** — 趣味性 × 难度 → 左上
  - **自定义** — 用户从零配置
- **FR-P2-008** 每个预设含示例 prompt 模板,用于 LLM 推断坐标时引导。

### 3.3 LLM 自动打分

- **FR-P2-009** 新建 task 时,系统**异步**调用 LLM 推断当前 view 上的 (x, y) 坐标 + 一句话理由。Capture 浮窗不等待 LLM 返回(宪章 VI.3)。
- **FR-P2-010** LLM 返回前,task 占据默认位置(地图中心或上次手动位置)。
- **FR-P2-011** LLM 返回后,task 自动移动到推断坐标,并在节点旁短暂显示推断理由(3 秒淡出,可在 detail panel 永久查看)。
- **FR-P2-012** LLM 推断 prompt 必须包含:
  - 当前 view 的轴名、description、ideal corner
  - 该 view 上最近 5–10 个用户**手动调整**过的 task 作为 few-shot 示例
  - 用户的输入文本(task title + description)
- **FR-P2-013** LLM 返回值必须包含:`x`(0–100)、`y`(0–100)、`rationale`(一句话,≤ 100 字符)。
- **FR-P2-014** Prompt 模板存储为**可见、可编辑**的资源文件(宪章约束 B);用户可在 Settings → Prompts 查看与覆盖默认模板。

### 3.4 用户覆盖与 Few-shot 回流

- **FR-P2-015** 当用户在 LLM 推断后**手动拖拽**移动节点超过阈值(例如 10 单位欧氏距离),记录为一次"override"事件。
- **FR-P2-016** Override 事件存入数据库,字段包含:`task_id`、`view_id`、`llm_suggested_coord`、`user_final_coord`、`timestamp`、`task_title_snapshot`、`task_description_snapshot`。
- **FR-P2-017** 后续同 view 上的新建 task,LLM prompt 自动注入最近 N 个 override 作为 few-shot。
- **FR-P2-018** 用户能在 Settings → Calibration 查看 LLM 与自己的"距离"统计(平均 override 距离、随时间变化趋势),作为 LLM 是否变得更懂自己的反馈。

### 3.5 Provider-Agnostic LLM

- **FR-P2-019** 必须支持至少三个 backend:Anthropic Claude、OpenAI、本地 Ollama(宪章 V.5)。
- **FR-P2-020** Settings → LLM 让用户配置 API key、选择 provider、选择模型。
- **FR-P2-021** 每个 LLM 调用通过统一 adapter 接口,核心代码不直接依赖任何 SDK。
- **FR-P2-022** 调用失败时(网络、key 无效、超限),task 仍创建成功,只是没有 LLM 推断坐标(落在默认位置),并在 task 上显示"LLM 不可用,可手动调整"的小图标。

### 3.6 离线 Fallback

- **FR-P2-023** 当 LLM 服务不可用时,新建 task 仍能完成(宪章 VI.4);系统不展示推断坐标但保留 task。
- **FR-P2-024** 离线创建的 task 在网络恢复后,**不**自动追溯调用 LLM——用户需主动触发(避免后台行为污染用户已手动放置的位置)。

---

## 4. 非功能需求

- **NFR-P2-001** LLM 推断 P95 响应延迟 < 2 秒(宪章质量底线)。
- **NFR-P2-002** Capture P95 延迟仍 < 5 秒,即使 LLM 调用未返回也满足(宪章 VI.1)。
- **NFR-P2-003** Prompt 模板所有变量必须以清晰命名出现,便于用户审计(避免黑盒)。
- **NFR-P2-004** View 切换 < 200ms(感知即时)。
- **NFR-P2-005** LLM API key 必须在本地加密存储(平台 Keychain / Credential Manager)。

---

## 5. 验收标准

- **AC-P2-1** 用户能创建一个自定义 view(轴名"成长价值"× "时间投入",ideal corner 左上),新建 5 个 task,LLM 推断的坐标合理(主观评估,但 8/10 测试场景需通过 reviewer 认可)。
- **AC-P2-2** 切换 LLM provider(Anthropic → OpenAI → Ollama)后,产品继续工作,推断质量在可接受范围内。
- **AC-P2-3** 关闭网络,新建 task 仍能完成;开启网络后,新建 task 恢复 LLM 推断。
- **AC-P2-4** 用户拖动覆盖 10 次后,LLM 在该 view 上的推断距离用户最终位置的平均欧氏距离明显下降(< 50% 初始距离)。
- **AC-P2-5** 用户能在 Settings 找到 prompt 模板,修改后看到 LLM 推断行为相应变化。
- **AC-P2-6** 在 3 个 view 间快速切换,每次切换 < 200ms,坐标渲染立即正确。
- **AC-P2-7** 同一个 task 在 view A 上坐标为 (80, 70),在 view B 上坐标为 (20, 90);切换两边,坐标都正确。

---

## 6. 范围

### In Scope
- 多 view 系统、自定义轴配置、预设模板、LLM 自动打分、few-shot 回流、provider-agnostic LLM、prompt 透明化。

### Out of Scope(本 phase)
- 依赖关系编辑(留给 P3)
- 路径推荐(留给 P3)
- 外部 task 导入(留给 P4)
- 三轴 view(留给 P6)

---

## 7. 依赖

- **前置 phase**: P0、P1
- **外部**: Anthropic API、OpenAI API、Ollama 本地服务(开发环境)

---

## 8. Constitution Alignment

| 宪章条款 | 关系 | 处理 |
|---------|------|------|
| III.1–III.5 (多 view 单 task) | **直接落地** | FR-P2-003、AC-P2-7 验证 invariant |
| I.1–I.2 (LLM 建议而非命令) | **核心保护** | LLM 推断后用户必须能拖动覆盖;FR-P2-009 异步设计避免 LLM 强迫等待 |
| I.5 (覆盖作为 few-shot 反馈,而非反向覆盖) | **直接落地** | FR-P2-015–018 落地此机制 |
| II (坐标驱动,非时间驱动) | **轴定义保护** | 预设模板中**禁止**出现以"deadline 紧迫度"为隐式默认轴的配置;用户主动选才行 |
| V.5 (LLM provider-agnostic) | **直接落地** | FR-P2-019–022 |
| VI.3 (capture 不阻塞等 LLM) | **直接落地** | FR-P2-009、AC-P2-3 |
| 约束 B (prompt 模板可见可编辑) | **直接落地** | FR-P2-014、NFR-P2-003 |

**潜在张力点**

- **关于 LLM 推断后自动移动节点位置**: 严格读宪章 I.3,"自动调整 task 坐标"似乎被禁止。但这里的"自动"是用户主动新建 task 后**首次**落点,并非对已存在的用户放置的坐标做后续修改。处理:在 UI 上明示"LLM 建议位置,可拖动",且**任何用户已手动调整过的节点,LLM 不会再次主动移动**。
- **关于 few-shot 中的 task title/description snapshot**: 涉及用户内容回流到 prompt。如果 LLM 是云端,这是数据出境。处理:Settings 中提供 toggle"允许 LLM 使用 override 历史作为 few-shot",默认开,用户可关。

---

## 9. 开放问题

- **OQ-P2-1** 预设模板的具体话术与 prompt 设计,需要内容团队 + 早期用户共创。
- **OQ-P2-2** "Override 距离阈值"(目前定为 10 单位)需要 dogfooding 验证。
- **OQ-P2-3** Few-shot 数量上限(目前定 5–10):太少效果差,太多 token 成本高。
- **OQ-P2-4** 三轴 / Z 轴是否在 P2 阶段预留 API hook?(P6 才正式做,但数据模型层可能要为它留位置)
- **OQ-P2-5** View 模板的"自定义"模式 onboarding 流程:wizard 式 vs 直接编辑器?

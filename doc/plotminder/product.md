# Plotminder 产品规格

> **在自己的地图上，做下一步决定。**
> Your axes. Your map. Your next move.

Plot 是用户做的（把事画到地图上），Minder 是产品做的（替你看着、提醒、推荐下一步），一个名字直接讲完了产品哲学里"用户判断主角、系统建议配角"那条分工——比 LOCUS 自带解释。

---

## 一、一句话定义

Plotminder 把任务、决策、想法放在用户自定义的二维坐标系上，并基于这些坐标推荐下一步的执行路径。

不是 todo list，不是日历，不是知识库——是一张可缩放、可拖拽、坐标语义由用户定义的**决策地图**。

---

## 二、目标用户

**主要用户：单兵作战的多线程工作者**

特征：一个人同时管多件性质不同的事；用过 Notion / Todoist / Apple Reminders 但都没坚持下来；受不了 list 越列越长却不知道做什么；重视 agency，反感"AI 替我自动安排"；愿意为生产力工具付 $10-30/月。

典型职业画像：独立 SaaS 创始人、freelance 设计师 / 开发者 / 写作者、一人多角的早期 PM、独立研究者、副业型知识工作者。

**次要用户：在多维度间做判断的专业角色**

产品经理（Impact × Effort 排 roadmap）、投资人（风险 × 收益 排 candidate）、创意工作者（兴趣 × 商业潜力 排项目）、学生 / 研究者（难度 × 价值 排目标）。

**反用户（不建议用 Plotminder 的人）**

重日历重时间块的人（Motion 更顺）、需要复杂团队协作的人（Linear / Asana 更顺）、只想要简单清单的人（Things / Todoist 更顺）。Plotminder 是有学习曲线的工具，第一周需要花 30 分钟配置 view，这一点不掩饰。

---

## 三、四条不动摇的产品原则

**1. 用户的判断是主角，系统的建议是配角。**
LLM 给出的坐标和路径都是建议，用户随时可以一键拖动覆盖。Plotminder 绝不偷偷修改用户的数据。

**2. 时间是属性而非轴。**
除非用户主动把"时间紧迫程度"设成一个轴，否则系统不会基于 deadline 自动排序。优先级来自用户对世界的判断维度，不来自系统对时间的计算。这一条把 Plotminder 从"又一个日程工具"变成"通用决策画布"。

**3. 多 view 单 task。**
同一个 task 可以同时存在于「工作」、「个人成长」、「产品决策」多个 view 上，坐标随 view 改变，但底层是同一个对象。

**4. 看见 > 自动化。**
让用户每周看见自己时间走向，比替用户自动决定更有价值。Plotminder 的杠杆在于"逼用户每周看见自己 axis-space 分布"，而不是"用 AI 替用户做所有决定"。

---

## 四、六个核心用户时刻

Plotminder 的产品体验围绕用户每天 / 每周会发生的六个关键时刻设计。

### 1. The Open（打开瞬间）

打开 Plotminder 看到的是当前 view 的地图。默认是 Eisenhower（重要 × 紧急），可一键切换到其他 view。地图可缩放：今天 zoom 看本周热点，季度 zoom 看全局分布。一目了然哪些是悬顶之剑（右上角红色大圆点），哪些可以慢慢做（中部），哪些已完成（淡化）。

**不是一个塞满日程的日历视图，而是一张可呼吸的地图。**

### 2. The Capture（捕捉瞬间）

全局快捷键（Cmd+Space / Ctrl+Space）唤起浮窗，自然语言输入一句话。LLM 自动推断当前 view 上的 (x, y) 坐标 + 一句理由。用户回车确认或拖动微调，全程 5 秒以内，不打断当前思路。

手机端用 Share Sheet / Siri Shortcut / Widget。开车路上一句"提醒我办车险"就进来了。

**这个时刻的成败决定日活。**

### 3. The Sort（整理瞬间）

地图上的连续拖拽 + 框选批量调整 + 双指缩放看局部细节。Shift 拖只改 X 轴、Cmd 拖只改 Y 轴、双击节点进 detail panel。

这是 power user 上瘾的细节——从 Notion 和 Linear 学的：让 keyboard ninja 觉得自己更快了，是产品口碑的关键。

### 4. The Decide（决策瞬间）—— **killer moment**

10 点钟、屏幕前、你问"现在做啥"。Plotminder 的回答是：

- 地图上高亮"下一站"
- 画出接下来 3-5 步的推荐路径线
- 每段路径附一句小字理由

**路径推荐逻辑是纯坐标驱动的**，不依赖 deadline：

1. 优先取当前 view 下 Pareto frontier 上的节点（没有别的节点在所有用户选的轴上都比它好）
2. frontier 内部按"距 ideal corner 的远近"排序（ideal corner 用户自己声明——Eisenhower 是右上、Impact/Effort 是左上、BCG 风险收益是右下）
3. DAG 依赖约束（A 没完不能开始 B）是硬条件
4. 平票时看用户给轴的权重

reasoning label 示例："重要 92 / 紧急 88 — 当前 frontier 最右上"、"被 [任务 X] 阻塞，X 完成后自动激活"。

**这是 Plotminder 跟所有竞品的差异化集中点，demo 视频应该围绕这一帧拍。**

### 5. The Complete（完成瞬间）

打勾时给一个微妙的小动画——节点褪色但留在地图上直到周末，路径线自动延伸到下一站。完成节点的"轨迹"在地图上可见，让用户能感觉到"今天确实做了事"。

参考 Apple Things 的小蓝勾——这是多巴胺循环的关键 5 秒。

### 6. The Reflect（复盘瞬间）

周日 20:00（或用户配置时间）自动弹出。看到的是：

- 本周地图 vs 上周地图的对比 + 可播放的 7 天演变动画
- 已完成节点在 axis-space 的分布热力图
- AI 一句话总结："本周完成 14 个节点，10 个在右上紧急区，2 个左上核心成长区。建议下周给左上至少 3 个节点。"

**这一句话直接踩中 Covey 哲学的母题**——人会被紧急事挟持、忽视真正重要但不紧急的事。Plotminder 用可视化方式把这洞察每周送到用户脸上。

**这是让人愿意长期用下去的钩子。**

---

## 五、关键交互机制

### Map View 的视觉编码

| 视觉属性 | 编码内容 |
|---------|---------|
| 位置 (x, y) | 用户在当前 view 两个轴上对该 task 的评分（连续 0-100） |
| 节点大小 | duration estimate（可选属性） |
| 节点颜色 | 状态（todo / doing / done / archived） |
| 节点形状/边框 | 类型（task / event / idea） |
| 透明度 | 距上次更新的时间（"年久失修"的节点会褪色） |

### 多 View 系统

每个 view 是一组配置：两/三个轴的名字与 ideal corner、包含哪些 task（全部 / 按 tag 过滤 / 按 project 过滤）、默认排序、默认 zoom level。同一个 task 在不同 view 上有不同坐标，但底层是同一个对象。

### LLM 自动打分

输入新 task → LLM 推断当前 view 上的坐标 → 给出一句话理由 → 用户接受或拖动覆盖。覆盖记录作为 few-shot 喂回 prompt，模型越用越懂用户。

任务从「估 5 个属性」简化到「估 2-3 个坐标」，准确率和用户信任度都高得多。

### 路径推荐算法

输入：当前 view 上所有 active 节点 + 用户的 DAG 依赖关系
输出：N 步的推荐路径（默认 N=5）+ 每段的 reasoning

逻辑：拓扑排序 → 帕累托前沿 → 距 ideal corner 远近 → 用户权重打破平局。所有决策仅基于坐标和依赖关系，不基于时间。

---

## 六、集成与导入

**双向同步架构**：每个 task 携带 `sources: [{platform, external_id, last_synced_at}]` 字段。每个平台一个 adapter 实现统一的 `fetch / push / subscribe` 接口。冲突默认 last-write-wins，保留 change history，冲突时弹 UI 让用户决定。

**优先级路径**：

1. **Apple 生态**（EventKit）：Reminders + Calendar + Notes
2. **Google 生态**（OAuth）：Calendar + Tasks
3. **Microsoft 365**（Graph API）：Outlook + OneNote + To Do
4. **第三方 task 工具**：Todoist + Notion + Linear（REST API）

**反向集成**：iOS Share Sheet（一键加入）、Siri Shortcut（语音输入）、iOS / macOS Widget（首屏快速 capture）、可选 Calendar 写入（路径上的 task 同步成日历事件）。

---

## 七、用户旅程

**第一个 5 分钟（onboarding）** —— 选一个预设 view（Eisenhower / Impact-Effort / 投资矩阵 / 读书片单 / 健身目标 / 自定义），导入一个外部数据源（Apple Reminders 或 Google Calendar），LLM 自动把现有 task 落到地图上，用户拖几下微调，出第一条推荐路径。冷启动的 5 分钟决定首日留存。

**第一天（first-day loop）** —— Capture 一次新 task、走完一条推荐路径、勾掉 2-3 个节点。傍晚收到一个 30 秒的当日小结。

**第一周（week-1 retention milestone）** —— 用户已经自然形成 1-2 个常用 view，每天打开 Plotminder 3-5 次，周日 Reflect 第一次触发——这是 week-1 aha moment。

**第一个月（month-1 advocacy threshold）** —— 用户已经在用 3 个 view，Reflect 数据足够形成趋势对比，至少推荐给一个朋友（"我现在用一个怪东西管事，挺好用的"）。

---

## 八、竞品差异化

**vs Eisenhower 类工具**（TickTick 四象限、Focus Matrix、Priority Matrix、Eisenhower.me、Notion 模板）
Plotminder 用连续坐标替代 4 格子，用多 view 替代固定二维，加上路径推荐。

**vs 决策矩阵工具**（Miro Impact/Effort、Creately、Coda RICE 评分板）
Plotminder 做个人 + 团队场景通吃，包含执行路径不只做分类，LLM 自动打分。

**vs 调度工具**（Motion、Sunsama、Reclaim AI）
不正面竞争——它们做时间块、Plotminder 做决策。但有用户重叠：从 Motion 流失的"失去 agency"用户、从 Sunsama 流失的"仪式感太重"用户，都是 Plotminder 的潜在 convert。

**vs 思维画布**（Heptabase、Obsidian Canvas、tldraw、Scrintal）
Plotminder 用语义化坐标轴替代纯空间布局，包含任务执行循环。不做 backlink、不做知识图谱（不抢 PKM 赛道）。

---

## 九、不做清单

为了保持产品聚焦，v1.0 明确不做：

- 时间块 / 日历调度（让 Motion 做）
- AI 自动修改用户数据（永远只给建议）
- 复杂团队协作（v2 再说）
- 笔记 / 文档（task 自带 note 字段够用）
- 知识图谱 / backlink（让 Obsidian 做）
- 时间追踪 / 番茄统计（集成 Toggl 即可）
- 项目甘特图

---

## 十、技术栈

- **核心**：Go 后端 + SQLite 本地存储 + Flutter 跨平台前端（macOS / Windows / Linux / iOS / Android）
- **架构**：local-first，预留 `SyncProvider` interface 支持未来云同步
- **LLM**：provider-agnostic（Anthropic / OpenAI / 本地 Ollama），统一 prompt 接口调用
- **数据格式**：SQLite + JSON 双写，用户文件夹内可见，方便备份 / 迁移 / Git 版本控制

---

## 十一、发布路径

8 个开发 phase 分三个对外里程碑：

| 里程碑 | 包含 phase | 关键能力 |
|--------|-----------|---------|
| **Alpha（内部）** | P0 Foundation + P1 MapView | 数据模型 + 2D 拖拽地图 |
| **Closed beta** | P2 Smart axes + P3 Path view | 自定义维度 + LLM 打分 + 路径推荐 |
| **v1.0 public** | P4 Imports + P5 Live mode | 双向同步 + 番茄追踪 + 重规划建议 |
| **Future** | P6 3D + P7 Cloud sync | 第三轴 + 多设备 + 团队协作 |

---

## 十二、定价（暂定）

- **Free**：1 个 workspace、3 个 view、所有核心功能、本地数据完全可用
- **Plus ($8 / 月)**：无限 workspace + view、LLM 自动打分、跨设备同步
- **Pro ($16 / 月)**：高级 Reflect、所有集成（Apple / Google / Microsoft / Todoist）、AI insight

**定价原则**：核心体验完全免费，让用户能用一两个月真正爱上它；付费换的是"更多 + 联网"，不是核心功能锁。

---

## 十三、一句话 pitch（不同场景版本）

| 场景 | 一句话 |
|------|-------|
| 30 秒电梯 | "Plotminder 把你脑子里的事画在一张你自定义的二维地图上，然后告诉你下一步走哪。" |
| 朋友圈分享 | "终于找到一个不替我自动安排时间的生产力工具——它只帮我看见全局、推荐下一步。" |
| 投资人 pitch | "Eisenhower 矩阵的连续版本 + 多维自定义 + AI 路径推荐。跨过日程工具的红海，开辟决策画布的新赛道。" |
| 朋友求推 | "你不是缺一个 todo app，你是缺一张能看见全部事情的地图。" |

---

*文档版本 v0.1 · 持续迭代中*

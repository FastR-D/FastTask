# FastTask 对话能力扩展：多会话 / 思考过程 / 图片附件

> 文档状态：🟢 已实现（多会话 / 思考过程 / 图片附件三项均已落地）
> 更新：2026-09-23
> 上位决策：[ADR-0002](adr/0002-assistant-transport.md)（协议未变）、[ADR-0005](adr/0005-libfx-agent-harness.md)（网关链路）
> 相关：[`agent-impl.md`](agent-impl.md)（服务端状态结构）、[`frontend.md`](frontend.md)（运行时接线）、[`harness.md`](harness.md) §4（模型网关）

## 1. 文档定位

三项能力 **assistant-ui 均有原生支持**，本文档的价值不在"要不要自己造"，而在三处：

1. **哪一项的原生支持真的接在 assistant-transport 这条路径上**——`onRespondToToolApproval` 的教训（[ADR-0002](adr/0002-assistant-transport.md) §3.1）说明"类型里有"不等于"这条路径接了"。下面每一项都核对过 `node_modules` 里的实际类型。
2. **服务端要补什么**——三项都需要后端配合，多会话还需要新表。
3. **模型网关（`harness.md` §4）要注意什么**。

核对基准：`@assistant-ui/react` 0.15.21、`@assistant-ui/core` 0.3.20、`assistant-stream` 0.3.44（`web/node_modules` 实测）。

### 1.0 核对结论速查

| 能力 | assistant-transport 路径原生支持？ | 结论 |
|---|---|---|
| 图片附件 | ✅ `AssistantTransportOptions.adapters.attachments?: AttachmentAdapter` **在选项里** | 直接用 |
| 思考过程 | ✅ `ReasoningMessagePart` + `useMessagePartReasoning`，由我们的 converter 产出 | 直接用 |
| 多会话 | ⚠️ **`AssistantTransportOptions` 里没有 threadList** | 需用 `useRemoteThreadListRuntime` **包一层**，见 §2.1 |

---

## 2. 多会话持久化

### 2.1 接法：包一层，不是加一个选项

`AssistantTransportOptions` 的完整 `adapters` 只有两项：

```ts
adapters?: {
  attachments?: AttachmentAdapter | undefined;
  history?: ThreadHistoryAdapter | undefined;
};
```

**没有 threadList。** 多会话在 assistant-ui 里是**另一层运行时**：

```ts
const runtime = useRemoteThreadListRuntime({
  runtimeHook: () => useAssistantTransportRuntime({ /* 现有配置，见 frontend.md §4 */ }),
  adapter: fastTaskThreadListAdapter,
  threadId,                       // 受控
  onThreadIdChange: setThreadId,  // 同步到路由
})
```

**关键语义：`runtimeHook` 是按线程实例化的**（`RemoteThreadListHookInstanceManager`）。
每个线程有自己的 `useAssistantTransportRuntime` 实例，因此：

- `api` / `resumeApi` / `resumeStateApi` **必须是线程作用域的**，不能是当前那个全局路径。
- 现有 `web/src/agent/runtime.ts` 要把端点改成按 `threadId` 拼接，
  或通过 `body` 把 `threadId` 带上——**两者取其一，不要两条都做**，否则服务端有两个 threadId 来源。
- 推荐前者（路径携带），因为它让服务端的鉴权与路由检查落在同一处。

> **注意**：`useRemoteThreadListRuntime` 在 `@assistant-ui/react` 里的实现路径是
> `legacy-runtime/runtime-cores/remote-thread-list/`。它是从 `@assistant-ui/core/react` 重导出的当前公开 API，
> 但 `legacy-runtime` 这个路径名值得留意——**升级 assistant-ui 时把这条接线列为重点回归项**。

#### 2.1.1 🟢 实现取舍：没有用 `useRemoteThreadListRuntime`

上面的接法是本文档的推荐，**实现走了另一条路**，理由是这条路径与本项目已有的两处约束冲突：

1. `runtimeHook` 按线程实例化意味着每个线程各有一个 transport runtime，而本项目的一次 run 可能
   **交给浏览器宿主驱动**（[`harness.md`](harness.md) §1.2）：驱动逻辑挂在 `onResponse` 上，
   由 `web/src/agent/runtime.ts` 这一个文件收敛（ADR-0002 §4）。按线程实例化会把「谁在驱动这个 run」
   变成每线程一份状态，取消、心跳、审批轮询都要跟着分身。
2. `legacy-runtime` 路径下的线程列表语义（本地临时 id、乐观合并）与 §2.5 的删除语义、
   §2.4 的 checkpoint 恢复都要再对一遍，收益只是少写一个列表组件。

实际实现：

- **线程目录自己维护**：`web/src/agent/threads.ts` 直接调 §2.2 的 Go 端点（列表分页、创建、重命名、
  归档、删除、标题生成），`web/src/agent/index.tsx` 渲染列表并持有当前线程。
- **切换线程 = 换 key 重挂 transport runtime**，挂载前 `GET /agent/threads/{id}/state` 取服务端状态作为
  `initialState`。同一时刻只有一个 runtime 实例，因此「谁在驱动」始终唯一。
- **`threadId` 走请求体**（`prepareAgentCommand` 从服务端状态里取），端点保持非线程作用域。
  本文档 §2.1 说「两者取其一」，实现取的是后者（body）。原因：assistant-ui 在首条命令前会先赋一个
  `__LOCALID_...` 的临时 remoteId，路径携带会让第一条消息打到不存在的线程上；body 携带则由
  `prepareAgentCommand` 把临时 id 归一为 `null`，服务端据此建线程并回填真实 id。
  **服务端只有一个 threadId 来源**这条要求仍然成立：路径里没有 threadId。

### 2.2 `RemoteThreadListAdapter` → Go 端点

适配器的完整契约（实测类型）：

```ts
type RemoteThreadListAdapter = {
  list(params?): Promise<RemoteThreadListResponse>;
  rename(remoteId, newTitle): Promise<void>;
  updateCustom?(remoteId, custom): Promise<void>;
  archive(remoteId): Promise<void>;
  unarchive(remoteId): Promise<void>;
  delete(remoteId): Promise<void>;
  initialize(threadId): Promise<RemoteThreadInitializeResponse>;   // { remoteId, externalId? }
  generateTitle(remoteId, unstable_messages): Promise<AssistantStream>;
  fetch(threadId): Promise<RemoteThreadMetadata>;
  unstable_Provider?; unstable_useAdapters?;
}
```

映射：

| 适配器方法 | Go 端点 | 备注 |
|---|---|---|
| `list` | `GET /api/v1/agent/threads?status=&cursor=` | 按 `user_id` 过滤，游标分页 |
| `fetch` | `GET /api/v1/agent/threads/{id}` | 跨用户返回 **404**，不是 403（`arch.md` §12） |
| `initialize` | `POST /api/v1/agent/threads` | 返回 `{ remoteId }`；`remoteId` 即服务端 thread id |
| `rename` | `PATCH /api/v1/agent/threads/{id}` | `{ title }` |
| `updateCustom` | 同上 | `{ custom }`，见 §2.3 的用途 |
| `archive` / `unarchive` | `POST .../{id}/archive` / `/unarchive` | 软状态切换 |
| `delete` | `DELETE /api/v1/agent/threads/{id}` | 见 §2.5 的删除语义 |
| `generateTitle` | `POST .../{id}/title` | 见 §2.4 |

`RemoteThreadMetadata` 的字段直接对应表列：
`status: "regular" | "archived"`、`remoteId`、`externalId?`、`title?`、`lastMessageAt?`、`custom?`。

### 2.3 数据模型（新增 migration `000006_agent_threads`）

```text
agent_threads
  id              TEXT PK
  user_id         TEXT NOT NULL            -- 所有查询强制过滤
  goal_id         TEXT NULL                -- 可选绑定，复用 agent.md §11 的既有判断
  title           TEXT NULL
  status          TEXT NOT NULL            -- 'regular' | 'archived'
  custom          TEXT NULL                -- JSON，见下
  checkpoint      BLOB NULL                -- harness.md §6，libfx 对话历史
  libfx_version   TEXT NULL                -- checkpoint 的版本标记
  last_message_at DATETIME NULL
  created_at / updated_at
  INDEX (user_id, status, last_message_at DESC)
```

现有 `AgentRun` / `AgentMessage` / `AgentMessagePart` 增加 `thread_id` 外键并回填。

**回填策略（migration 内一次性执行，不可留 NULL）：**

```text
1. 为每个有 agent 消息的 user_id，按现有 Conversation 分组：
     每个 Conversation → 建一个 agent_threads 行
       id            = 新生成
       user_id       = Conversation.user_id
       goal_id       = Conversation.goal_id（现有字段，models.go:229）
       title         = Conversation.title，为空则取首条用户消息前 30 字
       status        = 'regular'
       checkpoint    = NULL          ← 迁移前的历史没有 libfx checkpoint
       last_message_at = 该会话最后一条消息时间
2. 不属于任何 Conversation 的孤儿 agent 消息 → 每个 user_id 建一个
     title = '历史对话' 的兜底线程，全部挂上去
3. 回填完成后把 thread_id 改为 NOT NULL
```

两条必须注意：

- **`checkpoint` 为 NULL 的老线程，第一次对话时走 §6.3 的降级路径**
  （[`harness.md`](harness.md) §6.3：从权威消息重建摘要）。这是预期行为，不是 bug，
  但要在 UI 上给一次性提示，说明历史上下文已压缩。
- 迁移**必须可重入**：`000006` 失败重跑不得产生重复线程。用
  `WHERE NOT EXISTS` 或先建唯一索引 `(user_id, source_conversation_id)` 保证。
  为此 `agent_threads` 需要多一列 `source_conversation_id TEXT NULL UNIQUE`，迁移后不再写入。

`custom` 只放**前端展示用的非权威数据**（例如置顶标记、颜色）。
**不得**把任何参与授权或不变量判断的字段放进去——它由客户端写入，属不可信输入。

### 2.4 与 checkpoint 的关系

`checkpoint` 存在 `agent_threads` 上，不是 `agent_runs`——它是**线程级**的对话历史
（`harness.md` §6）。切换线程即切换 checkpoint，这正是多会话与 libfx 契合的地方。

`generateTitle` 返回 `Promise<AssistantStream>`。两种合规实现：

- **推荐**：服务端用确定性规则生成（取首条用户消息前 N 字，去换行、截断），
  端点返回一个只含该文本的 `AssistantStream`。**不消耗模型额度，不引入新的模型调用路径。**
- 备选：走一次独立的模型调用。若选此路，它**必须经过 `harness.md` §4 的同一条网关链路**，
  不得在前端或 sidecar 里另开一条出口——否则 ADR-0005 §3.3「运行时零 Vercel 访问」失去单点保证。

### 2.5 删除语义

`delete` 必须**同时**删除该线程的 `AgentRun` / `AgentMessage` / `AgentMessagePart` /
`agent_run_chunks` / `checkpoint` / **附件**（§4.5）。

- 在一个事务里做。
- 线程存在运行中的 run 时，先取消再删除，不允许留下孤儿 run。
- 归档（`archive`）不删数据，只改 `status`。**UI 上要让两者的区别对用户可见**，
  不要把 delete 做成看起来可撤销的样子。

---

## 3. 思考过程（reasoning）

### 3.1 完整链路

```text
qwen3.8-max
  └─ OpenAI 兼容流里的 reasoning_content 增量
       └─ Go 模型代理                                   [harness.md §4.4]
            ├─ 解析 delta.reasoning_content
            │    └─ AgentMessagePart(kind='reasoning')
            │         └─ update-state → UI
            │              └─ converter 产出 ReasoningMessagePart
            │                   └─ assistant-ui 原生渲染
            └─ 原样 SSE 转发给宿主
                 └─ @ai-sdk/openai-compatible           [宿主进程内，现成，不用写]
                      └─ reasoning-start / -delta / -end → libfx
```

**全链路唯一要我们写的是 Go 侧的 chunk 翻译和 converter 的一个分支。**

**2026-09-23 已端到端实测确认**（[`harness.md`](harness.md) §16）：浏览器生产产物中
`reasoning_content` 正确映射为 `reasoning-start` / `reasoning-delta` / `reasoning-end`。

### 3.2 档位控制

`LanguageModelV4CallOptions` 有：

```ts
reasoning?: 'provider-default' | 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh';
```

这一项由 **libfx 宿主**放进请求体，经 shim 翻成 provider 侧的等价参数。
Go 代理**不覆盖**它（与 `system` / `tools` 不同——档位不影响任何不变量），但要：

- 在 `POST /agent/runs` 的响应里下发一个服务端默认档位，宿主照此设置；
- 校验取值在枚举内，非法值回落到 `provider-default`。

### 3.3 服务端状态结构

`agent-impl.md` §2.7 的 part 类型新增 `reasoning`：

```jsonc
{ "type": "reasoning", "id": "<V4 part id>", "text": "..." }
```

`id` 沿用 V4 分片里的 `id`，因为同一条消息里可能有**多段**推理（`reasoning-start` 可出现多次），
必须按 id 聚合，不能拼成一坨。

### 3.4 三条必须遵守的规则

1. **思考内容同样是模型输出，同样不可信**（`agent.md` §4 不变量 4）。
   它**只用于展示**，绝不参与任何判断、解析或提取结构化数据。
   不要写"从 reasoning 里解析出模型打算调用什么工具"这类代码。
2. **默认折叠。** 用 assistant-ui 的 `ReasoningGroupComponent` / `ChainOfThoughtPrimitive`，
   默认收起，用户点开才展开。移动端尤其如此。
3. **思考内容要持久化，但要可关闭。** 默认持久化（否则刷新后历史消息的思考链消失，体验割裂）；
   同时提供管理员开关，关闭后 Go 代理**丢弃 reasoning 分片不落库**——
   有些部署不希望思考链留在数据库里。

   🟢 **实现形态**：开关是**部署级配置** `FASTTASK_AGENT_REASONING_PERSIST`（默认 `true`），
   经 uber-fx 注入 `AgentService`，代理在 `ReasoningDelta` 里直接丢弃分片——**既不落库也不进 chunk 流**，
   所以关闭后前端连实时思考过程都看不到（这正是「不希望思考链留下」的部署想要的）。
   没有做成数据库里的管理员 UI 项：它是一个「数据是否落地」的合规决定，属于部署方而不是使用者，
   放进 UI 反而会让一次误点改变既有数据的留存策略。

---

## 4. 图片附件

### 4.1 原生支持已确认在本路径上

`AssistantTransportOptions.adapters.attachments?: AttachmentAdapter` —— **在选项里，直接可用**。
包已导出 `SimpleImageAttachmentAdapter`、`SimpleTextAttachmentAdapter`、`CompositeAttachmentAdapter`，
消息侧有 `ImageMessagePart` / `FileMessagePart`。

### 4.2 不能直接用 `SimpleImageAttachmentAdapter`

它把图片转成 data URI 塞进消息。对 FastTask 不合适：

- 图片会进入 `AgentMessagePart`、`agent_run_chunks`、checkpoint，SQLite 会迅速膨胀；
- 每一轮模型调用都要重传整张图的 base64；
- `harness.md` §3.1 硬规则 2「宿主不持久化任何东西」要求附件有服务端归属。

**改为自定义 adapter，上传到我们后端，消息里只带引用：**

```ts
// web/src/agent/attachments.ts
const fastTaskImageAdapter: AttachmentAdapter = {
  accept: 'image/png,image/jpeg,image/webp,image/gif',
  async add({ file }) { /* 返回 pending attachment，立即显示缩略图 */ },
  async send(attachment) { /* POST 到 §4.3，返回带 attachment_id 的 CompleteAttachment */ },
  async remove(attachment) { /* DELETE，或仅从 composer 移除未发送的 */ },
}
```

用 `CompositeAttachmentAdapter` 组合，未来加 PDF / 文本时不用改接线。

### 4.3 上传端点

```
POST   /api/v1/agent/attachments        multipart/form-data
       → { attachment_id, mime, width, height, bytes }
DELETE /api/v1/agent/attachments/{id}
GET    /api/v1/agent/attachments/{id}   ← 仅所有者可读，用于 UI 回显
```

存储：沿用现有的本地存储约定，**不引入对象存储**。
记录表 `agent_attachments(id, user_id, thread_id NULL, mime, bytes, path, created_at)`。

### 4.4 网关侧：引用 → 真实图片

模型调用时，Go 代理把消息里的 `attachment_id` 展开成 V4 的 file part
（`@ai-sdk/openai-compatible` 会转成 OpenAI 的 `image_url`，实测已支持）。

**展开发生在 Go 侧，不在宿主侧。** 原因有二：

- 宿主（尤其在浏览器里）不应持有其它用户附件的可达路径；
- 展开时要按 `user_id` 校验归属，这必须在服务端做。

宿主发来的请求体里若直接含 base64 图片数据，**Go 代理予以丢弃**——
所有图片必须经由 `attachment_id` 引用，这样每一张都经过归属校验。

### 4.5 限制与校验（全部在 Go 侧强制）

| 项 | 默认值 | 超出时 |
|---|---|---|
| 单张大小 | 8 MB | 400，明确错误文案 |
| 单条消息张数 | 4 | 400 |
| 单线程累计 | 100 张 / 500 MB | 400，提示清理 |
| MIME 白名单 | png / jpeg / webp / gif | 400 |

另外三条：

- **必须按魔数嗅探真实类型**，不信任 `Content-Type` 与扩展名。
- **必须剥除 EXIF**（含 GPS）。科研场景常见手机拍摄的实验记录，地理位置不应入库。
- 线程删除时级联删除其附件（§2.5）；未绑定线程的孤儿附件由现有清理任务按 TTL 回收。

---

## 5. 三项共同的服务端改动汇总

| 改动 | 涉及 |
|---|---|
| migration `000006_agent_threads` + `agent_attachments` | §2.3、§4.3 |
| `AgentRun` / `AgentMessage` / `AgentMessagePart` 增加 `thread_id` 并回填 | §2.3 |
| 线程 CRUD 九个端点 | §2.2 |
| 附件上传 / 读取 / 删除三个端点 | §4.3 |
| `AgentMessagePart` 新增 `reasoning` 与 `image` 两种 part | §3.3、§4.4 |
| 模型代理增加：reasoning 分片落库、附件引用展开、base64 丢弃 | §3.1、§4.4 |
| 现有 agent 端点全部改为线程作用域 | §2.1 |
| `agent_threads.source_conversation_id`（仅迁移用） | §2.3 |

**`interface.md` 必须同步更新**，新增端点走 Huma 定义（流式端点的例外范围不扩大）。

## 6. 验收

1. 新建三个会话，分别对话后刷新页面与换设备登录，历史与 checkpoint 均正确恢复。
2. 归档/取消归档/重命名/删除四个操作与服务端状态一致；删除后附件与 chunk 无残留（查库断言）。
3. qwen3.8-max 的思考链实时流式展示，默认折叠，刷新后历史消息仍可展开。
4. 管理员关闭思考持久化后，新消息不落 reasoning part，且 UI 不显示空的折叠块。
5. 在输入框挂两张图片后连同文字发送，模型正确引用图片内容；EXIF GPS 已剥除（exiftool 断言）。
6. 跨用户访问线程与附件均返回 404。
7. **WASM 与 sidecar 两种模式下，以上全部行为一致**（[`harness.md`](harness.md) §14.3 的等价性测试需覆盖本文三项）。

## 7. 待确认

| 事项 | 默认取值 | 何时重新评估 |
|---|---|---|
| 线程标题生成 | 🟢 确定性截断，不调模型（§2.4）；另有 `POST /agent/threads/{id}/title` 显式重算 | 用户反馈标题质量差时 |
| 思考内容默认是否持久化 | 🟢 是，部署级 `FASTTASK_AGENT_REASONING_PERSIST` 可关（§3.4 规则 3） | 数据库体积成为问题时 |
| 附件是否支持 PDF | 否，仅图片 | 走 `integration/` 的 FastRead 边界，不由 agent 直接解析 |
| 线程数量上限 | 不设限，只分页 | 出现性能问题时 |

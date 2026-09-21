# ADR-0004：mdui 与 assistant-ui 的样式与职责边界

> 状态：已接受  
> 提出日期：2026-09-22  
> 定案日期：2026-09-22（mdui 完全重写落地后，依据提交 `9456e5f` 的真实代码）  
> 实现文档：[`doc/frontend.md`](../frontend.md)

## 1. 背景

本 ADR 曾被有意挂起：在 mdui 重写落地前，任何样式方案都是对尚不存在的代码做猜测。现在工作流 A 已完成（提交 `9456e5f restruct with mdui`），可以依据事实定案。

## 2. 决策

采用**方案 A：只用 mdui 设计令牌，手写 assistant-ui primitives 的样式**。不引入 Tailwind。

这不是在三个候选里权衡的结果——**mdui 重写已经把这套约定建立起来并执行得相当彻底**，本 ADR 只是把既成事实写进规则，让工作流 B 遵守同一套约定。

## 3. 依据（代码事实）

| 事实 | 证据 |
|---|---|
| **没有引入 Tailwind 或任何原子化 CSS** | `web/package.json` 的依赖只有 `mdui` 与 `@fontsource/material-icons` |
| **零硬编码色值** | `styles.css`、`admin.css`、`lens.css` 中全部颜色形如 `rgb(var(--mdui-color-*))`，扫描无字面色值 |
| **排版与形状同样走令牌** | `var(--mdui-typescale-*)`、`var(--mdui-shape-corner-*)` |
| **主题是 `mdui-theme-auto`** | `index.html` 的 `<html class="mdui-theme-auto">`，跟随 `prefers-color-scheme`。无 `setColorScheme` 调用、无自定义主题层、无手动切换 |
| **自定义事件已有既定解法** | `src/mdui-react.ts` 提供 `useMduiEvent(ref, name, handler)`（ref + `addEventListener`）与 `fieldValue(event)` |
| **组件注册收敛在单一入口** | `src/mdui.ts` 逐个 import 组件，且**只**被 `main.tsx` 引入，使 jsdom 测试中自定义元素保持惰性 |
| **JSX 类型来自 mdui 官方** | `src/vite-env.d.ts` 的 `/// <reference types="mdui/jsx.en.d.ts" />` |

暗色模式因此对工作流 B 是零成本的：只要不硬编码颜色，对话区自动跟随系统主题。这一条本身就足以否决方案 B——引入 Tailwind 会让对话区需要**第二套**暗色模式实现，并与 mdui 的令牌手工对齐。

## 4. 对工作流 B 的约束

1. **不得引入 Tailwind 或任何原子化 CSS 框架。**
2. **颜色一律 `rgb(var(--mdui-color-*))`，字号一律 `var(--mdui-typescale-*)`，圆角一律 `var(--mdui-shape-corner-*)`。** 提交中不允许出现字面色值。
3. **新样式写进 `web/src/agent.css`**，由 `web/src/agent/index.tsx` 引入，与 `lens.css` / `admin.css` 的既有惯例一致。
4. **需要新的 mdui 组件时，加进 `src/mdui.ts`**——那是唯一注册点，不得在别处 import 组件。
5. **非标准名称的自定义事件用 `useMduiEvent`，取输入值用 `fieldValue`。** 不要另造一套适配。
6. **assistant-ui 的 primitives 保持无样式**，不引入其官方 shadcn 组件包。

## 5. 被否决的方案

| 方案 | 否决理由 |
|---|---|
| B. 对话区引入 Tailwind + assistant-ui 官方组件 | 会在项目里并存两套样式体系与两套暗色模式实现，而现有代码已经证明纯令牌方案可行且干净 |
| C. 在 primitives 之上封装 FastTask 组件层 | 多一层抽象但换不来什么——mdui 组件已经可以直接用在对话区（`Dialogue` 现在就在用 `mdui-list`、`mdui-text-field`、`mdui-button-icon`），不需要再包一层 |

## 6. 定案时发现的、需要在实现中处理的问题

- **`.chat-shell` 的固定高度不适合 assistant-ui。** 当前是 `height:calc(100dvh - 20rem)`（`styles.css:233`，移动端 `styles.css:250`），这个魔数依赖页头的确切高度。assistant-ui 的 Thread viewport 需要一个由 flex/grid 约束、自行管理滚动的容器。接入时应改为 `minmax(0,1fr)` 之类的弹性约束，而不是继续加魔数。
- **测试中 mdui 元素未定义但仍在 DOM 中。** 既有测试用 `closest('mdui-chip')`、`closest('mdui-button')` 断言并能通过，说明这条路可行。但**不要依赖 mdui 组件的交互行为做断言**——`src/mdui.ts` 不会在测试里加载，组件没有行为，只有属性。

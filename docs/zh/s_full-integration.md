---
title: "s_full · 端到端集成"
chapter: full
slug: s_full-integration
est_read_min: 30
---

# s_full · 端到端集成

> 把 s01..s11 串成一个真实的 nanobot 调用轨迹。这一节没有新代码——这是"读 11 节最后能拼出来的图"。

---

## 全栈架构图

```
                  ┌──────────────────────────────────────────────┐
                  │  CLI / Channel (Telegram, Slack, …)          │
                  │     ─ user types message ─                   │
                  └────────────────┬─────────────────────────────┘
                                   │  InboundMessage{SessionKey, UserText}
                                   ▼
              ┌────────────────────────────────────────────────┐
              │   Bus  (s05)  per-session goroutine + chan      │
              │   - serializes within-session                  │
              │   - parallel across sessions                   │
              └────────────────┬───────────────────────────────┘
                               │
                               ▼
                       ┌──────────────────────────────┐
                       │  Loop.Process  (s11)         │
                       │  outer state machine         │
                       │                              │
                       │  RESTORE → COMPACT → COMMAND │
                       │     ↓                        │
                       │  BUILD → RUN → SAVE → RESPOND│
                       │     ↓                        │
                       │  DONE                        │
                       └────┬───┬───┬───┬───┬─────────┘
                            │   │   │   │   │
            ┌───────────────┘   │   │   │   └──────────────┐
            ▼                   ▼   ▼   ▼                  ▼
   SessionManager(s05)  Consolidator(s10) Runner(s04+s09)  MemoryStore(s06)
            │                   │       │       │                  │
            │            (LLM-summarize)│       │            (file I/O)
            │                           │       │
            │                  ┌────────▼───────▼────────┐
            │                  │   Provider (s03)        │
            │                  │ ─ AnthropicProvider ─    │
            │                  │   /v1/messages          │
            │                  └─────────┬───────────────┘
            │                            │
            │                            ▼
            │                   Tool execution
            │                   via Registry (s02)
            │                            │
            │              ┌─────────────┴────────────┐
            │              ▼                          ▼
            │         BashTool(s01-...)          (future: MCP)
            │
            └─►  ContextBuilder (s08) reads:
                   ├ Memory (s06): MEMORY/SOUL/USER.md + history.jsonl
                   └ Skills (s07): every SKILL.md it can find

                  Hook (s09) taps every iteration in Runner.
                  AutoCompact (s10) wakes idle sessions in the background.
```

每个方块右上角的 `(sNN)` 注明它是哪一节构建的。可以从 s01 顺序读到 s11，每读一节，这张图多亮一个方块。

## 16 步执行轨迹

跟一个真实请求 `"搜一下 AI 最新进展，总结一下"` 走完整一遍：

| # | 步骤 | 涉及章节 | 关键代码点 |
|---|---|---|---|
| 1 | 用户输入 → CLI 把 stdin 包装成 `InboundMessage` | (CLI) | `main.go` |
| 2 | `Bus.Send` 把消息推进 session 的 channel | s05 | `bus.go::Bus.Send` |
| 3 | session goroutine 接到 msg，调 `Loop.Process` | s05 + s11 | `bus.go::sessionLoop` |
| 4 | **RESTORE** 状态：从 `SessionManager.GetOrCreate(key)` 拿到 Session，加载已存在的 messages | s05 + s06 | `state_restore.go` |
| 5 | **COMPACT** 状态：检查 token 预算；如果超了，调 `Consolidator.Consolidate(s)` 压缩老消息 | s10 | `state_compact.go` |
| 6 | **COMMAND** 状态：检查是否 `/help` / `/clear` 等斜杠命令；不是则透传 | s11 | `state_command.go` |
| 7 | **BUILD** 状态：`ContextBuilder.BuildSystemPrompt(rt)` 拼起 AGENTS.md + SOUL.md + USER.md + MEMORY.md + skills + `[Runtime Context]` | s06 + s07 + s08 | `state_build.go` |
| 8 | **RUN** 状态进 `Runner.Run`：第 0 轮调 `Provider.Chat(req)` —— Anthropic 解码 prompt cache 命中（因为 Registry 排序稳定，s02 教过） | s02 + s03 + s04 | `state_run.go` → `runner.go` → `provider_anthropic.go` |
| 9 | LLM 返 `LLMResponse{ToolCalls: [{name: "web_search", input: {q: "AI"}}], FinishReason: "tool_calls"}` | s03 | `runner.go` |
| 10 | Runner 调 Hook 的 `BeforeExecuteTools`（捕获 telemetry），然后 `Registry.Execute("web_search", {q})` | s09 + s02 | `hook.go::SDKCaptureHook` |
| 11 | 工具实际执行（DuckDuckGo HTTP / Tavily / 任何 Tool 实现），返回 string | s01 / extension | `bash_tool.go`-style |
| 12 | Runner 把 tool_result 装进 user 消息，再调 `Provider.Chat`——第 1 轮，LLM 看着工具结果合成最终回答 | s04 | `runner.go` |
| 13 | LLM 返 `LLMResponse{Content: "AI 最新进展是...", FinishReason: "stop"}`；Runner 收尾，调 Hook.`FinalizeContent` | s09 | `hook.go` |
| 14 | **SAVE** 状态：`MemoryStore.AppendHistory` 把 user/assistant 两条 message 写到 `history.jsonl`；Session.UpdatedAt 更新 | s06 | `state_save.go` |
| 15 | **RESPOND** 状态：把 `tc.FinalContent` 经 `OutboundMessage` 推回 Bus；CLI 打印到 stdout | s05 + s11 | `state_respond.go` |
| 16 | （后台）AutoCompact 的 ticker 每 5 分钟扫一次空闲 session；这次 session 还活跃所以跳过；20 分钟后下一次扫描会触发 `Consolidator.consolidate` 压缩历史 | s10 | `autocompact.go::Run` |

每一步都有"它在哪节学过"。这张表是把 11 节的 diff 反过来读：你已经构建了所有这些原语。

## 故意省略的特性

| 上游特性 | 为什么省略 | 在哪能找到 |
|---|---|---|
| MCP 工具包装 | 上游 wraps 一个 Go MCP SDK；教这个先要教 SDK | App. B 延伸练习 |
| OpenAI-compat router | Phase G 多模型 addendum 加 | `docs/zh/multi-model.md` |
| Channels（Telegram/Slack/Discord/…）| 1 channel = 1 transport adapter；我们用 CLI 一个 | App. B 延伸练习 |
| Heartbeat / cron / serve | 运维层关注，非 agent 核心 | App. B 延伸练习 |
| Pydantic 配置校验 | Go 没等价；用 plain struct + `Validate() error` | s11 sidebar |
| litellm 移除 refactor | Python 生态特定 | App. A footnote |
| OpenAI Responses API helpers | provider-specific | Phase G addendum |
| Streaming 响应 | 教学简化（hook.OnStream 已留 seam）| App. B 延伸练习 |
| Dream 第二阶段记忆压缩 | Consolidator 是第一阶段；Dream 是更高阶的 LLM 编辑 | App. A |

## 一段话总结

learn-nanobot 不是 nanobot 的"轻量版"——它是 nanobot 的**心智模型版**。读完 s01..s11，你脑子里会有一张 nanobot 的拓扑图：哪一节负责什么、谁依赖谁、为什么是这种切法。然后你打开 nanobot 的源码，每个 import 行都有一个对应的 learn-nanobot 章节。

下一步：读两个附录加深心智模型。

- **附录 A** 讲 nanobot 最有意思的设计——记忆是诠释，不是转录。
- **附录 B** 是上游源码导读地图，告诉你怎么从 mini 顺着指针读到 production。

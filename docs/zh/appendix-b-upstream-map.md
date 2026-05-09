---
title: "附录 B · 上游源码导读地图"
chapter: B
slug: appendix-b-upstream-map
est_read_min: 20
---

# 附录 B · 上游源码导读地图

> learn-nanobot 11 节读完后，下一步是真的去读 [HKUDS/nanobot](https://github.com/HKUDS/nanobot)。这一节是地图。

---

## 推荐阅读顺序

按下面的顺序读，每一步都是从 mini 到 production 的等价物——你已经知道它在做什么，只需要看上游怎么把它做得更稳。

```
1. nanobot/__main__.py                                  ← CLI 入口
2. nanobot/nanobot.py                                   ← SDK facade
3. nanobot/agent/loop.py                                ← s11 的对照（最长，~600 LOC）
   ├ 入门：__init__, _state_run, process
   └ 跳过：streaming, image-gen, ask_user, subagent

4. nanobot/agent/runner.py                              ← s04 的对照
   ├ 入门：AgentRunSpec, AgentRunResult, run
   └ 跳过：retries with heartbeat, length recovery, injection

5. nanobot/providers/base.py                            ← s03 的对照
   ├ 入门：LLMProvider ABC, LLMResponse, ToolCallRequest
   └ 跳过：retry helpers (_TRANSIENT_ERROR_MARKERS 等)

6. nanobot/providers/anthropic_provider.py              ← s03 的对照
   ├ 入门：_build_kwargs, _parse_response
   └ 跳过：streaming, thinking blocks, generation_settings

7. nanobot/agent/tools/registry.py                      ← s02 的对照（很短）
8. nanobot/agent/tools/base.py                          ← s01+s02 的 Tool ABC

9. nanobot/session/manager.py                           ← s05 的对照
10. nanobot/bus/queue.py 或 nanobot/bus/events.py        ← s05 的 Bus 对照

11. nanobot/agent/memory.py:1-400                       ← s06 的对照
12. nanobot/agent/skills.py                             ← s07 的对照（中等长度）
13. nanobot/agent/context.py                            ← s08 的对照
14. nanobot/agent/hook.py                               ← s09 的对照（短）

15. nanobot/agent/memory.py:800-1200                    ← s10 的对照（Consolidator）
16. nanobot/agent/autocompact.py                        ← s10 的对照（短）
```

读完 1-16，你已经把 nanobot 的核心 ~3000 LOC 看完了。剩下的都是 channel adapter / API server / config 这些"运维"部分。

## 文件 → 章节 完整映射

| 上游文件 | 章节 | 备注 |
|---|---|---|
| `nanobot/agent/loop.py:1-120` | s01 | 仅入口骨架 |
| `nanobot/agent/loop.py`（全文）| s11 | 课程的最大对照 |
| `nanobot/agent/tools/registry.py` | s02 | |
| `nanobot/agent/tools/base.py` | s01, s02 | |
| `nanobot/providers/base.py` | s03 | |
| `nanobot/providers/anthropic_provider.py` | s03 | |
| `nanobot/providers/openai_compat_provider.py` | Phase G | 不在编号章节里 |
| `nanobot/agent/runner.py` | s04 | |
| `nanobot/session/manager.py` | s05 | |
| `nanobot/bus/queue.py` 或 `events.py` | s05 | |
| `nanobot/agent/memory.py:1-400` | s06 | 文件 I/O 层 |
| `nanobot/agent/memory.py:800-1200` | s10 | Consolidator |
| `nanobot/agent/skills.py` | s07 | |
| `nanobot/agent/context.py` | s08 | |
| `nanobot/agent/hook.py` | s09 | |
| `nanobot/agent/autocompact.py` | s10 | |
| `nanobot/agent/tools/mcp.py` | App. B 练习 | 可选 |
| `nanobot/api/`, `nanobot/channels/`, `nanobot/web/` | App. B 练习 | 传输层 |
| `nanobot/cli/` | App. B 练习 | onboard / agent 命令 |
| `nanobot/cron/`, `nanobot/heartbeat/` | App. B 练习 | 运维 |
| `nanobot/agent/memory.py` 的 Dream 部分 | App. A 练习 | 二阶段记忆压缩 |

## 5 个延伸练习

### 1. 加一个 OpenAI-compatible provider

实现 `Provider` interface 给任何 OpenAI 兼容端点（OpenRouter / DeepSeek / Ollama）。

提示：s03 的 `LLMResponse` 已经规范化了 finish_reason。你需要的就是一个把 OpenAI 的 tool_calls JSON 翻成 `[]ToolCall` 的函数。**Phase G 的 `multi-model` 文档已经做了这步**——读它再自己实现一遍就好。

### 2. 接一个真实 MCP 服务器

写一个用 stdio JSON-RPC 与 MCP server 通信的 Go 客户端，把它包成 `Tool`，让它能塞进 s02 的 `Registry`。

参考 [`nanobot/agent/tools/mcp.py`](https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/tools/mcp.py)。Go 侧可以参考 [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)（如果存在）或自己写 JSON-RPC 客户端。

### 3. 实现 Dream

App. A 描述了 Dream 的目的：周期性读 `history.jsonl`，让 LLM 调 `edit_file` 工具改写三个 markdown 文件。

实现路径：
- 一个 `Dream struct` 持有 `Provider` + `MemoryStore` + `Skills`
- `(d *Dream) Cycle(ctx)` 方法：读 history → 调 `Provider.Chat` 让 LLM 列出 insights → 再调一次让它产出改写计划 → 调用 `MemoryStore.WriteMemory` 落盘
- 加个 cron-style scheduler 让它每天凌晨跑一次

参考上游 `nanobot/agent/memory.py` 里的 Dream class。

### 4. 加 Telegram channel

把 `python-telegram-bot` 的 Go 等价物（如 `tucnak/telebot`）包成 goroutine：从 Telegram 收消息 → 推到 s05 的 `Bus` → 发回去。

```
[telegram poller]  ─►  Bus.Send(InboundMessage{...})
                  ◄─    bus.Out() ─►  bot.Send(text)
```

只需要一个新文件 `agents/sNN-channel-telegram/telegram.go`，不需要改其它。

### 5. Streaming 响应

s09 的 `Hook.OnStream` 是 seam。改造 s03 的 `AnthropicProvider` 用 streaming endpoint，每个 token chunk 就调 `hook.OnStream(delta)`。然后写一个 `StreamingPrintHook` 把 delta 直接打到 stdout。

参考 nanobot 的 streaming 实现：`nanobot/providers/anthropic_provider.py` 里的 `stream` 方法 + `_LoopHook.on_stream`。

## 最后

如果你做完了 11 节 + 这 5 个延伸练习，恭喜——你已经独立从零建了一个 nanobot-equivalent。读上游源码不会再有"魔法"感觉，因为每一行你都知道它对应你写过的哪段。

这是 learn-nanobot 想达到的目标。

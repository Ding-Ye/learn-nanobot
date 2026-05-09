# learn-nanobot

> 用 Go 从零渐进构建一个 [nanobot](https://github.com/HKUDS/nanobot)，11 节加一个端到端集成 + 两份附录。每节读 30 分钟、敲一次代码、对照一段上游 Python 源码。
>
> English: [README.en.md](./README.en.md)

## 这是什么 / What is this

[HKUDS/nanobot](https://github.com/HKUDS/nanobot) 是一个 ~5k 行 Python 写的"超轻量个人 AI agent"——10+ 个 LLM provider、Telegram/Discord/Slack 等多通道、文件系统记忆、MCP 集成、技能（Markdown）。代码很容易读，但**怎么从零长出来的**没人讲。

`learn-nanobot` 是这个 gap 的解药。每一节用 Go 实现一个机制（agent loop、tool registry、provider 抽象、session、memory、…），每一节末尾对照真实的 nanobot 上游源码。看完 11 节，你能从 main loop 一行一行读到 production。

## 为什么用 Go 学一份 Python 项目 / Why Go for a Python upstream

强迫翻译。把 `asyncio.Lock` 翻成 `chan + goroutine`，把 Pydantic 翻成 `Validate() error`，把动态 attribute 翻成显式 struct。每一次"翻译"都是对上游的一次深读——比直接读 Python 多看见一层结构。

教学法借鉴 [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)：心智模型先于代码 / 30-60 行核心 / 与上一节的 diff / 上游源码对照。

## 课程 / Curriculum

| # | 章节 | 教什么 | 状态 |
|---|---|---|---|
| s01 | [最小 agent loop](./docs/zh/s01-minimum-loop.md) | provider + 1 工具 + turn cap | ✅ |
| s02 | [工具注册表](./docs/zh/s02-tool-registry.md) | `Registry` 缓存 + 排序 + register/unregister | ✅ |
| s03 | [Provider 抽象层](./docs/zh/s03-provider-abstraction.md) | `LLMResponse` 规范化 + retry 分类 | ✅ |
| s04 | [Agent runner](./docs/zh/s04-agent-runner.md) | 内部工具循环切出来 | ✅ |
| s05 | [Session 与 MessageBus](./docs/zh/s05-session-bus.md) | 每会话一个 goroutine | ✅ |
| s06 | [MemoryStore](./docs/zh/s06-memory-store.md) | append-only jsonl + cursor + MEMORY/SOUL/USER.md | ✅ |
| s07 | [技能加载器](./docs/zh/s07-skills-loader.md) | YAML frontmatter + requires 检查 | ✅ |
| s08 | [上下文构建器](./docs/zh/s08-context-builder.md) | Bootstrap 文件 + 历史裁剪 + RuntimeMeta | ✅ |
| s09 | [Hook 系统](./docs/zh/s09-hooks.md) | 5 个生命周期点 + CompositeHook | ✅ |
| s10 | [Consolidator + AutoCompact](./docs/zh/s10-consolidator-autocompact.md) | LLM 摘要 + TTL 后台压缩 | ✅ |
| s11 | TurnState 状态机 | RESTORE→COMPACT→COMMAND→BUILD→RUN→SAVE→RESPOND→DONE | ⏳ |
| s_full | 端到端集成 | 16 步执行轨迹 + 故意省略对照 | ⏳ |
| A | 附录 · 记忆是诠释，不是转录 | append-only → Dream → curated MEMORY.md | ⏳ |
| B | 附录 · 上游源码导读地图 | 文件→章节映射 + 5 个延伸练习 | ⏳ |

完整规划见 [`.learn/plan.md`](./.learn/plan.md)。

## 跑起来 / Run

```bash
# 跑 s01
cd agents/s01-minimum-loop
export ANTHROPIC_API_KEY=sk-ant-...
go run . "list the .go files in this directory"

# 看文档（双语）
cd web && npm install && npm run dev
# 然后打开 http://localhost:3000
```

需要 Go ≥ 1.23 和 Node ≥ 20。

## 仓库结构 / Layout

```
learn-nanobot/
├── agents/s01-minimum-loop/    # 一个 Go module 一节，互不 import
├── docs/{zh,en}/               # 双语章节，CI 强制 heading 数对齐
├── upstream-readings/          # 注解过的真实上游片段
├── web/                        # Next.js 文档站
├── .github/workflows/          # go.yml / docs.yml / web.yml
└── .learn/                     # 生成器留下的 dossier + plan
```

## 致谢 / Credits

- 上游：[HKUDS/nanobot](https://github.com/HKUDS/nanobot)（MIT License）——本仓库一切对照都来自其源码。
- 教学法启发：[shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) —— 课程结构、双语对照、上游源码穿插这一套都来自这里。

## License

MIT。详见 [`LICENSE`](./LICENSE)。

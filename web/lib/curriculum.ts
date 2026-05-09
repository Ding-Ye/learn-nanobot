// The locked curriculum from .learn/plan.md. SessionNav and the landing page both
// read from this single source of truth. Slugs match docs/{zh,en}/<slug>.md.
//
// "available: false" means the chapter exists in the curriculum but its
// docs aren't written yet — the link will render but the entry shows as
// "未发布 / not yet" in the sidebar.

export type ChapterMeta = {
  slug: string;
  num: string; // "s01", "s02", "s_full", "A", "B", "M"
  title: { zh: string; en: string };
  available: boolean;
};

export const CURRICULUM: ChapterMeta[] = [
  // M (multi-model) — added in Phase G when has_llm_call_layer = true.
  // Created with available:false at bootstrap; flipped to true in Phase G.
  {
    slug: "multi-model",
    num: "M",
    title: {
      zh: "多模型接入指南（OpenAI / DeepSeek / Qwen / 自托管 …）",
      en: "Multi-model guide (OpenAI / DeepSeek / Qwen / self-hosted …)",
    },
    available: false,
  },
  {
    slug: "s01-minimum-loop",
    num: "s01",
    title: { zh: "最小 agent loop", en: "Minimum agent loop" },
    available: true,
  },
  {
    slug: "s02-tool-registry",
    num: "s02",
    title: { zh: "工具注册表", en: "Tool registry" },
    available: true,
  },
  {
    slug: "s03-provider-abstraction",
    num: "s03",
    title: { zh: "Provider 抽象层", en: "Provider abstraction" },
    available: true,
  },
  {
    slug: "s04-agent-runner",
    num: "s04",
    title: {
      zh: "Agent runner（内部工具循环）",
      en: "Agent runner (inner tool-loop)",
    },
    available: false,
  },
  {
    slug: "s05-session-bus",
    num: "s05",
    title: { zh: "Session 与 MessageBus", en: "Session + MessageBus" },
    available: false,
  },
  {
    slug: "s06-memory-store",
    num: "s06",
    title: {
      zh: "MemoryStore（文件读写）",
      en: "Memory store (file I/O)",
    },
    available: false,
  },
  {
    slug: "s07-skills-loader",
    num: "s07",
    title: { zh: "技能加载器", en: "Skills loader" },
    available: false,
  },
  {
    slug: "s08-context-builder",
    num: "s08",
    title: { zh: "上下文构建器", en: "Context builder" },
    available: false,
  },
  {
    slug: "s09-hooks",
    num: "s09",
    title: { zh: "生命周期 Hook 系统", en: "Hook system" },
    available: false,
  },
  {
    slug: "s10-consolidator-autocompact",
    num: "s10",
    title: {
      zh: "Consolidator 与 AutoCompact",
      en: "Consolidator + AutoCompact",
    },
    available: false,
  },
  {
    slug: "s11-turn-state-machine",
    num: "s11",
    title: {
      zh: "TurnState 状态机（完整 AgentLoop）",
      en: "TurnState state machine (full AgentLoop)",
    },
    available: false,
  },
  {
    slug: "s_full-integration",
    num: "s_full",
    title: { zh: "端到端集成", en: "End-to-end integration" },
    available: false,
  },
  {
    slug: "appendix-a-memory-as-interpretation",
    num: "A",
    title: {
      zh: "附录 A · 记忆是诠释，不是转录",
      en: "Appendix A · Memory as interpretation",
    },
    available: false,
  },
  {
    slug: "appendix-b-upstream-map",
    num: "B",
    title: {
      zh: "附录 B · 上游源码导读地图",
      en: "Appendix B · Upstream source-reading map",
    },
    available: false,
  },
];

export type Locale = "zh" | "en";

export function chapterTitle(c: ChapterMeta, locale: Locale): string {
  return c.title[locale];
}

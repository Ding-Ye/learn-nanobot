# web/

Next.js doc viewer for learn-nanobot. Static site that reads `../docs/{zh,en}/*.md` at build time and renders the curriculum side-by-side in two languages.

## 跑起来 / Run

```bash
cd web
npm install
npm run dev    # http://localhost:3000
```

需要 Node ≥ 20。/ Node ≥ 20 required.

## 编辑 / Authoring

- 文档：`../docs/zh/<slug>.md` 与 `../docs/en/<slug>.md`，front matter 保持一致。
- 课程清单：`web/lib/curriculum.ts`——把章节 `available: false` 改为 `true` 才会出现在 sidebar 链接里。
- 上游片段：`../upstream-readings/<file>.py`，由 `web/components/UpstreamReader.tsx` 读取。
- 章节 → 上游片段映射：`web/app/[locale]/s/[slug]/page.tsx` 里的 `guessUpstreamFile()`。

## 构建 / Build

```bash
npm run build
npm start
```

## 技术栈 / Stack

Next.js 15 + React 19 + Tailwind v4 + remark/rehype + Shiki。教学法借鉴 [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)。

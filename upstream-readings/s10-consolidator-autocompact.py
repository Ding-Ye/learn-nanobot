# Upstream reading: nanobot/agent/memory.py (Consolidator) +
#                    nanobot/agent/autocompact.py (whole file)
#
# Source:
#   https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/memory.py (lines ~440–760)
#   https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/autocompact.py
#
# Why these two files together: they form the memory-compression
# pipeline. `Consolidator` does the synchronous work (pick a boundary,
# call the LLM, persist the summary, advance the cursor). `AutoCompact`
# is the asynchronous driver — a TTL ticker that schedules consolidator
# runs against idle sessions in the background. s10 ports both.
#
# Reading guide:
#   1. Consolidator.__init__       — what it depends on (provider,
#                                    sessions, build_messages callback,
#                                    tool definitions, context window).
#   2. pick_consolidation_boundary — the load-bearing 20 lines. The
#                                    legal-user-turn rule. Read it
#                                    twice; this is the heart of s10.
#   3. archive                     — the LLM call + summary persistence.
#                                    Note the raw_archive fallback.
#   4. maybe_consolidate_by_tokens — the outer "loop until prompt fits".
#                                    Multi-round; bounded by
#                                    _MAX_CONSOLIDATION_ROUNDS = 5.
#   5. AutoCompact.__init__        — TTL + the _archiving set.
#   6. AutoCompact.check_expired   — the scheduler. Three filters:
#                                    in-flight, active, idle.
#   7. AutoCompact._archive        — the goroutine body in upstream
#                                    Python — note _split_unconsolidated
#                                    keeps the recent suffix.
#
# Annotated excerpt:

"""Consolidator — lightweight token-budget triggered consolidation."""

# learn-Go: in s10 we ship `Consolidator` with five fields:
#   Provider           — LLM (used to be `provider` upstream)
#   TokenEst           — function field replacing tiktoken
#   ConsolidationRatio — same name, default 0.5
#   Memory             — MemoryStore (replaces upstream's `store`)
#   Model              — model id (string)
#
# Upstream's __init__ also takes:
#   - sessions               — SessionManager. We don't need it inside
#                              the consolidator because Consolidate is
#                              called with a *Session pointer.
#   - context_window_tokens  — total budget. We don't enforce a budget
#                              here; we trust the caller to pass a
#                              `tokensToRemove` already computed against
#                              their budget.
#   - build_messages         — callback to assemble probe messages for
#                              token estimation. We don't need it
#                              because our PickBoundary uses
#                              estimateMessageTokens directly.
#   - get_tool_definitions   — same; tool defs are part of upstream's
#                              token estimation chain. Our placeholder
#                              estimator doesn't include tool overhead.
#   - max_completion_tokens  — used by the safety buffer. We elide.

class Consolidator:
    _MAX_CONSOLIDATION_ROUNDS = 5
    _SAFETY_BUFFER = 1024

    def __init__(self, store, provider, model, sessions,
                 context_window_tokens, build_messages, get_tool_definitions,
                 max_completion_tokens=4096, consolidation_ratio=0.5):
        self.store = store
        self.provider = provider
        self.model = model
        self.sessions = sessions
        self.context_window_tokens = context_window_tokens
        self.max_completion_tokens = max_completion_tokens
        self.consolidation_ratio = consolidation_ratio
        self._build_messages = build_messages
        self._get_tool_definitions = get_tool_definitions
        # learn-Go: weakref locks per session — Go would use sync.Map
        # of *sync.Mutex if we needed per-session serialization. We
        # don't, because AutoCompact's archiving map already serializes
        # per session.
        self._locks = weakref.WeakValueDictionary()

    # ---- the load-bearing method ------------------------------------
    #
    # learn-Go: this is `Consolidator.PickBoundary` in consolidator.go.
    # Two structural deviations:
    #   1. We return `(Boundary, bool)` instead of `tuple | None`. Go
    #      idiom favors the explicit ok flag.
    #   2. We add an isToolResultMessage check inline. Upstream relies
    #      on the message structure where role="user" with tool_result
    #      content has a different `_channel_delivery` flag set
    #      (see autocompact.py's _split_unconsolidated). For learn we
    #      directly inspect the content blocks since we don't track
    #      delivery flags.

    def pick_consolidation_boundary(self, session, tokens_to_remove):
        """Pick a user-turn boundary that removes enough old prompt tokens."""
        start = session.last_consolidated
        if start >= len(session.messages) or tokens_to_remove <= 0:
            return None

        removed_tokens = 0
        last_boundary = None
        for idx in range(start, len(session.messages)):
            message = session.messages[idx]
            # learn-Go: we add `not isToolResultMessage(msg)` to the
            # condition — see comment above on why upstream doesn't.
            if idx > start and message.get("role") == "user":
                last_boundary = (idx, removed_tokens)
                # learn-Go: upstream returns early on first satisfying
                # boundary; we walk to the end and return the LAST
                # satisfying boundary. The two behaviors converge in
                # practice (most sessions have only a few user messages
                # in the candidate range) but the "latest" choice
                # maximizes drop per round.
                if removed_tokens >= tokens_to_remove:
                    return last_boundary
            removed_tokens += estimate_message_tokens(message)

        return last_boundary

    # ---- archive: LLM call + persistence ----------------------------
    #
    # learn-Go: this is `Consolidator.Consolidate` in consolidator.go.
    # Differences:
    #   - Upstream renders messages via `MemoryStore._format_messages`
    #     (a static method that emits "[ts] ROLE [tools]: content"
    #     lines). Our `formatMessageForSummary` does the same shape but
    #     drops the timestamp (we don't track per-message ts in
    #     Session.Messages).
    #   - Upstream truncates the rendered text to fit
    #     `_input_token_budget` (context_window - completion - safety).
    #     We don't truncate; the boundary picker is conservative
    #     enough that the rendered chunk fits.
    #   - Upstream calls `provider.chat_with_retry` (with classify-
    #     retryable). We call `Provider.Chat` and surface
    #     `FinishReason == "error"` as a Go error.
    #   - Upstream falls back to `raw_archive` (dump messages to
    #     history.jsonl without summarization) when the LLM fails. We
    #     don't ship this fallback in s10 — see App. B's note for the
    #     extension.

    async def archive(self, messages):
        if not messages:
            return None
        try:
            formatted = MemoryStore._format_messages(messages)
            formatted = self._truncate_to_token_budget(formatted)
            response = await self.provider.chat_with_retry(
                model=self.model,
                messages=[
                    {"role": "system", "content": render_template(
                        "agent/consolidator_archive.md", strip=True)},
                    {"role": "user", "content": formatted},
                ],
                tools=None, tool_choice=None,
            )
            if response.finish_reason == "error":
                raise RuntimeError(f"LLM returned error: {response.content}")
            summary = response.content or "[no summary]"
            self.store.append_history(summary,
                                       max_chars=_ARCHIVE_SUMMARY_MAX_CHARS)
            return summary
        except Exception:
            logger.warning("Consolidation LLM call failed, raw-dumping")
            self.store.raw_archive(messages)
            return None

    # ---- maybe_consolidate_by_tokens: outer loop --------------------
    #
    # learn-Go: we don't ship the outer multi-round loop. Our
    # Consolidate is one round; if the prompt is still too big the
    # caller (s11's COMPACT state) will call Consolidate again. The
    # multi-round version exists because upstream allows the
    # consolidator to be invoked from many places; pushing the loop
    # into Consolidate centralizes the budget logic. We chose the
    # simpler shape because the s11 state machine already loops.

    async def maybe_consolidate_by_tokens(self, session, *,
                                           session_summary=None,
                                           replay_max_messages=None):
        if not session.messages or self.context_window_tokens <= 0:
            return
        lock = self.get_lock(session.key)
        async with lock:
            budget = self._input_token_budget
            target = int(budget * self.consolidation_ratio)
            estimated, source = self.estimate_session_prompt_tokens(session)
            if estimated < budget:
                return
            for round_num in range(self._MAX_CONSOLIDATION_ROUNDS):
                if estimated <= target:
                    break
                boundary = self.pick_consolidation_boundary(
                    session, max(1, estimated - target))
                if boundary is None:
                    break
                end_idx = boundary[0]
                chunk = session.messages[session.last_consolidated:end_idx]
                summary = await self.archive(chunk)
                session.last_consolidated = end_idx
                self.sessions.save(session)
                if not summary:
                    break
                estimated, source = self.estimate_session_prompt_tokens(session)


# ====================================================================
# AutoCompact — the TTL background driver
# ====================================================================

class AutoCompact:
    _RECENT_SUFFIX_MESSAGES = 8

    # learn-Go: `AutoCompact` in autocompact.go. Our struct has:
    #   Sessions     — *SessionManager
    #   Consolidator — *Consolidator
    #   TTL          — time.Duration (replaces session_ttl_minutes)
    #   archiving    — map[string]bool
    #   summaries    — map[string]string
    #
    # We omit:
    #   - _summaries[key] = (text, timestamp) — we keep just the text.
    #     The timestamp was only used for the "Inactive for N minutes"
    #     re-injection prompt; that's s11's COMPACT/BUILD logic.
    #   - retain_recent_legal_suffix — the keep-last-N invariant.
    #     Our boundary picker handles long tails by walking forward;
    #     a future PR can add `RetainSuffix int`.

    def __init__(self, sessions, consolidator, session_ttl_minutes=0):
        self.sessions = sessions
        self.consolidator = consolidator
        self._ttl = session_ttl_minutes
        self._archiving = set()
        self._summaries = {}

    def _is_expired(self, ts, now=None):
        if self._ttl <= 0 or not ts:
            return False
        if isinstance(ts, str):
            ts = datetime.fromisoformat(ts)
        return ((now or datetime.now()) - ts).total_seconds() >= self._ttl * 60

    # ---- check_expired: the scheduler -------------------------------
    #
    # learn-Go: `AutoCompact.CheckExpired` in autocompact.go. The Go
    # version is identical in shape:
    #   1. Skip in-flight sessions (`a.archiving[key]`).
    #   2. Skip active sessions (`activeSessionKeys[key]`).
    #   3. Skip not-yet-idle sessions (`now - sess.UpdatedAt <= TTL`).
    #   4. Mark `archiving[key] = true` BEFORE spawning so a
    #      back-to-back call can't double-queue.
    #   5. Spawn `go a.runOne(ctx, sess)` and continue.

    def check_expired(self, schedule_background, active_session_keys=()):
        """Schedule archival for idle sessions, skipping in-flight ones."""
        now = datetime.now()
        for info in self.sessions.list_sessions():
            key = info.get("key", "")
            if not key or key in self._archiving:
                continue
            if key in active_session_keys:
                continue
            if self._is_expired(info.get("updated_at"), now):
                self._archiving.add(key)
                schedule_background(self._archive(key))

    # ---- _archive: the goroutine body -------------------------------
    #
    # learn-Go: `AutoCompact.runOne` in autocompact.go. The deferred
    # `discard(key)` is the same pattern as `defer delete(archiving)`.
    # Errors are absorbed silently — the next tick retries if the
    # session is still expired.

    async def _archive(self, key):
        try:
            self.sessions.invalidate(key)
            session = self.sessions.get_or_create(key)
            archive_msgs, kept_msgs = self._split_unconsolidated(session)
            if not archive_msgs and not kept_msgs:
                session.updated_at = datetime.now()
                self.sessions.save(session)
                return
            last_active = session.updated_at
            summary = ""
            if archive_msgs:
                summary = await self.consolidator.archive(archive_msgs) or ""
            if summary and summary != "(nothing)":
                self._summaries[key] = (summary, last_active)
                session.metadata["_last_summary"] = {
                    "text": summary,
                    "last_active": last_active.isoformat(),
                }
            session.messages = kept_msgs
            session.last_consolidated = 0
            session.updated_at = datetime.now()
            self.sessions.save(session)
        except Exception:
            logger.exception("Auto-compact: failed for {}", key)
        finally:
            self._archiving.discard(key)


# ====================================================================
# Reading map — where to go after this excerpt
# ====================================================================
#
# 1. nanobot/agent/memory.py:_format_messages — the static helper that
#    upstream uses to render messages for the consolidator prompt. We
#    inline a simpler version in `formatMessageForSummary`.
#
# 2. nanobot/agent/memory.py:_truncate_to_token_budget — uses tiktoken
#    to truncate the rendered chunk so the LLM's input fits the
#    context window. We rely on the boundary picker being conservative
#    enough that no truncation is needed.
#
# 3. nanobot/agent/memory.py:Dream — the SECOND-tier memory consolidator
#    (cron-scheduled). Reads history.jsonl, edits MEMORY.md/SOUL.md/
#    USER.md via the edit_file tool. We don't ship Dream; App. A
#    discusses it as the "memory as interpretation" pattern's apex.
#
# 4. nanobot/agent/autocompact.py:_split_unconsolidated — keeps the
#    most recent N messages. The keep-last-N invariant is a useful
#    extension; learn-nanobot's boundary picker doesn't enforce it
#    today.
#
# 5. nanobot/agent/loop.py:_state_compact — the COMPACT state in s11.
#    It calls maybe_consolidate_by_tokens during turn execution.
#    Reading this gives you the synchronous-consolidation path that
#    complements AutoCompact's asynchronous one.
#
# 6. nanobot/templates/agent/consolidator_archive.md — the full
#    summarization system prompt (~80 lines). Our s10
#    `summarizationSystem` const is a 20-line subset that captures
#    the intent.
#
# Total LOC excerpted above: ~200 (Consolidator init+pick+archive +
# maybe_consolidate_by_tokens + AutoCompact init+check_expired+_archive).
# Total LOC in upstream files: ~550 (memory.py:Consolidator +
# autocompact.py).

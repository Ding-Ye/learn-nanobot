# Upstream reading: nanobot/agent/loop.py (the canonical reading for s11)
#
# Source:
#   https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/loop.py
#
# This file is THE chapter-defining read of learn-nanobot. It's the
# outer state machine that ties together every other file — what the
# research dossier calls "the conductor". For s11 we port the core
# state-machine vocabulary (`TurnState` enum + `TurnContext` dataclass +
# `_TRANSITIONS` table + the seven `_state_X` methods).
#
# Reading guide (in order):
#
#   1. TurnState (enum)      — the seven states + DONE. Mirrors our
#                              learn-Go TurnState in turnstate.go.
#   2. TurnContext (@dataclass) — the mutable bag. Note how every state
#                              method takes `ctx: TurnContext` and
#                              mutates it in place rather than returning
#                              a new context. We mirror the pattern in
#                              learn-Go (turnstate.go::TurnContext).
#   3. _TRANSITIONS (dict)   — the static graph. Keyed by (state, event)
#                              tuple in upstream; we collapse to a
#                              single key in learn-Go because our handlers
#                              can write tc.State directly when they want
#                              to short-circuit (e.g. /help → RESPOND).
#   4. _state_restore        — get-or-create session, extract documents,
#                              load checkpoint. Maps to learn-Go
#                              state_restore.go (simpler — no checkpoint).
#   5. _state_compact        — auto-compact prep + per-turn budget check.
#                              Maps to learn-Go state_compact.go.
#   6. _state_command        — slash-command dispatch. Maps to learn-Go
#                              state_command.go (we ship just /help and
#                              /clear; upstream has /dream, /onboard,
#                              /status, ...).
#   7. _state_build          — assemble messages via ContextBuilder.
#                              Maps to learn-Go state_build.go.
#   8. _state_run            — call AgentRunner.run. Maps to learn-Go
#                              state_run.go (one-line wrapper).
#   9. _state_save           — persist messages + media + cursor.
#                              Maps to learn-Go state_save.go.
#  10. _state_respond        — assemble OutboundMessage. Maps to learn-Go
#                              state_respond.go (just sets FinalContent;
#                              the Bus driver in main.go handles the
#                              channel-side dispatch).
#
# Annotated excerpt below.


"""The TurnState enum — outer loop's vocabulary."""

class TurnState(Enum):
    RESTORE = auto()
    COMPACT = auto()
    COMMAND = auto()
    BUILD = auto()
    RUN = auto()
    SAVE = auto()
    RESPOND = auto()
    DONE = auto()

# learn-Go: turnstate.go defines a typed `TurnState int` plus a const
# block with iota. We add `StateError` (upstream uses Python exceptions
# for the error case; Go's idiomatic alternative is an explicit terminal
# state plus an `Err` field on the bag).


"""The TurnContext dataclass — mutable bag flowing through every state."""

@dataclass
class TurnContext:
    msg: InboundMessage
    session_key: str
    state: TurnState
    turn_id: str
    session: Session | None = None

    history: list[dict[str, Any]] = field(default_factory=list)
    initial_messages: list[dict[str, Any]] = field(default_factory=list)

    final_content: str | None = None
    tools_used: list[str] = field(default_factory=list)
    all_messages: list[dict[str, Any]] = field(default_factory=list)
    stop_reason: str = ""
    had_injections: bool = False

    user_persisted_early: bool = False
    save_skip: int = 0

    outbound: OutboundMessage | None = None
    generated_media: list[str] = field(default_factory=list)

    on_progress: Callable[..., Awaitable[None]] | None = None
    on_stream: Callable[[str], Awaitable[None]] | None = None
    on_stream_end: Callable[..., Awaitable[None]] | None = None
    on_retry_wait: Callable[[str], Awaitable[None]] | None = None

    pending_queue: asyncio.Queue | None = None
    pending_summary: str | None = None

    trace: list[StateTraceEntry] = field(default_factory=list)

# learn-Go: turnstate.go::TurnContext. We ship a slimmer version —
# Msg, SessionKey, State, Session, History, SystemPrompt, InitialMessages,
# Result, FinalContent, ToolsUsed, Generated, Err. We omit:
#
#   - turn_id      — upstream uses for tracing across services; learn-Go
#                    runs single-process and has no analog yet.
#   - on_progress / on_stream / ... — streaming is omitted (App. B
#                    extension exercise).
#   - pending_queue — upstream's mid-turn-injection mechanism; not in
#                    learn-Go.
#   - trace        — we use a simple loopDispatchTrace function var
#                    (loop.go) for testing instead of a per-turn slice.
#
# We add SystemPrompt and Result because we split the runner's output
# into discrete tc fields rather than spreading it across stop_reason /
# all_messages / tools_used / final_content.


"""The _TRANSITIONS table — static state graph."""

_TRANSITIONS: dict[tuple[TurnState, str], TurnState] = {
    (TurnState.RESTORE, "ok"): TurnState.COMPACT,
    (TurnState.COMPACT, "ok"): TurnState.COMMAND,
    (TurnState.COMMAND, "dispatch"): TurnState.BUILD,
    (TurnState.COMMAND, "shortcut"): TurnState.DONE,
    (TurnState.BUILD, "ok"): TurnState.RUN,
    (TurnState.RUN, "ok"): TurnState.SAVE,
    (TurnState.SAVE, "ok"): TurnState.RESPOND,
    (TurnState.RESPOND, "ok"): TurnState.DONE,
}

# learn-Go: turnstate.go::Transitions. We collapse the 2-key tuple into a
# single key because:
#
#   1. Our handlers can write tc.State directly when they short-circuit
#      (state_command.go writes StateRespond when "/help" matches). The
#      driver respects that. Upstream's "shortcut" event accomplishes
#      the same goal via the table; both work.
#
#   2. Map[K]V with K = TurnState (one value) is simpler to walk in the
#      graph-integrity test (state_test.go::TestTransitions_AllStates...).
#      A 2-tuple key would need an alternative test that enumerates
#      events.
#
# Trade-off: our table has fewer entries but our state methods carry
# more behavior. Upstream's pattern is more declarative; ours is more
# imperative. Both are valid.


"""_state_restore — entry handler."""

async def _state_restore(self, ctx: TurnContext) -> str:
    """Restore checkpoint / pending user turn; extract documents."""
    msg = ctx.msg

    if msg.media:
        new_content, image_only = extract_documents(msg.content, msg.media)
        ctx.msg = dataclasses.replace(msg, content=new_content, media=image_only)
        msg = ctx.msg

    preview = msg.content[:80] + "..." if len(msg.content) > 80 else msg.content
    logger.info("Processing message from {}:{}: {}", msg.channel, msg.sender_id, preview)

    if ctx.session is None:
        ctx.session = self.sessions.get_or_create(ctx.session_key)
    mark_webui_session(ctx.session, msg.metadata)

    if self._restore_runtime_checkpoint(ctx.session):
        self.sessions.save(ctx.session)
    if self._restore_pending_user_turn(ctx.session):
        self.sessions.save(ctx.session)

    return "ok"

# learn-Go: state_restore.go. Simplifications:
#   - We don't extract documents (no inline media support yet).
#   - We don't restore runtime checkpoints (no crash recovery).
#   - We don't restore pending user turns (no mid-turn-resume).
# All three are extension exercises in App. B.
#
# What we keep: get-or-create the session via SessionManager. That's the
# load-bearing line; everything else upstream is operational concern.


"""_state_compact — pre-turn consolidation."""

async def _state_compact(self, ctx: TurnContext) -> str:
    ctx.session, pending = self.auto_compact.prepare_session(
        ctx.session, ctx.session_key
    )
    ctx.pending_summary = pending
    return "ok"

# learn-Go: state_compact.go.
#
# Upstream's `auto_compact.prepare_session` does TWO things:
#   1. Load any pending summary from the AutoCompact background goroutine.
#   2. Inline-consolidate if the session is over budget.
#
# We do (2) directly via Consolidator.Consolidate when l.CompactBudget
# is set. (1) doesn't apply to our learn-Go AutoCompact — its summaries
# are persisted to MemoryStore.WriteSummary, not staged for the next
# turn's prompt. Wiring (1) is part of the App. B exercise where you
# integrate AutoCompact's summary cache into the BUILD prompt.


"""_state_command — slash-command dispatch."""

async def _state_command(self, ctx: TurnContext) -> str:
    raw = ctx.msg.content.strip()
    cmd_ctx = CommandContext(
        msg=ctx.msg, session=ctx.session, key=ctx.session_key, raw=raw, loop=self
    )
    result = await self.commands.dispatch(cmd_ctx)
    if result is not None:
        ctx.outbound = result
        return "shortcut"  # → DONE per the table
    return "dispatch"      # → BUILD per the table

# learn-Go: state_command.go.
#
# Upstream's CommandRouter is a registered set of handlers. We hard-code
# /help and /clear because the curriculum doesn't ship the dynamic
# command registration the upstream router supports. The pattern is the
# same: a command match short-circuits the rest of the state machine.


"""_state_build — assemble prompt + messages."""

async def _state_build(self, ctx: TurnContext) -> str:
    await self.consolidator.maybe_consolidate_by_tokens(
        ctx.session,
        session_summary=ctx.pending_summary,
        replay_max_messages=self._max_messages,
    )
    self._set_tool_context(...)

    _hist_kwargs = {
        "max_messages": self._max_messages,
        "max_tokens": self._replay_token_budget(),
        "include_timestamps": True,
    }
    ctx.history = ctx.session.get_history(**_hist_kwargs)

    pending_ask_id = pending_ask_user_id(ctx.history)
    ctx.initial_messages = self._build_initial_messages(
        ctx.msg, ctx.session, ctx.history, pending_ask_id, ctx.pending_summary
    )
    ctx.user_persisted_early = self._persist_user_message_early(...)
    return "ok"

# learn-Go: state_build.go. We invoke ContextBuilder (s08) which already
# handles history slicing, prompt assembly, runtime metadata splice. The
# upstream version splits these across BUILD itself (the runtime context),
# ContextBuilder (the bootstrap files), and the consolidator (the summary).
# Our learn-Go version concentrates them in ContextBuilder for clarity.
#
# We also do a memory-root sanity check at the top — if the workspace dir
# is gone, we surface the error here. That's how
# TestLoop_ErrorInBuild_TransitionsToError exercises the error-transition
# path.


"""_state_run — call the inner LLM ↔ tools loop."""

async def _state_run(self, ctx: TurnContext) -> str:
    result = await self._run_agent_loop(
        ctx.initial_messages,
        on_progress=ctx.on_progress,
        on_stream=ctx.on_stream,
        # ...lots of plumbing...
        session=ctx.session,
        channel=ctx.msg.channel,
        chat_id=ctx.msg.chat_id,
        session_key=ctx.session_key,
        pending_queue=ctx.pending_queue,
    )
    final_content, tools_used, all_msgs, stop_reason, had_injections = result
    ctx.final_content = final_content
    ctx.tools_used = tools_used
    ctx.all_messages = all_msgs
    ctx.stop_reason = stop_reason
    ctx.had_injections = had_injections
    return "ok"

# learn-Go: state_run.go. One-liner:
#   result, err := l.Runner.Run(ctx, spec); tc.Result = &result
# The Runner (s04) already encapsulates everything `_run_agent_loop` does;
# we don't have to thread streaming/progress callbacks because we don't
# ship streaming yet.


"""_state_save — persist the turn."""

async def _state_save(self, ctx: TurnContext) -> str:
    if ctx.final_content is None or not ctx.final_content.strip():
        ctx.final_content = EMPTY_FINAL_RESPONSE_MESSAGE

    ctx.save_skip = 1 + len(ctx.history) + (1 if ctx.user_persisted_early else 0)
    skip_msgs = ctx.all_messages[ctx.save_skip:]
    ctx.generated_media = generated_image_paths_from_messages(skip_msgs)
    last_msg = ctx.all_messages[-1] if ctx.all_messages else None
    if ctx.generated_media and last_msg and last_msg.get("role") == "assistant":
        # ...attach media...

    self._save_turn(ctx.session, ctx.all_messages, ctx.save_skip)
    ctx.session.enforce_file_cap(on_archive=self.context.memory.raw_archive)
    self._clear_pending_user_turn(ctx.session)
    self._clear_runtime_checkpoint(ctx.session)
    self.sessions.save(ctx.session)
    self._schedule_background(
        self.consolidator.maybe_consolidate_by_tokens(...)
    )
    return "ok"

# learn-Go: state_save.go. We:
#   1. AppendHistory user + assistant to history.jsonl (MemoryStore).
#   2. Append the user message + post-turn messages to Session.Messages.
#
# We omit:
#   - skip_msgs slicing — our Result.Messages is already exactly the
#     per-turn slice (Runner deals with this).
#   - generated_media — no media support yet.
#   - enforce_file_cap — workspace housekeeping; out of scope for the
#     state-machine teaching point.
#   - _schedule_background consolidation — AutoCompact runs separately;
#     no need to fire-and-forget here.


"""_state_respond — assemble OutboundMessage."""

async def _state_respond(self, ctx: TurnContext) -> str:
    ctx.outbound = self._assemble_outbound(
        ctx.msg,
        ctx.final_content,
        ctx.all_messages,
        ctx.stop_reason,
        ctx.had_injections,
        ctx.generated_media,
        ctx.on_stream,
    )
    return "ok"

# learn-Go: state_respond.go. We just set tc.FinalContent — the Bus
# driver in main.go does the channel-side dispatch via OutboundMessage.
# Upstream's `_assemble_outbound` shapes the outbound for streaming /
# media / multipart; we pass the raw string and let the channel adapt.


# === Reading map ===
#
# How to read upstream loop.py end-to-end (1746 LOC, longest file in
# the repo):
#
#   1. Lines 1-77    — imports + module-level constants. Skim.
#   2. Lines 79-185  — _LoopHook (the in-house hook for the main loop).
#                      Skim — it's a CompositeHook fed to the Runner.
#   3. Lines 187-238 — TurnState + StateTraceEntry + TurnContext. **READ
#                      CAREFULLY** — these are exactly what s11 ports.
#   4. Lines 240-450 — AgentLoop.__init__ + tool registry plumbing.
#                      Skim — we cover the same ground in main.go's
#                      registry construction.
#   5. Lines 450-1370 — provider-snapshot loading, hook plumbing,
#                       run_agent_loop (the inner loop's direct entry
#                       point), helpers for the various states.
#                       READ run_agent_loop carefully — it's the s04
#                       Runner.Run analog. The rest is plumbing.
#   6. Lines 1372-1512 — the seven `_state_X` methods + the
#                        `_TRANSITIONS` table on line 258. **THIS IS
#                        s11**. Read top-to-bottom.
#   7. Lines 1513-1700 — message persistence helpers, sanitize functions,
#                        outbound assembly. Useful as deep dive.
#   8. Lines 1700-1746 — `process_direct` entry, `process_message`
#                        entry, lock acquisition. Useful for understanding
#                        how the bus drives the state machine.
#
# Reading order to absorb ONLY s11's teaching points:
#   loop.py:187 (TurnState)
#     -> loop.py:208 (TurnContext)
#       -> loop.py:258 (_TRANSITIONS)
#         -> loop.py:1372..1512 (the seven state methods)
#           -> learn-Go turnstate.go + loop.go + state_*.go (in that order)
#
# That's about 200 LOC of upstream read in total to grasp s11.

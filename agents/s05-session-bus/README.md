## s05 · session-bus

> 把 s04 的 Runner 包到 SessionManager + Bus 里：每个 session 一个 goroutine + buffered chan，同 session 内的 turn 串行、跨 session 并行。Go 版的 `asyncio.Lock`。
> Wrap s04's Runner in a SessionManager + Bus. Each session gets its own goroutine + buffered chan; turns within one session are serial, across sessions parallel. The Go translation of `asyncio.Lock`.

### Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -session demo "list .go files in this directory"
```

### Files / 文件

| File | What's new vs s04 |
|---|---|
| `provider*.go`, `tools.go`, `bash_tool.go`, `registry.go`, `runner.go` | unchanged — carried over verbatim because each session is its own Go module |
| `session.go` | **new** — `Session` struct + `SessionManager` (`GetOrCreate`/`Get`/`Append`/`GetHistory`) |
| `bus.go` | **new** — `Bus` with per-session goroutines, `InboundMessage` / `OutboundMessage`, `Send` / `Out` / `Stop` |
| `main.go` | now wires `SessionManager` + `Bus` and dispatches one InboundMessage |
| `session_test.go` / `bus_test.go` | covers idempotency, append order, history slicing, parallel sessions, serial within session, drain on stop |

### Test / 测试

```bash
go test -count=1 -race ./...
```

### Teaching points / 教学要点

1. **Per-session goroutine = `asyncio.Lock` per session.** Upstream Python uses `async with self._locks[key]` to serialize turns inside a session; we get the same property structurally — only one goroutine reads from each session's inbound chan.
2. **`Bus.in` is a `map[key]chan` guarded by a mutex.** The map is mutated on first Send for a new session; the mutex protects map writes. The chan itself is goroutine-safe by Go's spec.
3. **Drain on Stop.** Closing the inbound chan tells the session loop to exit *after* draining queued messages — pending Sends still get a reply.
4. **`Runner` did not change.** The Runner is unchanged from s04. The Bus just calls `runner.Run(ctx, spec)` from inside each session's goroutine. This is a composition layer, not a refactor.

### Next / 下一节

s06 makes the SessionManager persistent: it gains a `MemoryStore` field that writes the message log to `history.jsonl` and curated facts to `MEMORY.md` / `SOUL.md` / `USER.md`. The Bus and Runner shapes don't change.

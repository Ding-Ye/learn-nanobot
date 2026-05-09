package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMemoryStore_AppendHistory_AppendsLine — happy path: append one entry,
// then verify the file has exactly one '\n'-terminated JSON line and the
// returned cursor is 1.
func TestMemoryStore_AppendHistory_AppendsLine(t *testing.T) {
	root := t.TempDir()
	m, err := NewMemoryStore(root)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}

	cursor, err := m.AppendHistory(HistoryEntry{
		Role:    "user",
		Content: "hello",
	})
	if err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	if cursor != 1 {
		t.Errorf("cursor = %d, want 1", cursor)
	}

	historyPath := filepath.Join(root, "history.jsonl")
	b, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}

	lines := bytes_split(b, '\n')
	// The file ends with a newline, so split produces a trailing "".
	nonEmpty := 0
	for _, l := range lines {
		if len(l) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty != 1 {
		t.Fatalf("history.jsonl line count = %d, want 1 (raw=%q)", nonEmpty, string(b))
	}

	var got HistoryEntry
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if got.Role != "user" || got.Content != "hello" {
		t.Errorf("entry = %+v, want role=user content=hello", got)
	}
	if got.TS.IsZero() {
		t.Errorf("TS was zero, want auto-filled by AppendHistory")
	}

	// .cursor reflects the appended cursor.
	c, err := m.ReadCursor()
	if err != nil {
		t.Fatalf("ReadCursor: %v", err)
	}
	if c != 1 {
		t.Errorf("ReadCursor = %d, want 1", c)
	}
}

// TestMemoryStore_ReadMemory_DefaultsWhenMissing — reading a Markdown file
// that doesn't exist must return "" with no error (so the ContextBuilder
// can call this on a fresh agent without conditional handling).
func TestMemoryStore_ReadMemory_DefaultsWhenMissing(t *testing.T) {
	m, err := NewMemoryStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	for _, name := range []string{"MEMORY.md", "SOUL.md", "USER.md", "AGENTS.md", "TOOLS.md"} {
		got, err := m.ReadMemory(name)
		if err != nil {
			t.Errorf("ReadMemory(%q) returned error: %v (wanted nil)", name, err)
		}
		if got != "" {
			t.Errorf("ReadMemory(%q) = %q, want \"\"", name, got)
		}
	}
}

// TestMemoryStore_WriteMemory_AtomicReplace — write twice; the second write
// fully replaces the first. The intermediate state should never be visible
// (we can't observe atomicity directly in a unit test, but we can at least
// verify no leftover tmp files and the contents are exactly the second write).
func TestMemoryStore_WriteMemory_AtomicReplace(t *testing.T) {
	root := t.TempDir()
	m, err := NewMemoryStore(root)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if err := m.WriteMemory("MEMORY.md", "first version\n"); err != nil {
		t.Fatalf("first WriteMemory: %v", err)
	}
	if err := m.WriteMemory("MEMORY.md", "second version, longer body\n"); err != nil {
		t.Fatalf("second WriteMemory: %v", err)
	}

	got, err := m.ReadMemory("MEMORY.md")
	if err != nil {
		t.Fatalf("ReadMemory: %v", err)
	}
	if got != "second version, longer body\n" {
		t.Errorf("body = %q, want %q", got, "second version, longer body\n")
	}

	// No leftover tmp files in the directory.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

// TestMemoryStore_Cursor_RoundTrip — write 5, read 5. The simplest possible
// invariant.
func TestMemoryStore_Cursor_RoundTrip(t *testing.T) {
	m, err := NewMemoryStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	if err := m.WriteCursor(5); err != nil {
		t.Fatalf("WriteCursor: %v", err)
	}
	got, err := m.ReadCursor()
	if err != nil {
		t.Fatalf("ReadCursor: %v", err)
	}
	if got != 5 {
		t.Errorf("ReadCursor = %d, want 5", got)
	}

	// Round-trip a second value (overwrite).
	if err := m.WriteCursor(42); err != nil {
		t.Fatalf("second WriteCursor: %v", err)
	}
	got, err = m.ReadCursor()
	if err != nil {
		t.Fatalf("second ReadCursor: %v", err)
	}
	if got != 42 {
		t.Errorf("second ReadCursor = %d, want 42", got)
	}
}

// TestMemoryStore_AppendHistory_Concurrent_ConsistentLines — 10 goroutines
// each call AppendHistory once. Final line count must be 10 and every line
// must be parseable JSONL. This protects against a subtle bug where two
// concurrent O_APPEND writes interleave their bytes and produce a mangled
// line.
//
// Run with `go test -race` to catch any data races on the appendMu mutex.
func TestMemoryStore_AppendHistory_Concurrent_ConsistentLines(t *testing.T) {
	root := t.TempDir()
	m, err := NewMemoryStore(root)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := m.AppendHistory(HistoryEntry{
				Role:    "assistant",
				Content: strings.Repeat("x", 1024) + "/" + itoa(i),
			})
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	// Re-open the file and parse every line.
	historyPath := filepath.Join(root, "history.jsonl")
	f, err := os.Open(historyPath)
	if err != nil {
		t.Fatalf("open history.jsonl: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	got := 0
	for scanner.Scan() {
		got++
		var entry HistoryEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Errorf("line %d: not valid JSONL: %v (raw=%q)", got, err, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner.Err: %v", err)
	}

	if got != N {
		t.Errorf("line count = %d, want %d", got, N)
	}

	// Final cursor reflects the actual line count.
	c, err := m.ReadCursor()
	if err != nil {
		t.Fatalf("ReadCursor: %v", err)
	}
	if c != N {
		t.Errorf("ReadCursor = %d, want %d", c, N)
	}
}

// ---- helpers ----

// bytes_split is a tiny stdlib-free split-on-byte (mirrors strings.Split for []byte).
// We use it so the test file's imports stay minimal.
func bytes_split(b []byte, sep byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == sep {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	out = append(out, b[start:])
	return out
}

// itoa — same trivial helper used in s05's session_test.go.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

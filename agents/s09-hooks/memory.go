package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MemoryStore — same as s06; carried over verbatim because skills don't
// change persistence. The skills directory typically lives next to the
// memory files (e.g. <workspace>/skills/), but they're independently
// owned: SkillLoader is read-only over Markdown; MemoryStore is read/write
// over jsonl + Markdown.

type HistoryEntry struct {
	TS        time.Time `json:"ts"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	ToolsUsed []string  `json:"tools_used,omitempty"`
	Usage     Usage     `json:"usage,omitempty"`
}

type MemoryStore struct {
	root     string
	appendMu sync.Mutex
}

// BootstrapFiles — same constant as s06. s08's ContextBuilder will read
// these into the system prompt; s07 doesn't touch them but keeps the
// constant so future sessions can import the idea.
var BootstrapFiles = []string{"AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"}

func NewMemoryStore(root string) (*MemoryStore, error) {
	if root == "" {
		return nil, errors.New("MemoryStore: root must be non-empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("MemoryStore: mkdir %s: %w", root, err)
	}
	return &MemoryStore{root: root}, nil
}

func (m *MemoryStore) Root() string { return m.root }

func (m *MemoryStore) AppendHistory(entry HistoryEntry) (int, error) {
	m.appendMu.Lock()
	defer m.appendMu.Unlock()

	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}

	historyPath := filepath.Join(m.root, "history.jsonl")
	f, err := os.OpenFile(historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("MemoryStore.AppendHistory open: %w", err)
	}

	enc, err := json.Marshal(entry)
	if err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("MemoryStore.AppendHistory encode: %w", err)
	}
	enc = append(enc, '\n')
	if _, err := f.Write(enc); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("MemoryStore.AppendHistory write: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("MemoryStore.AppendHistory close: %w", err)
	}

	cursor, err := m.countLines(historyPath)
	if err != nil {
		return 0, fmt.Errorf("MemoryStore.AppendHistory count: %w", err)
	}
	if err := m.writeCursorAtomic(cursor); err != nil {
		return 0, fmt.Errorf("MemoryStore.AppendHistory cursor: %w", err)
	}
	return cursor, nil
}

func (m *MemoryStore) ReadCursor() (int, error) {
	cursorPath := filepath.Join(m.root, ".cursor")
	b, err := os.ReadFile(cursorPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("MemoryStore.ReadCursor: %w", err)
	}
	var c int
	if _, err := fmt.Sscanf(string(b), "%d", &c); err != nil {
		return 0, nil
	}
	return c, nil
}

func (m *MemoryStore) WriteCursor(c int) error {
	return m.writeCursorAtomic(c)
}

func (m *MemoryStore) ReadMemory(filename string) (string, error) {
	path := filepath.Join(m.root, filename)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("MemoryStore.ReadMemory %s: %w", filename, err)
	}
	return string(b), nil
}

func (m *MemoryStore) WriteMemory(filename, body string) error {
	if filename == "" {
		return errors.New("MemoryStore.WriteMemory: filename must be non-empty")
	}
	target := filepath.Join(m.root, filename)
	return atomicWrite(target, []byte(body))
}

func (m *MemoryStore) writeCursorAtomic(c int) error {
	target := filepath.Join(m.root, ".cursor")
	return atomicWrite(target, []byte(fmt.Sprintf("%d\n", c)))
}

func atomicWrite(target string, payload []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicWrite tmpfile: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicWrite write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite close: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite rename: %w", err)
	}
	return nil
}

func (m *MemoryStore) countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	count := 0
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

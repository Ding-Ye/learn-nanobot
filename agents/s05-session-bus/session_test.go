package main

import (
	"sync"
	"testing"
)

// mkUser is a small helper to keep the tests' Message construction terse.
func mkUser(text string) Message {
	return Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: text}}}
}

// TestSession_AppendMessage_OrderPreserved — adding messages one by one
// keeps insertion order in the slice and bumps UpdatedAt.
func TestSession_AppendMessage_OrderPreserved(t *testing.T) {
	sm := NewSessionManager()
	s := sm.GetOrCreate("k")
	first := s.UpdatedAt

	s.Append(mkUser("a"))
	s.Append(mkUser("b"))
	s.Append(mkUser("c"))

	if got := len(s.Messages); got != 3 {
		t.Fatalf("len(messages) = %d, want 3", got)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got := s.Messages[i].Content[0].Text; got != want {
			t.Errorf("messages[%d].text = %q, want %q", i, got, want)
		}
	}
	if !s.UpdatedAt.After(first) && !s.UpdatedAt.Equal(first) {
		// On a very fast machine UpdatedAt might equal first; we just
		// require it not to go backwards.
		t.Errorf("UpdatedAt regressed: %v -> %v", first, s.UpdatedAt)
	}
}

// TestSessionManager_GetOrCreate_Idempotent — calling GetOrCreate twice
// with the same key returns the same pointer; concurrent calls also
// converge on one *Session (no duplicate creation).
func TestSessionManager_GetOrCreate_Idempotent(t *testing.T) {
	sm := NewSessionManager()
	a := sm.GetOrCreate("alpha")
	b := sm.GetOrCreate("alpha")
	if a != b {
		t.Fatalf("GetOrCreate same key returned different pointers")
	}
	if got := sm.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}

	// Concurrent flavor: 50 goroutines all GetOrCreate the same key.
	const N = 50
	var wg sync.WaitGroup
	got := make([]*Session, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			got[i] = sm.GetOrCreate("beta")
		}()
	}
	wg.Wait()

	first := got[0]
	for i, p := range got {
		if p != first {
			t.Fatalf("goroutine %d got a different *Session for key beta: %p vs %p", i, p, first)
		}
	}
	if sm.Len() != 2 {
		t.Fatalf("Len after concurrent test = %d, want 2 (alpha + beta)", sm.Len())
	}
}

// TestSession_GetHistory_RespectsMaxMsgs — history with more messages
// than the cap returns the tail of the right size, in original order.
// Also exercises LastConsolidated as a left bound.
func TestSession_GetHistory_RespectsMaxMsgs(t *testing.T) {
	sm := NewSessionManager()
	s := sm.GetOrCreate("k")
	// 10 messages, "m0" .. "m9".
	for i := 0; i < 10; i++ {
		s.Append(mkUser(itoa(i)))
	}

	hist := s.GetHistory(3)
	if got := len(hist); got != 3 {
		t.Fatalf("len(GetHistory(3)) = %d, want 3", got)
	}
	wantSeq := []string{"7", "8", "9"}
	for i, w := range wantSeq {
		if got := hist[i].Content[0].Text; got != w {
			t.Errorf("hist[%d] = %q, want %q", i, got, w)
		}
	}

	// LastConsolidated cuts off the prefix. Set it so messages [0..4] are
	// "consolidated"; GetHistory(maxMsgs) should only see [5..9] → 5 msgs.
	s.LastConsolidated = 5
	hist2 := s.GetHistory(100)
	if got := len(hist2); got != 5 {
		t.Fatalf("len(GetHistory after LastConsolidated=5) = %d, want 5", got)
	}
	if hist2[0].Content[0].Text != "5" {
		t.Errorf("first message after consolidation = %q, want %q", hist2[0].Content[0].Text, "5")
	}

	// Asking for MORE than available returns the available count, not nil.
	hist3 := s.GetHistory(50)
	if got := len(hist3); got != 5 {
		t.Errorf("len(GetHistory(50)) = %d, want 5", got)
	}

	// 0 means "use upstream default 120"; with only 5 unconsolidated
	// messages we get those 5.
	hist4 := s.GetHistory(0)
	if got := len(hist4); got != 5 {
		t.Errorf("len(GetHistory(0)) = %d, want 5", got)
	}
}

// itoa is a tiny base-10 stringer so we don't import strconv just for
// test-helper terseness.
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

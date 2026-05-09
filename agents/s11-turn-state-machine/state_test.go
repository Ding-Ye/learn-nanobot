package main

import "testing"

// state_test.go — pure-data tests on the Transitions table. No mocking,
// no goroutines, no I/O. The only thing under test is the static graph.
//
// The point: the state graph IS the architecture of the chapter. If the
// graph is wrong, every other test passing means nothing — the agent
// might still terminate but it'll do so by skipping a state that should
// have run. We assert the table's integrity *separately* from the
// behavior tests so a regression in the graph itself is reported with a
// clear, unambiguous failure.

// TestTransitions_AllStatesReachableFromRestore walks the Transitions
// map starting from StateRestore and asserts that every numbered state
// (StateRestore through StateDone) is visited exactly once.
//
// StateError is NOT in the happy-path graph — it's reached only via the
// error-transition logic in loop.go and is exercised by
// TestLoop_ErrorInBuild_TransitionsToError, not here.
//
// If a future writer adds a state and forgets to wire it into the table,
// this test will fail with "state X not reachable from RESTORE". If a
// writer rewires the graph in a way that introduces a cycle, this test
// will fail because the visit counter exceeds the number of distinct
// states.
func TestTransitions_AllStatesReachableFromRestore(t *testing.T) {
	expected := map[TurnState]bool{
		StateRestore: false,
		StateCompact: false,
		StateCommand: false,
		StateBuild:   false,
		StateRun:     false,
		StateSave:    false,
		StateRespond: false,
		StateDone:    false,
	}

	cur := StateRestore
	visited := map[TurnState]int{}
	const maxSteps = 100

	for steps := 0; steps < maxSteps; steps++ {
		visited[cur]++
		if visited[cur] > 1 {
			t.Fatalf("cycle detected at state %s after %d steps", cur, steps)
		}
		expected[cur] = true
		if cur == StateDone {
			break
		}
		next, ok := Transitions[cur]
		if !ok {
			t.Fatalf("no transition from %s and not StateDone", cur)
		}
		cur = next
	}

	for state, seen := range expected {
		if !seen {
			t.Errorf("state %s not reachable from RESTORE", state)
		}
	}
}

// TestTransitions_NoTerminalStatesInTable asserts the Transitions map
// does NOT contain entries for StateDone or StateError. Terminal states
// must not have outgoing edges — otherwise the driver loop would
// happily keep iterating after Done.
func TestTransitions_NoTerminalStatesInTable(t *testing.T) {
	if _, ok := Transitions[StateDone]; ok {
		t.Errorf("StateDone must not be a key in Transitions")
	}
	if _, ok := Transitions[StateError]; ok {
		t.Errorf("StateError must not be a key in Transitions")
	}
}

// TestTurnState_String covers the String() method for every state. A
// regression that returns "UNKNOWN" for a real state would silently
// degrade the verbose-mode logs and the diff cue with upstream.
func TestTurnState_String(t *testing.T) {
	cases := []struct {
		s    TurnState
		want string
	}{
		{StateRestore, "RESTORE"},
		{StateCompact, "COMPACT"},
		{StateCommand, "COMMAND"},
		{StateBuild, "BUILD"},
		{StateRun, "RUN"},
		{StateSave, "SAVE"},
		{StateRespond, "RESPOND"},
		{StateDone, "DONE"},
		{StateError, "ERROR"},
		{TurnState(99), "UNKNOWN"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("State %d: got %q, want %q", int(c.s), got, c.want)
		}
	}
}

// TestTurnContext_AdvanceAndFail covers the small helpers on
// TurnContext that loop.go's driver depends on. These wrap one-line
// operations but the rules — "fail clears State to StateError",
// "advance respects the table" — are load-bearing.
func TestTurnContext_AdvanceAndFail(t *testing.T) {
	tc := &TurnContext{State: StateRestore}
	if !tc.advance() {
		t.Fatal("expected advance from RESTORE to succeed")
	}
	if tc.State != StateCompact {
		t.Errorf("after advance from RESTORE, got %s, want COMPACT", tc.State)
	}

	tc.fail(errFakeFailure)
	if tc.State != StateError {
		t.Errorf("after fail, State=%s, want ERROR", tc.State)
	}
	if tc.Err != errFakeFailure {
		t.Errorf("after fail, Err=%v, want errFakeFailure", tc.Err)
	}

	// IsTerminal must be true for ERROR and DONE, false for everything else.
	if !tc.IsTerminal() {
		t.Errorf("ERROR should be terminal")
	}
	tc.State = StateDone
	if !tc.IsTerminal() {
		t.Errorf("DONE should be terminal")
	}
	tc.State = StateRun
	if tc.IsTerminal() {
		t.Errorf("RUN should NOT be terminal")
	}
}

var errFakeFailure = newFakeError()

func newFakeError() error { return &fakeErr{msg: "fake failure"} }

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

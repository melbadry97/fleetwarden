package audit

import (
	"sync"
	"testing"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

var at = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func rec(node string, action fleet.Action) Record {
	return Record{
		Node: fleet.NodeID(node), Class: "h100", Action: action,
		Origin: fleet.OriginHealth, Signal: "eccUncorrectable",
		Decision: "admitted within budget 1/10", Outcome: OutcomeApplied,
	}
}

func TestAppend_SealsAndChains(t *testing.T) {
	l := NewLog(WithClock(func() time.Time { return at }))

	first := l.Append(rec("n1", fleet.ActionDrain))
	second := l.Append(rec("n2", fleet.ActionCordon))

	if first.Seq != 1 || second.Seq != 2 {
		t.Errorf("sequence numbers = %d,%d want 1,2", first.Seq, second.Seq)
	}
	if first.PrevHash != "" {
		t.Errorf("genesis record should have empty prevHash, got %q", first.PrevHash)
	}
	if second.PrevHash != first.Hash {
		t.Errorf("chain broken: second.PrevHash = %q, first.Hash = %q", second.PrevHash, first.Hash)
	}
	if first.Hash == second.Hash {
		t.Error("distinct records must hash differently")
	}
	if first.Actor == "" {
		t.Error("actor should default rather than be empty")
	}
	if first.Time != at {
		t.Errorf("time = %v, want injected clock value %v", first.Time, at)
	}
}

func TestVerify_PassesOnIntactChain(t *testing.T) {
	l := NewLog()
	for i := 0; i < 25; i++ {
		l.Append(rec("n1", fleet.ActionDrain))
	}
	if err := l.Verify(); err != nil {
		t.Errorf("intact chain failed verification: %v", err)
	}
}

func TestVerify_DetectsMutatedField(t *testing.T) {
	l := NewLog()
	l.Append(rec("n1", fleet.ActionDrain))
	l.Append(rec("n2", fleet.ActionDrain))

	// Simulate tampering: flip a field without recomputing the hash. This is
	// exactly what an attacker editing a log would do.
	l.mu.Lock()
	l.records[0].Decision = "denied"
	l.mu.Unlock()

	if err := l.Verify(); err == nil {
		t.Error("expected verification to detect the mutated record")
	}
}

func TestVerify_DetectsBrokenLinkage(t *testing.T) {
	l := NewLog()
	l.Append(rec("n1", fleet.ActionDrain))
	l.Append(rec("n2", fleet.ActionDrain))

	// Re-seal the record so its own hash is self-consistent but its link to
	// the predecessor is wrong. A naive per-record checksum would miss this.
	l.mu.Lock()
	l.records[1].PrevHash = "0000"
	l.records[1].Hash = l.records[1].computeHash()
	l.mu.Unlock()

	if err := l.Verify(); err == nil {
		t.Error("expected verification to detect broken chain linkage")
	}
}

func TestSink_ReceivesEveryRecord(t *testing.T) {
	var mu sync.Mutex
	var seen []Record

	l := NewLog(WithSink(SinkFunc(func(r Record) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r)
	})))

	l.Append(rec("n1", fleet.ActionDrain))
	l.Append(rec("n2", fleet.ActionCordon))

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("sink saw %d records, want 2", len(seen))
	}
	if seen[0].Hash == "" {
		t.Error("sink must receive sealed records, not drafts")
	}
}

func TestMaxTail_BoundsMemory(t *testing.T) {
	l := NewLog(WithMaxTail(10))
	for i := 0; i < 100; i++ {
		l.Append(rec("n1", fleet.ActionDrain))
	}
	if got := l.Len(); got != 10 {
		t.Errorf("retained %d records, want the 10-record cap", got)
	}
	// Verification must still work on a truncated tail.
	if err := l.Verify(); err != nil {
		t.Errorf("truncated tail failed verification: %v", err)
	}
}

func TestAppend_ConcurrentSafety(t *testing.T) {
	l := NewLog()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Append(rec("n1", fleet.ActionDrain))
		}()
	}
	wg.Wait()

	if got := l.Len(); got != 200 {
		t.Fatalf("appended %d records, want 200", got)
	}
	if err := l.Verify(); err != nil {
		t.Errorf("concurrent appends produced an invalid chain: %v", err)
	}
}

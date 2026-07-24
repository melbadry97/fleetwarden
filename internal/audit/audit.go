// Package audit provides a hash-chained record of every disruptive action.
//
// Each record embeds the hash of its predecessor, so removing or editing an
// entry invalidates every subsequent hash. This does not prevent tampering —
// an attacker with write access could rebuild the chain — but it makes silent
// tampering detectable, which is the realistic goal for an in-cluster log that
// is also mirrored to an external sink.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

// Outcome is the terminal result of an audited action.
type Outcome string

const (
	OutcomeAdmitted Outcome = "Admitted"
	OutcomeDenied   Outcome = "Denied"
	OutcomeApplied  Outcome = "Applied"
	OutcomeFailed   Outcome = "Failed"
)

// Record is one immutable audit entry.
type Record struct {
	Seq          uint64          `json:"seq"`
	Time         time.Time       `json:"time"`
	Actor        string          `json:"actor"`
	Node         fleet.NodeID    `json:"node"`
	Class        fleet.ClassName `json:"class"`
	Action       fleet.Action    `json:"action"`
	Origin       fleet.Origin    `json:"origin"`
	Signal       string          `json:"signal,omitempty"`
	Observed     string          `json:"observed,omitempty"`
	Decision     string          `json:"decision"`
	Outcome      Outcome         `json:"outcome"`
	BudgetBefore int             `json:"budgetBefore"`
	BudgetAfter  int             `json:"budgetAfter"`
	PrevHash     string          `json:"prevHash"`
	Hash         string          `json:"hash"`
}

// payload is the subset of a record that is hashed. Hash itself is excluded,
// since it is the output.
func (r Record) payload() []byte {
	c := r
	c.Hash = ""
	b, _ := json.Marshal(c)
	return b
}

// computeHash returns the chained hash for a record.
func (r Record) computeHash() string {
	sum := sha256.Sum256(r.payload())
	return hex.EncodeToString(sum[:])
}

// Sink receives records as they are appended. Implementations forward to
// Kubernetes Events, stdout, or an external log pipeline.
type Sink interface {
	Emit(Record)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Record)

// Emit implements Sink.
func (f SinkFunc) Emit(r Record) { f(r) }

// Log is an append-only, hash-chained audit log.
//
// It retains a bounded in-memory tail for verification and debugging; durable
// retention is the sink's responsibility.
type Log struct {
	mu       sync.Mutex
	seq      uint64
	lastHash string
	records  []Record
	maxTail  int
	sinks    []Sink
	now      func() time.Time
}

// Option configures a Log.
type Option func(*Log)

// WithSink registers an output sink.
func WithSink(s Sink) Option { return func(l *Log) { l.sinks = append(l.sinks, s) } }

// WithClock injects a deterministic clock for testing.
func WithClock(f func() time.Time) Option { return func(l *Log) { l.now = f } }

// WithMaxTail bounds the in-memory record tail.
func WithMaxTail(n int) Option { return func(l *Log) { l.maxTail = n } }

// NewLog builds an audit log.
func NewLog(opts ...Option) *Log {
	l := &Log{maxTail: 1024, now: time.Now}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Append records an action and returns the sealed record.
func (l *Log) Append(r Record) Record {
	l.mu.Lock()

	l.seq++
	r.Seq = l.seq
	if r.Time.IsZero() {
		r.Time = l.now()
	}
	if r.Actor == "" {
		r.Actor = "fleetwarden-manager"
	}
	r.PrevHash = l.lastHash
	r.Hash = r.computeHash()
	l.lastHash = r.Hash

	l.records = append(l.records, r)
	if len(l.records) > l.maxTail {
		l.records = l.records[len(l.records)-l.maxTail:]
	}
	sinks := append([]Sink(nil), l.sinks...)

	l.mu.Unlock()

	// Emit outside the lock: a slow sink must not stall reconciliation.
	for _, s := range sinks {
		s.Emit(r)
	}
	return r
}

// Records returns a copy of the retained tail.
func (l *Log) Records() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, len(l.records))
	copy(out, l.records)
	return out
}

// Verify walks the retained tail and reports the first inconsistency found.
//
// Because the tail is bounded, verification starts from whatever record is
// oldest in memory and checks linkage forward from there.
func (l *Log) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, r := range l.records {
		if got := r.computeHash(); got != r.Hash {
			return fmt.Errorf("record seq=%d: hash mismatch (stored %s, computed %s)", r.Seq, r.Hash, got)
		}
		if i > 0 {
			if r.PrevHash != l.records[i-1].Hash {
				return fmt.Errorf("record seq=%d: broken chain (prevHash %s, predecessor hash %s)",
					r.Seq, r.PrevHash, l.records[i-1].Hash)
			}
			if r.Seq != l.records[i-1].Seq+1 {
				return fmt.Errorf("record seq=%d: sequence gap after seq=%d", r.Seq, l.records[i-1].Seq)
			}
		}
	}
	return nil
}

// Len returns the retained record count.
func (l *Log) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

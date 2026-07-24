package signal

import (
	"testing"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

var base = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func eccSpec() Spec {
	return Spec{
		Name:      "eccUncorrectable",
		Kind:      KindPrometheus,
		Query:     "increase(gpu_ecc_uncorrectable_total[10m])",
		Operator:  OpGreaterThan,
		Threshold: "5",
		Window:    10 * time.Minute,
		Severity:  fleet.SeverityCritical,
		Action:    fleet.ActionDrain,
	}
}

func TestSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Spec)
		wantErr bool
	}{
		{"valid", func(s *Spec) {}, false},
		{"missing name", func(s *Spec) { s.Name = "" }, true},
		{"unknown kind", func(s *Spec) { s.Kind = "Telepathy" }, true},
		{"unknown operator", func(s *Spec) { s.Operator = "Vibes" }, true},
		{"prometheus without query", func(s *Spec) { s.Query = "" }, true},
		{"missing action", func(s *Spec) { s.Action = fleet.ActionNone }, true},
		{"negative window", func(s *Spec) { s.Window = -time.Minute }, true},
		// A numeric operator with a non-numeric threshold would fail silently
		// at every evaluation; the webhook must reject it at admission.
		{"non-numeric threshold for GreaterThan", func(s *Spec) { s.Threshold = "high" }, true},
		{"nodecondition without key", func(s *Spec) {
			s.Kind = KindNodeCondition
			s.Key = ""
			s.Operator = OpEquals
			s.Threshold = "True"
		}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := eccSpec()
			tc.mutate(&s)
			err := s.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestObserve_RequiresWindowToElapse(t *testing.T) {
	e := NewEvaluator(time.Hour)
	spec := eccSpec()

	// Breaching, but only 5 minutes into a 10 minute window.
	if bs, err := e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: base}}, base); err != nil {
		t.Fatal(err)
	} else if len(bs) != 0 {
		t.Fatalf("breach fired before window elapsed: %+v", bs)
	}

	at5 := base.Add(5 * time.Minute)
	if bs, _ := e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at5}}, at5); len(bs) != 0 {
		t.Fatalf("breach fired at 5m into a 10m window: %+v", bs)
	}

	at11 := base.Add(11 * time.Minute)
	bs, err := e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at11}}, at11)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 {
		t.Fatalf("expected breach after window elapsed, got %d", len(bs))
	}
	if bs[0].Node != "n1" || bs[0].Action != fleet.ActionDrain {
		t.Errorf("unexpected breach: %+v", bs[0])
	}
	if bs[0].Consecutive != 3 {
		t.Errorf("consecutive = %d, want 3", bs[0].Consecutive)
	}
}

// A single ECC error is noise; five in ten minutes is a dying card. Recovery
// must reset the window so a blip does not accumulate toward a drain.
func TestObserve_RecoveryResetsWindow(t *testing.T) {
	e := NewEvaluator(time.Hour)
	spec := eccSpec()

	e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: base}}, base)

	// Recovers at 5m.
	at5 := base.Add(5 * time.Minute)
	e.Observe(spec, []Sample{{Node: "n1", Value: "0", At: at5}}, at5)

	// Breaches again at 6m. Only 6 minutes have passed overall, but the run
	// restarted, so at 12m the new run is only 6 minutes old.
	at6 := base.Add(6 * time.Minute)
	e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at6}}, at6)

	at12 := base.Add(12 * time.Minute)
	bs, _ := e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at12}}, at12)
	if len(bs) != 0 {
		t.Fatalf("recovery should have reset the window, got breach: %+v", bs)
	}

	// At 17m the second run is 11 minutes old and should fire.
	at17 := base.Add(17 * time.Minute)
	bs, _ = e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at17}}, at17)
	if len(bs) != 1 {
		t.Fatalf("expected breach once the new run exceeded the window, got %d", len(bs))
	}
}

func TestObserve_HysteresisRequiresConsecutive(t *testing.T) {
	e := NewEvaluator(time.Hour)
	spec := eccSpec()
	spec.Window = 0
	spec.RequireConsecutive = 3

	for i, want := range []int{0, 0, 1} {
		at := base.Add(time.Duration(i) * time.Minute)
		bs, err := e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at}}, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(bs) != want {
			t.Errorf("observation %d: got %d breaches, want %d", i+1, len(bs), want)
		}
	}
}

func TestObserve_Operators(t *testing.T) {
	tests := []struct {
		name      string
		operator  Operator
		threshold string
		value     string
		want      bool
	}{
		{"gt breaches", OpGreaterThan, "5", "6", true},
		{"gt at threshold does not breach", OpGreaterThan, "5", "5", false},
		{"lt breaches", OpLessThan, "5", "4", true},
		{"equals breaches", OpEquals, "True", "True", true},
		{"equals does not breach", OpEquals, "True", "False", false},
		{"notequals breaches on drift", OpNotEquals, "2.4.1", "2.3.9", true},
		{"notequals matches baseline", OpNotEquals, "2.4.1", "2.4.1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEvaluator(time.Hour)
			spec := Spec{
				Name: "s", Kind: KindNodeLabel, Key: "k",
				Operator: tc.operator, Threshold: tc.threshold,
				Severity: fleet.SeverityWarning, Action: fleet.ActionTaint,
			}
			bs, err := e.Observe(spec, []Sample{{Node: "n1", Value: tc.value, At: base}}, base)
			if err != nil {
				t.Fatal(err)
			}
			got := len(bs) == 1
			if got != tc.want {
				t.Errorf("breach = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestObserve_NonNumericSampleIsAnError(t *testing.T) {
	e := NewEvaluator(time.Hour)
	_, err := e.Observe(eccSpec(), []Sample{{Node: "n1", Value: "NaNsense", At: base}}, base)
	if err == nil {
		t.Error("expected an error for a non-numeric sample under a numeric operator")
	}
}

func TestObserve_DeterministicOrdering(t *testing.T) {
	spec := eccSpec()
	spec.Window = 0

	e := NewEvaluator(time.Hour)
	samples := []Sample{
		{Node: "n3", Value: "9", At: base},
		{Node: "n1", Value: "9", At: base},
		{Node: "n2", Value: "9", At: base},
	}
	bs, err := e.Observe(spec, samples, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 3 {
		t.Fatalf("want 3 breaches, got %d", len(bs))
	}
	for i, want := range []fleet.NodeID{"n1", "n2", "n3"} {
		if bs[i].Node != want {
			t.Errorf("position %d = %s, want %s", i, bs[i].Node, want)
		}
	}
}

func TestPrune_BoundsMemory(t *testing.T) {
	e := NewEvaluator(10 * time.Minute)
	spec := eccSpec()
	spec.Window = time.Minute

	for i := 0; i < 200; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: at}}, at)
	}

	// Retention is max(10m, 2*window) = 10m, so only recent observations survive.
	if got := e.SeriesLen("n1", spec.Name); got > 15 {
		t.Errorf("retained %d observations, expected pruning to ~10", got)
	}
}

func TestForget_DropsNodeHistory(t *testing.T) {
	e := NewEvaluator(time.Hour)
	spec := eccSpec()
	e.Observe(spec, []Sample{{Node: "n1", Value: "9", At: base}}, base)

	if e.SeriesLen("n1", spec.Name) == 0 {
		t.Fatal("setup: expected retained history")
	}
	e.Forget("n1")
	if got := e.SeriesLen("n1", spec.Name); got != 0 {
		t.Errorf("history after Forget = %d, want 0", got)
	}
}

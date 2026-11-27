package detect

import (
	"testing"
	"time"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/collect"
	"github.com/melbadry97/fleetwarden/internal/fleet"
)

func snap(node, condName, condVal string, at time.Time) collect.Snapshot {
	s := collect.Snapshot{Node: node, At: at, Reachable: true, AuthOK: true, Health: "OK", Conditions: map[string]string{}}
	if condName != "" {
		s.Health = "Critical"
		s.Conditions[condName] = condVal
	}
	return s
}

func TestDetect_ECCBreachAfterHysteresis(t *testing.T) {
	base := time.Now()
	series := map[string][]collect.Snapshot{
		"n1": {snap("n1", "ECCUncorrectable", "7", base), snap("n1", "ECCUncorrectable", "14", base.Add(time.Second)), snap("n1", "ECCUncorrectable", "21", base.Add(2*time.Second))},
		"n2": {snap("n2", "", "", base), snap("n2", "", "", base.Add(time.Second)), snap("n2", "", "", base.Add(2*time.Second))},
	}
	b := Detect(series)
	got, ok := b["n1"]
	if !ok {
		t.Fatal("expected a breach on n1")
	}
	if got.Signal != "eccUncorrectable" || got.Action != fleet.ActionDrain {
		t.Errorf("n1 breach = %q/%s, want eccUncorrectable/Drain", got.Signal, got.Action)
	}
	if got.Consecutive < 2 {
		t.Errorf("n1 consecutive = %d, want >= 2", got.Consecutive)
	}
	if _, bad := b["n2"]; bad {
		t.Error("healthy n2 should not breach")
	}
}

func TestDetect_SingleSpikeDoesNotBreach(t *testing.T) {
	base := time.Now()
	series := map[string][]collect.Snapshot{
		// one breaching poll, then clean: hysteresis (RequireConsecutive 2) blocks it
		"n1": {snap("n1", "ECCUncorrectable", "9", base), snap("n1", "", "", base.Add(time.Second))},
	}
	if _, ok := Detect(series)["n1"]; ok {
		t.Error("a single spike must not breach under hysteresis")
	}
}

func TestDetect_MostUrgentWins(t *testing.T) {
	base := time.Now()
	// node breaches both thermal (warning) and ECC (critical); ECC must win.
	mk := func(i int) collect.Snapshot {
		s := snap("n1", "ECCUncorrectable", "14", base.Add(time.Duration(i)*time.Second))
		s.Conditions["ThermalThrottle"] = "45"
		return s
	}
	series := map[string][]collect.Snapshot{"n1": {mk(0), mk(1), mk(2)}}
	got := Detect(series)["n1"]
	if got.Signal != "eccUncorrectable" {
		t.Errorf("most-urgent breach = %q, want eccUncorrectable (critical beats warning)", got.Signal)
	}
}

func TestECCTrend_ProjectsETA(t *testing.T) {
	base := time.Now()
	snaps := []collect.Snapshot{
		snap("n1", "ECCUncorrectable", "7", base),
		snap("n1", "ECCUncorrectable", "14", base),
		snap("n1", "ECCUncorrectable", "21", base),
	}
	tr, ok := ECCTrend(snaps, 200, 30*time.Second)
	if !ok {
		t.Fatal("expected a trend")
	}
	if tr.SlopePerPoll != 7 {
		t.Errorf("slope = %v, want 7", tr.SlopePerPoll)
	}
	if !tr.Rising || tr.EtaMinutes <= 0 {
		t.Errorf("expected a positive rising ETA, got rising=%v eta=%v", tr.Rising, tr.EtaMinutes)
	}
}

func TestECCTrend_FlatSeriesHasNoETA(t *testing.T) {
	base := time.Now()
	snaps := []collect.Snapshot{
		snap("n1", "ECCUncorrectable", "5", base),
		snap("n1", "ECCUncorrectable", "5", base),
	}
	tr, ok := ECCTrend(snaps, 200, 30*time.Second)
	if !ok {
		t.Fatal("expected a trend")
	}
	if tr.Rising || tr.EtaMinutes != -1 {
		t.Errorf("flat series: rising=%v eta=%v, want false/-1", tr.Rising, tr.EtaMinutes)
	}
}

package diagnose

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/collect"
	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/detect"
	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

func healthy(node string) collect.Snapshot {
	return collect.Snapshot{Node: node, Reachable: true, AuthOK: true, Health: "OK", Conditions: map[string]string{}}
}

// ---- offline diagnoser ----

func TestOffline_ECCDrainWithForecast(t *testing.T) {
	b := signal.Breach{Node: "n1", Signal: "eccUncorrectable", Severity: fleet.SeverityCritical,
		Action: fleet.ActionDrain, Observed: "21", Consecutive: 3}
	tr := detect.Trend{Signal: "eccUncorrectable", EtaMinutes: 12.3}
	a, err := Offline{}.Assess(NodeCase{Node: "n1", Series: []collect.Snapshot{healthy("n1")}, Breach: &b, Trend: &tr})
	if err != nil {
		t.Fatal(err)
	}
	if a.FaultClass != "ecc_uncorrectable" || a.RecommendedAction != "drain" {
		t.Errorf("got %s/%s, want ecc_uncorrectable/drain", a.FaultClass, a.RecommendedAction)
	}
	if a.Prognosis.EtaMinutes != 12.3 {
		t.Errorf("forecast eta = %v, want 12.3", a.Prognosis.EtaMinutes)
	}
}

func TestOffline_FirmwareLieIsNonDisruptive(t *testing.T) {
	fp := collect.FirmwareProbe{Requested: "2.5.0", Accepted: true, Landed: false, Reported: "2.4.1"}
	a, err := Offline{}.Assess(NodeCase{Node: "n1", Series: []collect.Snapshot{healthy("n1")}, Firmware: &fp})
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsBmcLying {
		t.Error("expected isBmcLying=true for accepted-but-not-landed firmware")
	}
	if a.RecommendedAction != "taint" {
		t.Errorf("firmware lie action = %s, want taint (never drain for a firmware fault)", a.RecommendedAction)
	}
}

func TestOffline_UnreachableAndAuthFail(t *testing.T) {
	unreach := healthy("n1")
	unreach.Reachable = false
	a, _ := Offline{}.Assess(NodeCase{Node: "n1", Series: []collect.Snapshot{unreach}})
	if a.FaultClass != "unreachable" || a.RecommendedAction != "cordon" {
		t.Errorf("unreachable: got %s/%s", a.FaultClass, a.RecommendedAction)
	}

	noauth := healthy("n2")
	noauth.AuthOK = false
	a2, _ := Offline{}.Assess(NodeCase{Node: "n2", Series: []collect.Snapshot{noauth}})
	if a2.FaultClass != "auth_fail" || a2.RecommendedAction != "none" {
		t.Errorf("auth_fail: got %s/%s, want auth_fail/none (never disrupt)", a2.FaultClass, a2.RecommendedAction)
	}
}

func TestOffline_HealthyIsNoop(t *testing.T) {
	a, _ := Offline{}.Assess(NodeCase{Node: "n1", Series: []collect.Snapshot{healthy("n1")}})
	if a.FaultClass != "healthy" || a.RecommendedAction != "none" {
		t.Errorf("got %s/%s, want healthy/none", a.FaultClass, a.RecommendedAction)
	}
}

// ---- Claude diagnoser (fake Messages API) ----

func withEndpoint(url string, fn func()) {
	saved := anthropicEndpoint
	anthropicEndpoint = url
	defer func() { anthropicEndpoint = saved }()
	fn()
}

func TestClaude_ParsesToolUseAndSendsForcedTool(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
			t.Error("missing required anthropic headers")
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		resp := map[string]any{"content": []map[string]any{{
			"type": "tool_use", "name": "record_assessment",
			"input": map[string]any{
				"faultClass": "ecc_uncorrectable", "rootCause": "dying card",
				"isBmcLying": false, "confidence": 0.88, "reasoning": "climbing ECC",
				"recommendedAction": "drain",
				"prognosis":         map[string]any{"failureMode": "ecc critical", "etaMinutes": 11.5, "leadTimeAction": "cordon early", "confidence": 0.7},
			}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var a Assessment
	var err error
	withEndpoint(srv.URL, func() {
		a, err = NewClaude("test-key", "claude-haiku-5-5").Assess(NodeCase{Node: "n1", Series: []collect.Snapshot{healthy("n1")}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.FaultClass != "ecc_uncorrectable" || a.RecommendedAction != "drain" || a.Confidence != 0.88 {
		t.Errorf("parsed assessment = %+v", a)
	}
	if a.Prognosis.EtaMinutes != 11.5 {
		t.Errorf("prognosis eta = %v, want 11.5", a.Prognosis.EtaMinutes)
	}
	if a.Node != "n1" || !strings.HasPrefix(a.Source, "claude:") {
		t.Errorf("node/source = %q/%q", a.Node, a.Source)
	}
	// the request must force the structured tool
	tc, _ := gotBody["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "record_assessment" {
		t.Errorf("tool_choice = %v, want forced record_assessment", tc)
	}
}

func TestClaude_SurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "overloaded"}})
	}))
	defer srv.Close()

	var err error
	withEndpoint(srv.URL, func() {
		_, err = NewClaude("k", "m").Assess(NodeCase{Node: "n1", Series: []collect.Snapshot{healthy("n1")}})
	})
	if err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Errorf("expected surfaced API error, got %v", err)
	}
}

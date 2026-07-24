package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestRack(t *testing.T) (*Rack, *httptest.Server) {
	t.Helper()
	r := NewRack("r14", 4)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

func session(t *testing.T, srv *httptest.Server, id string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"UserName": "admin", "Password": "hunter2", "SystemID": id,
	})
	resp, err := http.Post(srv.URL+"/redfish/v1/SessionService/Sessions",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("session request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("session status = %d, want 201", resp.StatusCode)
	}
	tok := resp.Header.Get("X-Auth-Token")
	if tok == "" {
		t.Fatal("no X-Auth-Token returned")
	}
	return tok
}

func inventory(t *testing.T, srv *httptest.Server, id, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems/"+id, nil)
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("inventory request: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func update(t *testing.T, srv *httptest.Server, id, token, version string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"ImageURI": "https://fw.internal/bmc-" + version + ".bin", "Version": version,
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/redfish/v1/Systems/"+id+"/Actions/Update", bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("update request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestServiceRoot_Unauthenticated(t *testing.T) {
	_, srv := newTestRack(t)
	resp, err := http.Get(srv.URL + "/redfish/v1/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("service root status = %d, want 200 (spec requires it unauthenticated)", resp.StatusCode)
	}
}

func TestInventory_RequiresSession(t *testing.T) {
	_, srv := newTestRack(t)
	if code, _ := inventory(t, srv, "r14-0", ""); code != http.StatusUnauthorized {
		t.Errorf("status without token = %d, want 401", code)
	}
}

func TestSession_ExpiredTokenRejected(t *testing.T) {
	r, srv := newTestRack(t)
	clock := time.Now()
	r.SetClock(func() time.Time { return clock })

	tok := session(t, srv, "r14-0")
	if code, _ := inventory(t, srv, "r14-0", tok); code != http.StatusOK {
		t.Fatalf("fresh token rejected: %d", code)
	}

	clock = clock.Add(16 * time.Minute) // past the 15m TTL
	if code, _ := inventory(t, srv, "r14-0", tok); code != http.StatusUnauthorized {
		t.Errorf("expired token status = %d, want 401", code)
	}
}

func TestFault_Unreachable(t *testing.T) {
	r, srv := newTestRack(t)
	if err := r.Inject("r14-1", FaultUnreachable); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"UserName": "a", "Password": "b", "SystemID": "r14-1"})
	resp, err := http.Post(srv.URL+"/redfish/v1/SessionService/Sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("session request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
}

func TestFault_AuthFail(t *testing.T) {
	r, srv := newTestRack(t)
	_ = r.Inject("r14-1", FaultAuthFail)
	body, _ := json.Marshal(map[string]string{"UserName": "a", "Password": "b", "SystemID": "r14-1"})
	resp, err := http.Post(srv.URL+"/redfish/v1/SessionService/Sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("session request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestUpdate_HappyPathApplies(t *testing.T) {
	r, srv := newTestRack(t)
	clock := time.Now()
	r.SetClock(func() time.Time { return clock })
	r.SetApplyLag(2 * time.Second)

	tok := session(t, srv, "r14-0")
	if code := update(t, srv, "r14-0", tok, "2.5.0"); code != http.StatusAccepted {
		t.Fatalf("update status = %d, want 202", code)
	}

	// Immediately after acceptance the old version is still reported. This is
	// correct behaviour, not a bug: acceptance is not application.
	_, inv := inventory(t, srv, "r14-0", tok)
	fw := inv["FirmwareVersion"].(map[string]any)
	if fw["BMC"] != "2.4.1" {
		t.Errorf("version immediately after accept = %v, want 2.4.1", fw["BMC"])
	}

	clock = clock.Add(3 * time.Second)
	_, inv = inventory(t, srv, "r14-0", tok)
	fw = inv["FirmwareVersion"].(map[string]any)
	if fw["BMC"] != "2.5.0" {
		t.Errorf("version after apply lag = %v, want 2.5.0", fw["BMC"])
	}
}

// The nastiest realistic failure: the BMC returns 202, the task claims to be
// running, and the update never lands. A controller that trusts task state
// waits forever; only comparing reported inventory against the target catches
// it.
func TestFault_UpdateAcceptedNeverReboots(t *testing.T) {
	r, srv := newTestRack(t)
	clock := time.Now()
	r.SetClock(func() time.Time { return clock })
	_ = r.Inject("r14-2", FaultUpdateAcceptedNeverReboots)

	tok := session(t, srv, "r14-2")
	if code := update(t, srv, "r14-2", tok, "2.5.0"); code != http.StatusAccepted {
		t.Fatalf("update should still be accepted, got %d", code)
	}

	// Two hours later the session has long since expired, exactly as it would
	// for a controller polling a hung update. Re-authenticate and re-check.
	clock = clock.Add(2 * time.Hour)
	tok = session(t, srv, "r14-2")
	_, inv := inventory(t, srv, "r14-2", tok)
	fw := inv["FirmwareVersion"].(map[string]any)
	if fw["BMC"] != "2.4.1" {
		t.Errorf("hung update should never land, version = %v", fw["BMC"])
	}
}

// The mirror image: the flash succeeded but inventory reports the old version.
// Fools controllers that trust inventory instead of task state.
func TestFault_StaleInventoryLiesAboutSuccess(t *testing.T) {
	r, srv := newTestRack(t)
	_ = r.Inject("r14-3", FaultStaleInventory)

	tok := session(t, srv, "r14-3")
	update(t, srv, "r14-3", tok, "2.5.0")

	_, inv := inventory(t, srv, "r14-3", tok)
	fw := inv["FirmwareVersion"].(map[string]any)
	if fw["BMC"] != "2.4.1" {
		t.Errorf("stale inventory should report the old version, got %v", fw["BMC"])
	}

	// Reality diverges from the report — which is the entire point of the fault.
	b, _ := r.Get("r14-3")
	if b.Firmware.BMC != "2.5.0" {
		t.Errorf("underlying firmware should have applied, got %v", b.Firmware.BMC)
	}
}

func TestFault_HealthConditions(t *testing.T) {
	tests := []struct {
		fault        Fault
		wantHealth   string
		wantCondName string
	}{
		{FaultThermalThrottle, "Warning", "ThermalThrottle"},
		{FaultPCIeLinkDegraded, "Warning", "PCIeLinkWidthDegraded"},
		{FaultECCStorm, "Critical", "ECCUncorrectable"},
	}

	for _, tc := range tests {
		t.Run(string(tc.fault), func(t *testing.T) {
			r, srv := newTestRack(t)
			_ = r.Inject("r14-0", tc.fault)
			tok := session(t, srv, "r14-0")

			_, inv := inventory(t, srv, "r14-0", tok)
			status := inv["Status"].(map[string]any)
			if status["Health"] != tc.wantHealth {
				t.Errorf("health = %v, want %v", status["Health"], tc.wantHealth)
			}
			conds, _ := inv["Conditions"].([]any)
			if len(conds) == 0 {
				t.Fatal("expected at least one condition")
			}
			first := conds[0].(map[string]any)
			if first["Name"] != tc.wantCondName {
				t.Errorf("condition = %v, want %v", first["Name"], tc.wantCondName)
			}
		})
	}
}

// ECC counts must climb across polls so a windowed evaluator sees a rate
// rather than a constant.
func TestFault_ECCStormAccumulates(t *testing.T) {
	r, srv := newTestRack(t)
	_ = r.Inject("r14-0", FaultECCStorm)
	tok := session(t, srv, "r14-0")

	var prev float64
	for i := 0; i < 3; i++ {
		_, inv := inventory(t, srv, "r14-0", tok)
		conds := inv["Conditions"].([]any)
		val := conds[0].(map[string]any)["Value"].(string)
		var cur float64
		_, _ = fmtSscan(val, &cur)
		if i > 0 && cur <= prev {
			t.Errorf("poll %d: ECC count %v did not climb above %v", i, cur, prev)
		}
		prev = cur
	}
}

func TestInjectAll_CorrelatedRackFailure(t *testing.T) {
	r, srv := newTestRack(t)
	r.InjectAll(FaultPCIeLinkDegraded)

	for _, id := range r.IDs() {
		tok := session(t, srv, id)
		_, inv := inventory(t, srv, id, tok)
		status := inv["Status"].(map[string]any)
		if status["Health"] != "Warning" {
			t.Errorf("%s health = %v, want Warning", id, status["Health"])
		}
	}
}

func TestUnknownSystem(t *testing.T) {
	_, srv := newTestRack(t)
	if code, _ := inventory(t, srv, "does-not-exist", "tok"); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// fmtSscan avoids importing fmt just for a test helper conversion.
func fmtSscan(s string, out *float64) (int, error) {
	var v float64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		v = v*10 + float64(c-'0')
	}
	*out = v
	return 1, nil
}

package collect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBMC is a minimal Redfish stand-in: it mints a session token, serves
// inventory with an optional ECC condition, and applies (or lies about) a
// firmware update. It records how many sessions were minted so the test can
// assert session reuse.
type fakeBMC struct {
	mu          sync.Mutex
	version     string
	ecc         string // "" = no condition
	lie         bool   // accept update but never change version
	sessions    int
	lastPending string
}

func (f *fakeBMC) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/redfish/v1/SessionService/Sessions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessions++
		f.mu.Unlock()
		w.Header().Set("X-Auth-Token", "tok-123")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/redfish/v1/Systems/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.Contains(r.URL.Path, "/Actions/Update") {
			var body struct{ Version string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !f.lie {
				f.version = body.Version // honest BMC applies it
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"TaskState": "Running"})
			return
		}
		inv := map[string]any{
			"Id": "n0", "PowerState": "On",
			"Status":          map[string]string{"Health": "OK", "State": "Enabled"},
			"FirmwareVersion": map[string]string{"BMC": f.version},
		}
		if f.ecc != "" {
			inv["Status"] = map[string]string{"Health": "Critical", "State": "Enabled"}
			inv["Conditions"] = []map[string]string{{"Name": "ECCUncorrectable", "Severity": "Critical", "Value": f.ecc}}
		}
		_ = json.NewEncoder(w).Encode(inv)
	})
	return mux
}

func TestPoll_ParsesHealthAndConditions(t *testing.T) {
	bmc := &fakeBMC{version: "2.4.1", ecc: "21"}
	srv := httptest.NewServer(bmc.handler())
	defer srv.Close()

	c := New(srv.URL)
	s := c.Poll("n0", time.Now())
	if !s.Reachable || !s.AuthOK {
		t.Fatalf("expected reachable+authOK, got %+v", s)
	}
	if s.Health != "Critical" || s.BMCVersion != "2.4.1" {
		t.Errorf("health/version = %q/%q, want Critical/2.4.1", s.Health, s.BMCVersion)
	}
	if v, ok := s.Condition("ECCUncorrectable"); !ok || v != "21" {
		t.Errorf("ECC condition = %q/%v, want 21/true", v, ok)
	}
}

func TestPoll_ReusesSession(t *testing.T) {
	bmc := &fakeBMC{version: "2.4.1"}
	srv := httptest.NewServer(bmc.handler())
	defer srv.Close()

	c := New(srv.URL)
	for i := 0; i < 3; i++ {
		c.Poll("n0", time.Now())
	}
	bmc.mu.Lock()
	defer bmc.mu.Unlock()
	if bmc.sessions != 1 {
		t.Errorf("minted %d sessions across 3 polls, want 1 (token should be cached)", bmc.sessions)
	}
}

func TestPushFirmware_HonestBMCLands(t *testing.T) {
	bmc := &fakeBMC{version: "2.4.1", lie: false}
	srv := httptest.NewServer(bmc.handler())
	defer srv.Close()

	p := New(srv.URL).PushFirmware("n0", "2.5.0", 10*time.Millisecond)
	if !p.Accepted || !p.Landed {
		t.Errorf("honest BMC: accepted=%v landed=%v, want both true (reported %q)", p.Accepted, p.Landed, p.Reported)
	}
}

func TestPushFirmware_LyingBMCAcceptsButDoesNotLand(t *testing.T) {
	bmc := &fakeBMC{version: "2.4.1", lie: true}
	srv := httptest.NewServer(bmc.handler())
	defer srv.Close()

	p := New(srv.URL).PushFirmware("n0", "2.5.0", 10*time.Millisecond)
	if !p.Accepted {
		t.Error("expected the update to be accepted (202)")
	}
	if p.Landed {
		t.Error("lying BMC should not land the update")
	}
	if p.Reported != "2.4.1" {
		t.Errorf("reported %q, want the old 2.4.1", p.Reported)
	}
}

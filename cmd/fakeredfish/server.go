// Command fakeredfish simulates a rack of BMCs speaking a useful subset of the
// Redfish API, with injectable faults.
//
// This exists because you cannot meaningfully test fleet lifecycle logic
// against hardware you do not have, and because the interesting failures are
// not "the BMC is down" — they are the ones where the BMC lies to you. The
// fault list here is drawn from failure modes that actually occur: a BMC that
// accepts a firmware update and then never reboots, a session endpoint that
// authenticates but returns stale inventory, a link that renegotiates to a
// narrower width without reporting an error.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fault is an injectable misbehaviour.
type Fault string

const (
	// FaultNone is a healthy BMC.
	FaultNone Fault = "none"
	// FaultUnreachable drops all requests. The easy case.
	FaultUnreachable Fault = "unreachable"
	// FaultAuthFail rejects session creation with valid credentials.
	FaultAuthFail Fault = "auth-fail"
	// FaultStaleInventory returns the previous firmware version after a
	// successful update, which is how a silently failed flash presents.
	FaultStaleInventory Fault = "stale-inventory"
	// FaultUpdateAcceptedNeverReboots accepts SimpleUpdate, returns 202, and
	// then never actually applies it. The nastiest common failure: every
	// status check says "in progress" forever.
	FaultUpdateAcceptedNeverReboots Fault = "update-hangs"
	// FaultThermalThrottle reports sustained thermal throttling.
	FaultThermalThrottle Fault = "thermal"
	// FaultPCIeLinkDegraded reports a link renegotiated to a narrower width.
	FaultPCIeLinkDegraded Fault = "pcie-degraded"
	// FaultECCStorm reports climbing uncorrectable ECC counts.
	FaultECCStorm Fault = "ecc-storm"
	// FaultSlowResponse adds latency, exercising client timeouts.
	FaultSlowResponse Fault = "slow"
)

// AllFaults enumerates injectable faults, used by fwctl and tests.
func AllFaults() []Fault {
	return []Fault{
		FaultNone, FaultUnreachable, FaultAuthFail, FaultStaleInventory,
		FaultUpdateAcceptedNeverReboots, FaultThermalThrottle,
		FaultPCIeLinkDegraded, FaultECCStorm, FaultSlowResponse,
	}
}

// BMC is one simulated baseboard management controller.
type BMC struct {
	ID       string
	Rack     string
	Firmware struct {
		BMC  string
		BIOS string
		CPLD string
	}
	PowerState string
	Fault      Fault
	// pendingVersion is set by SimpleUpdate and applied on the next poll
	// unless a fault prevents it.
	pendingVersion string
	updateStarted  time.Time
	eccCount       int
	sessions       map[string]time.Time
}

// Rack is a collection of simulated BMCs behind one HTTP server.
type Rack struct {
	mu       sync.RWMutex
	bmcs     map[string]*BMC
	sessTTL  time.Duration
	now      func() time.Time
	applyLag time.Duration
}

// NewRack builds a rack of n BMCs named <prefix>-0..n-1.
func NewRack(prefix string, n int) *Rack {
	r := &Rack{
		bmcs:     make(map[string]*BMC, n),
		sessTTL:  15 * time.Minute,
		now:      time.Now,
		applyLag: 2 * time.Second,
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		b := &BMC{ID: id, Rack: prefix, PowerState: "On", Fault: FaultNone, sessions: map[string]time.Time{}}
		b.Firmware.BMC = "2.4.1"
		b.Firmware.BIOS = "1.9.0"
		b.Firmware.CPLD = "0.4.2"
		r.bmcs[id] = b
	}
	return r
}

// SetClock injects a deterministic clock for tests.
func (r *Rack) SetClock(f func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = f
}

// SetApplyLag controls how long a firmware update takes to land.
func (r *Rack) SetApplyLag(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyLag = d
}

// Inject sets a fault on one BMC.
func (r *Rack) Inject(id string, f Fault) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bmcs[id]
	if !ok {
		return fmt.Errorf("no such bmc %q", id)
	}
	b.Fault = f
	return nil
}

// InjectAll sets a fault across the whole rack, simulating a correlated
// failure — which is precisely what random canary sampling fails to catch.
func (r *Rack) InjectAll(f Fault) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bmcs {
		b.Fault = f
	}
}

// Get returns a snapshot of one BMC.
func (r *Rack) Get(id string) (BMC, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.bmcs[id]
	if !ok {
		return BMC{}, false
	}
	return *b, true
}

// IDs lists BMC identifiers in the rack.
func (r *Rack) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.bmcs))
	for id := range r.bmcs {
		out = append(out, id)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Handler returns the HTTP mux implementing the Redfish subset.
func (r *Rack) Handler() http.Handler {
	mux := http.NewServeMux()

	// Service root: unauthenticated by design, per the Redfish spec.
	mux.HandleFunc("/redfish/v1/", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id":      "/redfish/v1/",
			"RedfishVersion": "1.15.0",
			"Systems":        map[string]string{"@odata.id": "/redfish/v1/Systems"},
			"SessionService": map[string]string{"@odata.id": "/redfish/v1/SessionService"},
		})
	})

	mux.HandleFunc("/redfish/v1/SessionService/Sessions", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var body struct{ UserName, Password, SystemID string }
		_ = json.NewDecoder(req.Body).Decode(&body)

		r.mu.Lock()
		defer r.mu.Unlock()
		b, ok := r.bmcs[body.SystemID]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown system"})
			return
		}
		if b.Fault == FaultUnreachable {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "unreachable"})
			return
		}
		if b.Fault == FaultAuthFail {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}
		token := fmt.Sprintf("sess-%s-%d", body.SystemID, r.now().UnixNano())
		b.sessions[token] = r.now().Add(r.sessTTL)
		w.Header().Set("X-Auth-Token", token)
		writeJSON(w, http.StatusCreated, map[string]string{"Id": token})
	})

	mux.HandleFunc("/redfish/v1/Systems/", func(w http.ResponseWriter, req *http.Request) {
		// Path shapes: /Systems/{id} and /Systems/{id}/Actions/Update
		rest := strings.TrimPrefix(req.URL.Path, "/redfish/v1/Systems/")
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		id := parts[0]

		r.mu.Lock()
		defer r.mu.Unlock()
		b, ok := r.bmcs[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown system"})
			return
		}
		if b.Fault == FaultUnreachable {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "unreachable"})
			return
		}
		if b.Fault == FaultSlowResponse {
			time.Sleep(250 * time.Millisecond)
		}
		if !r.authorized(b, req) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or expired session"})
			return
		}

		if len(parts) >= 3 && parts[1] == "Actions" {
			r.handleUpdateLocked(w, req, b)
			return
		}
		r.applyPendingLocked(b)
		writeJSON(w, http.StatusOK, r.inventoryLocked(b))
	})

	return mux
}

func (r *Rack) authorized(b *BMC, req *http.Request) bool {
	tok := req.Header.Get("X-Auth-Token")
	if tok == "" {
		return false
	}
	exp, ok := b.sessions[tok]
	if !ok {
		return false
	}
	// Expired sessions are pruned rather than merely rejected, so a
	// long-running client that leaks tokens does not grow the map forever.
	if r.now().After(exp) {
		delete(b.sessions, tok)
		return false
	}
	return true
}

func (r *Rack) handleUpdateLocked(w http.ResponseWriter, req *http.Request, b *BMC) {
	if req.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var body struct {
		ImageURI string
		Version  string
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	if body.Version == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Version required"})
		return
	}
	b.pendingVersion = body.Version
	b.updateStarted = r.now()
	// 202 Accepted is the honest answer here, and it is exactly what makes
	// firmware rollouts hard: acceptance is not application.
	writeJSON(w, http.StatusAccepted, map[string]string{
		"TaskState": "Running",
		"Message":   "update accepted",
	})
}

// applyPendingLocked lands a pending firmware update, unless a fault prevents
// it. This is where the interesting lies live.
func (r *Rack) applyPendingLocked(b *BMC) {
	if b.pendingVersion == "" {
		return
	}
	switch b.Fault {
	case FaultUpdateAcceptedNeverReboots:
		// Never applies. Inventory keeps reporting the old version while the
		// task claims to be running. A rollout controller that trusts the
		// task state will wait forever.
		return
	case FaultStaleInventory:
		// Actually applied, but inventory lies about it. The mirror image of
		// the above, and it fools controllers that trust inventory instead.
		b.Firmware.BMC = b.pendingVersion
		b.pendingVersion = ""
		return
	}
	if r.now().Sub(b.updateStarted) >= r.applyLag {
		b.Firmware.BMC = b.pendingVersion
		b.pendingVersion = ""
	}
}

func (r *Rack) inventoryLocked(b *BMC) map[string]any {
	reported := b.Firmware.BMC
	if b.Fault == FaultStaleInventory {
		reported = "2.4.1" // whatever it was before, regardless of reality
	}

	health := "OK"
	var conditions []map[string]string
	switch b.Fault {
	case FaultThermalThrottle:
		health = "Warning"
		conditions = append(conditions, map[string]string{
			"Name": "ThermalThrottle", "Severity": "Warning", "Value": "45",
		})
	case FaultPCIeLinkDegraded:
		health = "Warning"
		conditions = append(conditions, map[string]string{
			"Name": "PCIeLinkWidthDegraded", "Severity": "Warning", "Value": "True",
		})
	case FaultECCStorm:
		health = "Critical"
		b.eccCount += 7
		conditions = append(conditions, map[string]string{
			"Name": "ECCUncorrectable", "Severity": "Critical",
			"Value": fmt.Sprintf("%d", b.eccCount),
		})
	}

	return map[string]any{
		"@odata.id":  "/redfish/v1/Systems/" + b.ID,
		"Id":         b.ID,
		"Rack":       b.Rack,
		"PowerState": b.PowerState,
		"Status":     map[string]string{"Health": health, "State": "Enabled"},
		"FirmwareVersion": map[string]string{
			"BMC": reported, "BIOS": b.Firmware.BIOS, "CPLD": b.Firmware.CPLD,
		},
		"Conditions": conditions,
	}
}

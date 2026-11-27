// Package collect reads out-of-band BMC telemetry from a Redfish endpoint
// (the fakeredfish simulator) into plain snapshots. It deliberately preserves
// everything the BMC reports, including values that may be lies: distinguishing
// truth from lie is the diagnostic job, not the collector's.
package collect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Snapshot is one poll of one BMC, reduced to the fields the pipeline reasons
// about.
type Snapshot struct {
	Node       string            `json:"node"`
	At         time.Time         `json:"at"`
	Reachable  bool              `json:"reachable"`
	AuthOK     bool              `json:"authOK"`
	Health     string            `json:"health"`
	PowerState string            `json:"powerState"`
	BMCVersion string            `json:"bmcVersion"`
	Conditions map[string]string `json:"conditions,omitempty"`
}

// Condition returns a reported condition value and whether it was present.
func (s Snapshot) Condition(name string) (string, bool) {
	v, ok := s.Conditions[name]
	return v, ok
}

// FirmwareProbe records the result of pushing a firmware update and re-reading
// inventory. Accepted-but-not-landed is the signature of a BMC that lies.
type FirmwareProbe struct {
	Requested string `json:"requested"`
	Accepted  bool   `json:"accepted"` // BMC returned 202 for SimpleUpdate
	Landed    bool   `json:"landed"`   // inventory then reported the new version
	Reported  string `json:"reported"` // what inventory claims after the push
}

// Client speaks the fakeredfish subset. One session per node, reused across
// polls (the simulator's session TTL is 15 minutes).
type Client struct {
	base     string
	http     *http.Client
	sessions map[string]string // node -> X-Auth-Token
}

// New builds a Client for a base URL such as http://localhost:8443.
func New(base string) *Client {
	return &Client{
		base:     base,
		http:     &http.Client{Timeout: 5 * time.Second},
		sessions: map[string]string{},
	}
}

// session returns a cached token for a node, minting one if needed. The two
// error strings "unreachable" and "auth-fail" are meaningful to callers.
func (c *Client) session(node string) (string, error) {
	if t, ok := c.sessions[node]; ok {
		return t, nil
	}
	body, _ := json.Marshal(map[string]string{
		"UserName": "fleetdoctor", "Password": "x", "SystemID": node,
	})
	req, _ := http.NewRequest(http.MethodPost, c.base+"/redfish/v1/SessionService/Sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("unreachable")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusGatewayTimeout:
		return "", fmt.Errorf("unreachable")
	case http.StatusUnauthorized:
		return "", fmt.Errorf("auth-fail")
	}
	tok := resp.Header.Get("X-Auth-Token")
	if tok == "" {
		return "", fmt.Errorf("no token (status %d)", resp.StatusCode)
	}
	c.sessions[node] = tok
	return tok, nil
}

// inventoryRaw mirrors the simulator's /Systems/{id} payload.
type inventoryRaw struct {
	Id              string                           `json:"Id"`
	Rack            string                           `json:"Rack"`
	PowerState      string                           `json:"PowerState"`
	Status          struct{ Health, State string }   `json:"Status"`
	FirmwareVersion struct{ BMC, BIOS, CPLD string } `json:"FirmwareVersion"`
	Conditions      []struct {
		Name, Severity, Value string
	} `json:"Conditions"`
}

// Poll reads one node's inventory into a Snapshot. Collection never returns an
// error: an unreachable or auth-refusing BMC is itself a diagnosable state,
// recorded in the snapshot's Reachable/AuthOK fields.
func (c *Client) Poll(node string, at time.Time) Snapshot {
	s := Snapshot{Node: node, At: at, Conditions: map[string]string{}}
	tok, err := c.session(node)
	if err != nil {
		s.Reachable = err.Error() != "unreachable"
		s.AuthOK = false
		return s
	}
	s.Reachable, s.AuthOK = true, true

	req, _ := http.NewRequest(http.MethodGet, c.base+"/redfish/v1/Systems/"+node, nil)
	req.Header.Set("X-Auth-Token", tok)
	resp, err := c.http.Do(req)
	if err != nil {
		s.Reachable = false
		return s
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGatewayTimeout {
		s.Reachable = false
		return s
	}
	var inv inventoryRaw
	if b, _ := io.ReadAll(resp.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &inv)
	}
	s.Health = inv.Status.Health
	s.PowerState = inv.PowerState
	s.BMCVersion = inv.FirmwareVersion.BMC
	for _, cond := range inv.Conditions {
		s.Conditions[cond.Name] = cond.Value
	}
	return s
}

// PushFirmware issues a SimpleUpdate and re-reads inventory after settle,
// reporting whether the BMC accepted the update and whether it actually landed.
func (c *Client) PushFirmware(node, version string, settle time.Duration) FirmwareProbe {
	p := FirmwareProbe{Requested: version}
	tok, err := c.session(node)
	if err != nil {
		return p
	}
	body, _ := json.Marshal(map[string]string{
		"ImageURI": "https://fw/bmc-" + version + ".bin", "Version": version,
	})
	req, _ := http.NewRequest(http.MethodPost, c.base+"/redfish/v1/Systems/"+node+"/Actions/Update", bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return p
	}
	p.Accepted = resp.StatusCode == http.StatusAccepted
	resp.Body.Close()

	time.Sleep(settle)
	after := c.Poll(node, time.Now())
	p.Reported = after.BMCVersion
	p.Landed = after.BMCVersion == version
	return p
}

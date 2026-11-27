package diagnose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// anthropicEndpoint is the Messages API. Overridable in tests.
var anthropicEndpoint = "https://api.anthropic.com/v1/messages"

// Claude is a Diagnoser backed by the Anthropic Messages API. It forces a
// single structured tool call so the response is machine-parseable rather than
// free text.
type Claude struct {
	apiKey string
	model  string
	http   *http.Client
}

// NewClaude builds a Claude diagnoser for the given API key and model id.
func NewClaude(apiKey, model string) *Claude {
	return &Claude{apiKey: apiKey, model: model, http: &http.Client{Timeout: 40 * time.Second}}
}

// Name implements Diagnoser.
func (d *Claude) Name() string { return "claude:" + d.model }

const systemPrompt = `You are a hardware-fleet diagnostic engine for GPU accelerator nodes. You are given out-of-band BMC telemetry (Redfish) for one node, polled several times, plus a deterministic trend computed by the caller.

Classify the fault and forecast its progression. Key judgement calls:
- BMCs can lie. A firmware update that returns 202 "accepted" but whose version never changes is either update-hangs (task reports Running forever) or stale-inventory (flash applied, inventory misreports). From inventory alone these are indistinguishable — say so rather than guessing, set isBmcLying=true, and recommend a NON-disruptive action (taint) plus reconciliation.
- Uncorrectable ECC that climbs across polls is a rate, not a blip — consistent with a dying card.
- Prefer the least disruptive action that fits: none < taint (non-disruptive) < cordon < drain < quarantine. Do not recommend draining for a firmware or credential problem.
- Base the ETA on the caller's trend numbers; do not invent a slope.

Respond ONLY by calling record_assessment.`

func toolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"faultClass": map[string]any{"type": "string", "enum": []string{
				"ecc_uncorrectable", "pcie_degraded", "thermal_throttle",
				"firmware_stuck", "firmware_stale_inventory", "firmware_ambiguous",
				"unreachable", "auth_fail", "healthy", "unknown"}},
			"rootCause":  map[string]any{"type": "string"},
			"isBmcLying": map[string]any{"type": "boolean"},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"reasoning":  map[string]any{"type": "string"},
			"recommendedAction": map[string]any{"type": "string", "enum": []string{
				"none", "taint", "cordon", "drain", "quarantine"}},
			"prognosis": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"failureMode":    map[string]any{"type": "string"},
					"etaMinutes":     map[string]any{"type": "number", "description": "minutes until failure at the assumed cadence; -1 if not applicable"},
					"leadTimeAction": map[string]any{"type": "string"},
					"confidence":     map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
				"required": []string{"failureMode", "etaMinutes", "leadTimeAction", "confidence"},
			},
		},
		"required": []string{"faultClass", "rootCause", "isBmcLying", "confidence", "reasoning", "recommendedAction", "prognosis"},
	}
}

// Assess implements Diagnoser by calling the Messages API with a forced
// record_assessment tool call and parsing its structured input.
func (d *Claude) Assess(c NodeCase) (Assessment, error) {
	caseJSON, _ := json.MarshalIndent(c, "", "  ")
	reqBody := map[string]any{
		"model":      d.model,
		"max_tokens": 1024,
		"system":     systemPrompt,
		"messages": []map[string]any{{
			"role":    "user",
			"content": "Telemetry and trend for one node:\n\n" + string(caseJSON),
		}},
		"tools": []map[string]any{{
			"name":         "record_assessment",
			"description":  "Record the structured diagnosis and prognosis for the node.",
			"input_schema": toolSchema(),
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "record_assessment"},
	}
	buf, _ := json.Marshal(reqBody)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, anthropicEndpoint, bytes.NewReader(buf))
	req.Header.Set("x-api-key", d.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return Assessment{}, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Assessment{}, fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return Assessment{}, fmt.Errorf("anthropic error: %s", out.Error.Message)
	}
	for _, blk := range out.Content {
		if blk.Type == "tool_use" && blk.Name == "record_assessment" {
			var a Assessment
			if err := json.Unmarshal(blk.Input, &a); err != nil {
				return Assessment{}, fmt.Errorf("parse tool input: %w", err)
			}
			a.Node = c.Node
			a.Source = d.Name()
			return a, nil
		}
	}
	return Assessment{}, fmt.Errorf("no record_assessment tool call in response (status %d)", resp.StatusCode)
}

# fleetdoctor

An AI-assisted **diagnose-and-predict** pipeline over the `fakeredfish` simulator.

It polls out-of-band BMC telemetry, detects breaches with the same windowing and
hysteresis the controller uses, asks **Claude** to diagnose the fault and forecast
its progression, then routes the recommendation through the **real disruption-budget
accountant**.

```
 poll fakeredfish ──▶ DETECT ──▶ DIAGNOSE ──▶ PREDICT ──▶ DECIDE
   (collect)         (detect:    (diagnose:   (detect:    (budget:
                      signal      Claude /     ECC-slope   Allow / Deny)
                      windowing + offline       forecast)
                      hysteresis) rules)
```

## The design decision that matters

**The LLM advises; the deterministic accountant decides.** A probabilistic
component never drains a node directly — it emits a `fleet.Intent` and the budget
admits or denies it. Diagnosis can be creative; disruption cannot. So even if the
model recommends draining twenty nodes, the accountant still caps the blast radius
at the class ceiling, denies the rest, and a human is paged. This is enforced —
and unit-tested — in `decide.go`.

## One package per stage

Each stage is its own package with its own tests, mirroring the repository's
`controllers` vs `internal` split: orchestration in `main`, every decision behind
a tested boundary.

| Package | Stage | Reuses |
|---|---|---|
| `collect` | Poll `/redfish/v1/Systems/{id}` into snapshots; probe a firmware update. | — |
| `detect` | Feed conditions through a `signal.Evaluator` (windowing + hysteresis); fit the ECC forecast. The Redfish→signal adapter is the `oobSource` the design doc left stubbed. | `internal/signal` |
| `diagnose` | Classify the fault and forecast, via Claude (forced tool-use) or the offline rule engine. | Anthropic API |
| `main` (`decide.go`) | Route each recommendation through the accountant. | `internal/budget` |

Where Claude earns its place is the subtle call: a firmware update that returns
`202 Accepted` but whose version never changes is either `update-hangs` or
`stale-inventory` — **indistinguishable from inventory alone.** The model is asked
to say so rather than guess, flag `isBmcLying`, and recommend a *non-disruptive*
taint plus reconciliation — never a drain for a firmware fault.

## Run it

```bash
make build-sim build-doctor

# a rack with a mix of real hardware faults
./bin/fakeredfish --rack r14 --count 16 \
  --faults r14-1=ecc-storm,r14-2=ecc-storm,r14-3=ecc-storm,r14-4=ecc-storm,r14-5=ecc-storm,r14-8=pcie-degraded,r14-9=thermal,r14-11=unreachable &

# diagnose + forecast + budget-gate (offline rule engine, no key needed)
./bin/fleetdoctor --rack r14 --count 16 --offline
# or simply:
make doctor-demo
```

Add Claude by exporting a key (default model `claude-haiku-5-5` — a rack costs
well under a cent):

```bash
export ANTHROPIC_API_KEY=sk-ant-...
./bin/fleetdoctor --rack r14 --count 16          # drops --offline → calls Claude
```

Probe the lying BMCs by pushing a firmware update and diagnosing the result:

```bash
./bin/fakeredfish --rack r14 --count 6 --faults r14-2=update-hangs,r14-3=stale-inventory &
./bin/fleetdoctor --rack r14 --count 6 --firmware 2.5.0 --offline
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--offline` | false | Use the rule-based diagnoser. Also the automatic fallback when `ANTHROPIC_API_KEY` is unset. |
| `--model` | `claude-haiku-5-5` | Claude model for diagnosis. |
| `--polls` / `--interval` | 4 / 700ms | Telemetry polls and spacing. |
| `--firmware <ver>` | — | Push a firmware update and diagnose whether it actually landed. |
| `--ecc-critical` | 200 | ECC count treated as card-failure for the forecast. |
| `--poll-cadence` | 30s | Assumed production cadence, used only to express the ETA in minutes (the simulator compresses time). |
| `--max-unavailable` / `--min-healthy` | 0.25 / count/2 | The disruption budget the recommendations are gated against. |
| `--json` | false | Emit the full structured assessment. |

## Testing

```bash
go test -race ./cmd/fleetdoctor/...
```

Every stage is unit-tested without the live binary: the collector against an
`httptest` BMC (including a lying one), detection and the trend fit directly, the
offline diagnoser across all fault classes, the Claude diagnoser against a fake
Messages API (forced tool, response parse, error surfacing), and the budget gate
asserting the ceiling holds against over-eager recommendations.

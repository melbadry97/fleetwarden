# fleetwarden — Build, Run and Extend

Everything here works on a laptop. No GPUs, no BMCs, no cluster beyond `kind`.

---

## 1. Prerequisites

| Tool | Version | Needed for |
|---|---|---|
| Go | 1.22+ | everything |
| Docker | any recent | `kind` |
| kind | 0.22+ | local cluster |
| kubectl | 1.29+ | everything |
| kwok | 0.5+ | simulated node fleet (installed by `make kind-up`) |

```bash
brew install go kind kubectl        # macOS
go install sigs.k8s.io/kind@latest  # or via Go
```

`controller-gen` and `setup-envtest` are installed automatically into `bin/` on first use.

---

## 2. Fastest useful loop

The core carries **no Kubernetes dependencies**, so you can exercise the interesting logic in about two seconds without a cluster:

```bash
git clone https://github.com/melbadry97/fleetwarden && cd fleetwarden
make deps
make test-core
```

```
ok  github.com/melbadry97/fleetwarden/internal/audit       coverage: 88.9%
ok  github.com/melbadry97/fleetwarden/internal/budget      coverage: 100.0%
ok  github.com/melbadry97/fleetwarden/internal/fleet       coverage: 100.0%
ok  github.com/melbadry97/fleetwarden/internal/lifecycle   coverage: 92.1%
ok  github.com/melbadry97/fleetwarden/internal/policy      coverage: 88.1%
ok  github.com/melbadry97/fleetwarden/internal/signal      coverage: 90.1%
ok  github.com/melbadry97/fleetwarden/cmd/fakeredfish      coverage: 77.6%
```

This is the loop to use while developing logic. Reserve the cluster for integration behaviour.

---

## 3. Full local demo

```bash
make demo
```

Which does:

1. `kind create cluster --name fleetwarden`
2. installs kwok (Kubernetes WithOut Kubelet)
3. creates **500 simulated H100 nodes** across 20 racks
4. installs the CRDs
5. applies the sample `NodeClass` and `NodeHealthPolicy`

Then run the manager against it:

```bash
make run
```

Webhooks are disabled in `make run` because they need serving certificates. Use `make deploy` for the in-cluster path with cert-manager.

### Watch it work

In a second terminal:

```bash
./hack/watch.sh
```

In a third, inject failures:

```bash
./hack/chaos.sh ecc 40      # 40 nodes report uncorrectable ECC
```

### What you should see

The sample class is 500 nodes with `maxUnavailable: 5%` and `minHealthyAbsolute: 12`, so the ceiling is 25 concurrent disruptions.

| Time | What happens |
|---|---|
| t+0s | 40 nodes begin breaching `eccUncorrectable` |
| t+0s | Nothing moves — the signal has a 10m window and `requireConsecutive: 2` |
| t+10m | All 40 transition `Healthy → Suspect` |
| t+15m | Grace period elapses; escalation to `Cordoned` begins |
| t+15m | **25 admitted, 15 denied** — the budget cap holds |
| t+15m | 15 `BudgetExhausted` warning events fire |

```bash
kubectl get events --field-selector reason=BudgetExhausted | head
```

```
WARNING  BudgetExhausted  nodehealthpolicy/h100-baseline
  denied Drain on gpu-317: would exceed maxUnavailable 25 (5% of 500)
```

That single line is the system working. A controller without central accounting would have drained all 40 and kept going.

### Other scenarios

```bash
./hack/chaos.sh rack r14    # correlated whole-rack failure
./hack/chaos.sh drift 200   # firmware drift — 200 nodes tainted, zero evicted
./hack/chaos.sh pcie 12
./hack/chaos.sh heal        # clear everything
```

The `drift` scenario is worth running deliberately: 200 nodes breach, and **nothing is disrupted**, because `Taint` is non-disruptive and never consumes budget. Gating drift detection on budget would stall it during exactly the incidents where drift matters most.

---

## 4. The Redfish simulator

```bash
make build-sim
./bin/fakeredfish --rack r14 --count 16 \
  --faults r14-3=ecc-storm,r14-7=update-hangs,r14-9=stale-inventory
```

Exercise it directly:

```bash
# Service root is unauthenticated, per spec
curl -s localhost:8443/redfish/v1/ | jq

# Create a session
TOKEN=$(curl -s -D- -X POST localhost:8443/redfish/v1/SessionService/Sessions \
  -H 'Content-Type: application/json' \
  -d '{"UserName":"admin","Password":"x","SystemID":"r14-0"}' \
  | grep -i x-auth-token | cut -d' ' -f2 | tr -d '\r')

# Read inventory
curl -s -H "X-Auth-Token: $TOKEN" localhost:8443/redfish/v1/Systems/r14-0 | jq

# Push firmware
curl -s -X POST -H "X-Auth-Token: $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ImageURI":"https://fw/bmc-2.5.0.bin","Version":"2.5.0"}' \
  localhost:8443/redfish/v1/Systems/r14-0/Actions/Update
```

Try the same update against `r14-7` (`update-hangs`). It returns `202 Accepted` and the task reports `Running`, and the version never changes — no matter how long you wait. Then try `r14-9` (`stale-inventory`), where the flash genuinely applies but inventory keeps reporting `2.4.1`.

Those two faults are mirror images, and together they are the reason a rollout controller cannot trust either task state or inventory alone.

---

## 5. Layout

```
api/v1alpha1/          CRD types — the full API surface
internal/              all logic, zero Kubernetes imports
  fleet/               domain vocabulary
  budget/              the admission accountant
  signal/              windowing, hysteresis, validation
  policy/              breach → intent collapse
  lifecycle/           state machine
  audit/               hash-chained log
controllers/           thin reconcilers — orchestration only
webhooks/validating/   admission-time safety
cmd/manager/           controller-manager entrypoint
cmd/fakeredfish/       BMC simulator
config/                CRDs, RBAC, deployment, samples
hack/                  demo and chaos scripts
```

**The `controllers/` vs `internal/` split is load-bearing.** Reconcilers fetch state, delegate, and apply — no decision-making. Every non-trivial decision lives behind an interface in `internal/`, which is what makes the state machine and budget accountant exhaustively testable without an API server. It is also the difference between a controller you can test and one you can only run.

---

## 6. Extending it

### Add a signal source

Implement one method:

```go
type Source interface {
    Fetch(spec signal.Spec) ([]signal.Sample, error)
}
```

Register it in `buildSources()` in `cmd/manager/main.go`. The evaluator handles windowing, hysteresis and breach determination; a source only produces raw samples.

### Add a lifecycle state

1. Add the constant and include it in `State.Valid()` in `internal/fleet/types.go`
2. Add the case to `Machine.Next()` in `internal/lifecycle/statemachine.go`
3. Add rows to the `TestTransitions` table — the state machine is pure, so a new state costs a test case, not a fixture
4. Handle the state in `applyTransition()` in the reconciler

### Implement the rollout engine

`internal/rollout` is the natural next module. It already has a designed shape ([design doc §5](DESIGN.md)) and needs only:

- wave selection with topology spread (canary one node per rack, because a rack-correlated failure is exactly what random sampling misses)
- soak timers between waves
- a **differential** health gate — compare the updated wave against an un-updated control group rather than an absolute threshold, so fleet-wide degradation from an unrelated cause does not trigger a false rollback
- halt-then-rollback, with rollback waves admitted by the same budget as forward waves, because a panicked mass rollback is just another way to drain the fleet

---

## 7. Troubleshooting

**Nodes never leave `Suspect`.** The grace period has not elapsed. The sample policy uses 5m; check `.spec.escalation.gracePeriod` and the `fleet.elbadry.dev/entered-state` annotation on the node.

**Everything is denied immediately.** The class is at its floor. Check `kubectl get nodeclass -o wide` — if `HEALTHY` equals `minHealthyAbsolute`, no further disruption is permitted by design.

**No breaches despite injected chaos.** The Prometheus source is a stub in this slice. `NodeCondition` and `NodeLabel` signals work fully; `Prometheus` and `OutOfBand` return `ErrNotImplemented`. `./hack/chaos.sh drift` exercises the working path end to end.

**Budget appears stuck after a manager restart.** In-flight slots are reclaimed after `--budget-stale-timeout` (default 30m). Without that reaper, a controller that crashes mid-drain would leak its slot permanently and eventually wedge the fleet.

**Two managers, weird budget behaviour.** Run with `--leader-elect=true`. Two active managers each hold their own ledger and will double-spend the budget.

---

## 8. Design notes worth reading

The [design doc](DESIGN.md) covers the full system. The sections most worth your time:

- **§2** — why `budget` is a standalone module with no dependencies
- **§5** — why canaries are topology-spread and health gates are differential
- **§8.3** — out-of-band credential isolation, and why BMC credentials are the crown jewels
- **§13** — alternatives considered, including why `MachineHealthCheck` is the wrong shape for accelerator fleets

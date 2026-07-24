# fleetwarden

A hardware-aware Kubernetes operator for GPU fleet node lifecycle management and rollback-gated firmware rollouts.

> Treats accelerator nodes the way a hyperscale fleet actually treats them: continuously health-scored, progressively quarantined, and never mass-disrupted without a fleet-wide availability budget.

![fleetwarden running: core suite, a lying Redfish BMC, and the disruption budget holding at 25 admitted / 15 denied](docs/fleetwarden-run.gif)

<sub>The core suite under the race detector, the Redfish simulator lying about a firmware update, and 40 injected ECC failures collapsing to **25 escalations and 15 denials** — driven through the real accountant.</sub>

```
  Healthy ──(breach)──▶ Suspect ──(persists past grace)──▶ Cordoned
     ▲                     │                                   │
     │              (recovers)                          (grace elapsed)
     │                     │                                   ▼
     └──(manual clear)── Quarantined ◀──(drain done)──── Draining
```

---

## Why this exists

Most Kubernetes operators reason about applications. This one reasons about hardware, because the failures that take down accelerator fleets are not application failures — they are ECC storms, PCIe links renegotiating to a narrower width, thermal throttling, and firmware that reports success without applying.

The specific gap it fills: Node Problem Detector *detects* but does not remediate progressively. Cluster API's `MachineHealthCheck` remediates by deleting and reprovisioning, which is the wrong answer when a node represents six figures of capital and hours of bring-up. `fleetwarden` is the coordination layer between them — quarantine and repair rather than destroy and recreate, with a disruption budget that spans every subsystem.

## The design decision that matters

**Every disruptive action routes through one admission point that holds fleet-wide state.** Modules do not disrupt nodes; they emit intents and receive a decision.

```
   policy ──intent──┐
   rollout ─intent──┤
   maintenance ─────┼──▶ budget.Admit(intent) ──▶ Allow | Deny(reason) | Defer
   sizing ──────────┘
```

The failure this prevents: a bad firmware push makes 200 nodes report unhealthy. A naive controller drains all 200 and the remediation *becomes* the outage. Here, the 11th drain is denied, the rollout halts, and a human is paged. Blast radius is bounded by construction rather than by hoping each subsystem is individually well behaved.

Three further details that come from how hyperscale fleets are actually operated, not from a tutorial:

**Signals are windowed with hysteresis.** A single ECC error is noise; five in ten minutes is a dying card. Recovery resets the window, so a blip cannot accumulate toward a drain.

**The ratio ceiling floors rather than rounds.** A 5% ceiling on a 10-node class permits zero disruptions, not one. Rounding up silently doubles the intended blast radius on exactly the small classes where a percentage is already a poor guardrail — which is why there is also an absolute healthy floor.

**Planned work cannot consume the headroom reserved for unplanned failure.** Rollouts and maintenance draw from a capped share of the budget. A dying node does not wait for a maintenance window, so unplanned remediation may use the whole budget; scheduled work may not.

## Quickstart

```bash
make deps                 # fetch modules
make test-core            # unit tests, no cluster required (~2s)
make demo                 # kind + kwok + 500 simulated nodes + CRDs
make run                  # run the manager against the demo cluster

./hack/chaos.sh ecc 40    # 40 nodes start reporting uncorrectable ECC
./hack/watch.sh           # watch states escalate and the budget cap hold
```

Injecting failures on 40 nodes in a 500-node class with a 5% ceiling produces **25 escalations and 15 denials**, not 40 drains. That is the whole point.

## Build status

The dependency-free core (`internal/`, `cmd/fakeredfish`) is verified: it compiles, passes `go vet`, and passes its full suite under the race detector. The Kubernetes-coupled layer (`api/`, `controllers/`, `webhooks/`, `cmd/manager`) is written but has not yet had its first `envtest` run — treat it as reviewed-not-yet-executed until `make test` passes locally. Expect to fix import and codegen details on the first `make manifests && make generate`.

## Implemented vs. designed

This repository is a vertical slice: one complete path through every architectural layer, with the remainder typed and stubbed. The [full design](docs/DESIGN.md) covers nine modules; M1 implements four end to end.

| Module | Status | Notes |
|---|---|---|
| `internal/fleet` | **Implemented** | Domain types, 100% covered |
| `internal/budget` | **Implemented** | Admission accountant, 100% covered |
| `internal/signal` | **Implemented** | Windowing + hysteresis, 90% covered |
| `internal/policy` | **Implemented** | Breach → intent collapse, 88% covered |
| `internal/lifecycle` | **Implemented** | State machine, 92% covered |
| `internal/audit` | **Implemented** | Hash-chained log, 89% covered |
| `controllers/nodehealth` | **Written** | Reconciler, taint/cordon/drain via eviction API — awaiting first envtest run |
| `webhooks/validating` | **Written** | Policy and class admission — awaiting first envtest run |
| `cmd/fakeredfish` | **Implemented** | BMC simulator, 78% covered |
| `internal/rollout` | Designed | Wave engine, health gates, auto-rollback |
| `internal/maintenance` | Designed | Window scheduling, blackouts |
| `internal/sizing` | Designed | Pool resize, workload rightsizing |
| `internal/oob` | Designed | Credential broker, short-TTL session minting |

Staging it this way is deliberate. The API surface is complete because the shape of the API is the clearest statement of the design; the implementation proves the architecture on one path before widening.

## Testing

The core carries no Kubernetes dependencies, so the interesting logic is exhaustively testable without an API server:

```bash
make test-core   # 140+ subtests, race detector on, ~2 seconds
make test        # adds envtest-backed controller tests
```

```
internal/budget      100.0%     internal/lifecycle    92.1%
internal/fleet       100.0%     internal/signal       90.1%
internal/audit        88.9%     internal/policy       88.1%
cmd/fakeredfish       77.6%
```

That the budget accountant sits at 100% is not an accident — it is the component whose failure mode is an outage.

Two bugs the tests caught during development, both worth reading:

- **The accountant trusted the caller's fleet snapshot.** Under parallel reconciles every goroutine observed `Unavailable: 0`, so all 100 concurrent drains were admitted and the budget silently did nothing. Fixed by reconciling the caller's view against the accountant's own in-flight ledger and taking the maximum. `TestAdmit_ConcurrentSafety` and `TestAdmit_StaleViewDoesNotOverAdmit` guard it.
- **A hung firmware update expires the session before it can be observed.** Caught while testing the simulator's `update-hangs` fault, and a real operational trap: any controller polling a stuck update for hours must re-authenticate to keep looking.

## The simulator

`cmd/fakeredfish` serves a Redfish-conformant subset for a simulated rack, with injectable faults. The interesting ones are not "the BMC is down" — they are the ones where the BMC lies:

| Fault | Behaviour |
|---|---|
| `update-hangs` | Accepts `SimpleUpdate`, returns 202, task reports Running forever, update never lands |
| `stale-inventory` | Flash *succeeds* but inventory keeps reporting the old version |
| `pcie-degraded` | Link renegotiated to a narrower width, reported as a Warning condition |
| `ecc-storm` | Uncorrectable counts climb across polls, so windowed evaluators see a rate |
| `auth-fail` | Authenticates cleanly against valid credentials, then refuses |

`update-hangs` and `stale-inventory` are deliberate mirror images: one fools a controller that trusts task state, the other fools a controller that trusts inventory. Handling a rollout correctly requires reconciling both.

```bash
make build-sim
./bin/fakeredfish --rack r14 --count 16 --faults r14-3=ecc-storm,r14-7=update-hangs
```

## Security posture

See [design doc §8](docs/DESIGN.md) for the full model. The short version:

- **RBAC is deliberately narrow.** `nodes` carries `patch` but not `update`; `nodes/status` is absent entirely, because status belongs to the kubelet. Secrets access is a namespaced `Role`, never part of the `ClusterRole` — a cluster-wide secrets grant on a fleet operator is a lateral-movement path to every credential in the cluster.
- **Drains go through the eviction API**, never direct pod deletion, so PodDisruptionBudgets are honoured. Bypassing them would defeat the point of having a disruption budget.
- **BMC credentials never enter a CRD, a status, a log line, or an Event.** The broker mints 15-minute, single-rack-scoped session handles and is the only component with egress to the management network.
- **Readiness verifies the audit chain.** A manager that cannot produce a verifiable audit trail should not be taking disruptive actions.

## Documentation

- [docs/DESIGN.md](docs/DESIGN.md) — full platform design: nine modules, six CRDs, security architecture, sizing, maintenance, alternatives considered
- [docs/GUIDE.md](docs/GUIDE.md) — running it locally, walking the demo, extending it

## Next

Rollout wave engine with differential health gates (compare the wave against an un-updated control group rather than an absolute threshold, so fleet-wide degradation from an unrelated cause does not trigger a false rollback). Then maintenance windows with reserved budget share, then the OOB broker.

## License

Apache 2.0

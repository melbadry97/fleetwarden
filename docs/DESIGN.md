# fleetwarden — Platform Design Document

**Status:** Draft · **Author:** Mohammed Elbadry · **Scope:** Accelerator fleet lifecycle control plane for Kubernetes

---

## 0. How to read this document

This is written as a real design doc — the full system, decomposed the way it would be if a team of six were building it over two quarters.

**The implementation is deliberately a vertical slice.** One weekend gets you the complete API surface plus one end-to-end path through every layer (signal → policy → budget → action → audit). Everything else is designed, typed, and stubbed. That is not a shortcut; it is how large systems are actually reviewed and staged, and a reviewer who reads §1–§9 and then sees §10 shipped will read it as engineering judgment rather than incompleteness.

**Everything here is fully modelable without hardware.** `kwok` simulates the node fleet; a `fakeredfish` server simulates out-of-band management endpoints. You can run a 500-node fleet with realistic BMC behavior on a laptop.

---

## 1. Goals and non-goals

### Goals
- **G1** — Continuously health-score accelerator nodes from multi-source signals and remediate progressively, with every action attributable to a triggering signal.
- **G2** — Never exceed a fleet-wide availability budget, regardless of how many nodes report unhealthy simultaneously.
- **G3** — Perform firmware/driver rollouts in gated waves with automatic halt and rollback.
- **G4** — Coordinate planned maintenance with unplanned remediation so the two cannot compound into an outage.
- **G5** — Manage node capacity by hardware class, including pool resize and workload rightsizing.
- **G6** — Treat out-of-band management credentials as the highest-sensitivity secret in the system.

### Non-goals
- Not a scheduler. Workload placement stays with kube-scheduler; `fleetwarden` only influences it via taints and cordons.
- Not a provisioning system. Bare-metal imaging is upstream; the fleet is assumed to exist.
- Not a monitoring system. It consumes Prometheus; it does not replace it.
- No multi-cluster federation in v1. Single cluster, explicitly deferred (§12).

---

## 2. System decomposition

Nine modules, each with one responsibility and an explicit interface. The dependency graph is acyclic — that property is what makes the vertical slice possible.

| Module | Responsibility | Depends on |
|---|---|---|
| `signal` | Ingest, normalize, and window telemetry into typed health signals | — |
| `policy` | Evaluate signals against `NodeHealthPolicy`; emit intents | `signal` |
| `budget` | Fleet-wide availability accounting; admit or deny every disruptive intent | — |
| `lifecycle` | Node state machine; execute admitted intents (taint/cordon/drain/quarantine) | `policy`, `budget` |
| `rollout` | Staged wave engine, health gates, halt and rollback | `lifecycle`, `budget`, `oob` |
| `maintenance` | Window scheduling, blackouts, planned-drain coordination | `budget` |
| `sizing` | Node classes, pool capacity, resize orchestration, rightsizing | `lifecycle` |
| `oob` | Out-of-band management client (Redfish/IPMI), credential brokering | `secrets` |
| `audit` | Structured, tamper-evident action log; Kubernetes Events; traces | all |

### Why `budget` is a standalone module with no dependencies

This is the most important architectural decision in the document, and the one worth defending out loud in an interview.

Every disruptive action in the system — unplanned remediation, a rollout wave, a maintenance drain, a pool downsize — routes through a single admission point that holds fleet-wide state. Modules do not disrupt nodes; they *request* disruption and receive an admission decision.

The failure this prevents: a bad firmware push makes 200 nodes report unhealthy. A naive controller drains 200 nodes and takes the cluster down — the remediation becomes the outage. With a central budget, the 11th drain is denied, the rollout is halted, and a human is paged. The blast radius is bounded by construction rather than by hoping each subsystem is individually well-behaved.

```
   policy ──intent──┐
   rollout ─intent──┤
   maintenance ─────┼──▶ budget.Admit(intent) ──▶ Allow | Deny(reason) | Defer(until)
   sizing ──────────┘              │
                                   └── holds: unavailable count, class quotas,
                                       in-flight drains, blackout state
```

---

## 3. Package layout

```
fleetwarden/
├── api/v1alpha1/                   # CRD types — the full surface, week one
│   ├── nodehealthpolicy_types.go
│   ├── firmwarerollout_types.go
│   ├── maintenancewindow_types.go
│   ├── nodeclass_types.go
│   ├── nodepoolsizing_types.go
│   ├── oobcredential_types.go
│   ├── groupversion_info.go
│   └── zz_generated.deepcopy.go
│
├── cmd/
│   ├── manager/main.go             # controller-manager entrypoint
│   ├── fwctl/                      # operator CLI (status, simulate, break-glass)
│   └── fakeredfish/                # simulated BMC fleet — see §11
│
├── internal/
│   ├── signal/
│   │   ├── source.go               # Source interface
│   │   ├── prometheus.go           # PromQL-backed source
│   │   ├── condition.go            # Node condition source
│   │   ├── label.go                # Node label source (firmware drift)
│   │   └── window.go               # sliding-window evaluator
│   ├── policy/
│   │   ├── evaluator.go
│   │   └── intent.go               # DisruptionIntent type
│   ├── budget/
│   │   ├── accountant.go           # the single admission point
│   │   ├── quota.go                # per-NodeClass quotas
│   │   └── ledger.go               # in-flight disruption ledger
│   ├── lifecycle/
│   │   ├── statemachine.go
│   │   ├── actions.go              # taint / cordon / drain / quarantine
│   │   └── drain.go                # eviction API wrapper, PDB-aware
│   ├── rollout/
│   │   ├── waves.go
│   │   ├── gate.go                 # health gate evaluation
│   │   └── rollback.go
│   ├── maintenance/
│   │   ├── schedule.go             # cron windows, blackouts
│   │   └── coordinator.go
│   ├── sizing/
│   │   ├── classes.go
│   │   ├── resize.go               # pool scale up/down
│   │   └── rightsize.go            # in-place pod vertical scaling
│   ├── oob/
│   │   ├── client.go               # Redfish/IPMI interface
│   │   ├── redfish.go
│   │   ├── session.go              # short-TTL session tokens
│   │   └── broker.go               # credential brokering, never returns raw creds
│   ├── secrets/
│   │   ├── provider.go             # pluggable backend interface
│   │   └── k8ssecret.go            # v1 backend; ESO/Vault behind same interface
│   └── audit/
│       ├── log.go                  # structured, hash-chained
│       ├── events.go               # Kubernetes Events
│       └── trace.go                # OpenTelemetry spans
│
├── controllers/                    # thin reconcilers — orchestration only, no logic
│   ├── nodehealth_controller.go
│   ├── firmwarerollout_controller.go
│   ├── maintenancewindow_controller.go
│   └── nodepoolsizing_controller.go
│
├── webhooks/
│   ├── validating/
│   │   ├── policy_webhook.go       # reject unsafe policies
│   │   └── rollout_webhook.go      # blast-radius admission
│   └── mutating/
│       └── defaults_webhook.go
│
├── config/                         # kustomize: RBAC, CRDs, certs, policies
└── test/
    ├── envtest/
    └── e2e/                        # kind + kwok + fakeredfish
```

**The `controllers/` vs `internal/` split is load-bearing.** Reconcilers do orchestration and nothing else — no business logic, no I/O decisions. All logic lives in `internal/` behind interfaces, which is what makes the state machine, the budget accountant, and the gate evaluator unit-testable without a cluster. Reviewers notice this immediately; it is the difference between a controller you can test and one you can only run.

---

## 4. API surface

Six CRDs. Define all six in week one — the type definitions are cheap, and the shape of the API is the clearest statement of the design.

### 4.1 `NodeClass` — hardware taxonomy

Everything else keys off this. Sizing, quotas, and rollout targeting are all class-scoped.

```yaml
apiVersion: fleet.elbadry.dev/v1alpha1
kind: NodeClass
metadata:
  name: h100-sxm-8x
spec:
  selector:
    matchLabels:
      fleet.elbadry.dev/sku: h100-sxm-8x
  hardware:
    accelerators: { count: 8, model: H100-SXM5-80GB, interconnect: NVLink4 }
    cpu: { sockets: 2, cores: 112 }
    memory: 2Ti
    localStorage: 30Ti
    powerEnvelopeWatts: 10200
  topology:
    rackAware: true
    spreadDomain: topology.kubernetes.io/rack
  disruption:
    maxUnavailable: 5%          # class-scoped quota, enforced by budget
    minHealthyAbsolute: 12      # floor — never go below this many, ever
  firmware:
    baseline:
      bmc: "2.4.1"
      bios: "1.9.0"
      cpld: "0.4.2"
```

### 4.2 `NodeHealthPolicy` — signals and escalation

```yaml
apiVersion: fleet.elbadry.dev/v1alpha1
kind: NodeHealthPolicy
metadata:
  name: h100-baseline
spec:
  nodeClassRef: { name: h100-sxm-8x }

  signals:
    - name: eccUncorrectable
      source: { type: Prometheus, query: 'increase(gpu_ecc_uncorrectable_total[10m])' }
      operator: GreaterThan
      threshold: "5"
      window: 10m
      severity: Critical
      action: Drain

    - name: pcieLinkDegraded
      source: { type: NodeCondition, key: PCIeLinkWidthDegraded }
      operator: Equals
      threshold: "True"
      window: 2m
      severity: Warning
      action: Cordon

    - name: firmwareDrift
      source: { type: NodeLabel, key: fleet.elbadry.dev/bmc-version }
      operator: NotEquals
      threshold: "2.4.1"
      severity: Info
      action: Taint

    - name: oobUnreachable
      source: { type: OutOfBand, probe: RedfishServiceRoot }
      operator: Equals
      threshold: "Unreachable"
      window: 5m
      severity: Critical
      action: Quarantine       # cannot manage it, cannot trust it

  escalation:
    gracePeriod: 5m
    drainTimeout: 15m
    requireConsecutiveBreaches: 2   # hysteresis — no flapping

  repair:
    quarantineLabel: fleet.elbadry.dev/repair-required
    requireManualClear: true
    autoReturnAfterVerify: false
```

### 4.3 `FirmwareRollout`, 4.4 `MaintenanceWindow`, 4.5 `NodePoolSizing`, 4.6 `OOBCredential`

See §5 (rollouts), §6 (maintenance), §7 (sizing), §8 (credentials) — each CRD is presented alongside the semantics it encodes, which reads better than a wall of YAML.

---

## 5. Rollout engine

```yaml
apiVersion: fleet.elbadry.dev/v1alpha1
kind: FirmwareRollout
metadata:
  name: bmc-2-5-0
spec:
  nodeClassRef: { name: h100-sxm-8x }
  component: bmc
  targetVersion: "2.5.0"
  rollbackVersion: "2.4.1"

  waves:
    - { percent: 1,   soak: 2h,  topologySpread: rack }   # canary, one per rack
    - { percent: 10,  soak: 4h }
    - { percent: 50,  soak: 8h }
    - { percent: 100, soak: 24h }

  gate:
    maxNodeFailureRate: 2%
    evaluationWindow: 30m
    requireSignals: [eccUncorrectable, oobUnreachable, pcieLinkDegraded]
    baselineComparison: true        # compare wave vs un-updated control group

  onGateBreach:
    action: HaltAndRollback
    notify: [pagerduty-fleet]

  maintenanceWindowRef: { name: weekly-tuesday }
```

Three details worth defending in a review:

- **Canary is topology-spread, not random.** One node per rack. A rack-correlated failure that random sampling would miss shows up in wave one.
- **The gate compares against a control group**, not an absolute threshold. If the whole fleet degrades for unrelated reasons, an absolute threshold produces a false rollback. Differential comparison against un-updated nodes isolates the change.
- **Rollback is bounded by the same budget as everything else.** A panicked mass rollback is just another way to drain the fleet. Rollback waves are admitted by `budget` exactly like forward waves.

---

## 6. Maintenance model

```yaml
apiVersion: fleet.elbadry.dev/v1alpha1
kind: MaintenanceWindow
metadata:
  name: weekly-tuesday
spec:
  schedule: "0 2 * * 2"           # Tue 02:00
  duration: 4h
  timezone: America/Los_Angeles
  nodeClassRefs: [{ name: h100-sxm-8x }]

  blackouts:
    - name: training-run-freeze
      selector:
        matchLabels: { workload.fleet/critical: "true" }
      until: "2026-09-01T00:00:00Z"

  budgetShare: 60%                # planned work may consume at most 60% of the
                                  # disruption budget, leaving headroom for
                                  # unplanned failures during the window
  drainStrategy:
    respectPDB: true
    gracePeriod: 30m
    evictionRate: 5/min
```

`budgetShare` is the point of this CRD. Planned and unplanned disruption draw from one pool; reserving headroom means a hardware failure during a maintenance window does not exceed the availability floor. This is the coordination that G4 asks for, and it falls out of the architecture for free because `budget` is centralized.

---

## 7. Sizing and resize

Two distinct problems, deliberately separated.

### 7.1 Pool sizing — how many nodes of each class

```yaml
apiVersion: fleet.elbadry.dev/v1alpha1
kind: NodePoolSizing
metadata:
  name: h100-prod
spec:
  nodeClassRef: { name: h100-sxm-8x }
  target:
    min: 48
    max: 512
    desired: 320
  policy:
    scaleUp:   { pendingPodThreshold: 10, stabilization: 5m,  rate: 32/hour }
    scaleDown: { utilizationBelow: 40%,   stabilization: 60m, rate: 8/hour }
  drainOnScaleDown:
    respectPDB: true
    preferEmptyNodes: true
```

Scale-down is intentionally an order of magnitude slower than scale-up and prefers empty nodes. Removing capacity is the dangerous direction; asymmetric rates encode that.

### 7.2 Workload rightsizing — how big each pod should be

Uses in-place pod vertical scaling (Kubernetes 1.27+, `resize` subresource) to adjust requests from observed utilization without eviction. Recommendation-only by default; enforcement is opt-in per namespace.

```
observed utilization (p95, 7d) ──▶ recommendation ──▶ [dry-run | in-place resize]
                                          │
                                          └── never resizes below observed p99
```

---

## 8. Security architecture

Six layers. §8.3 is the one that matters most and the one your background makes credible.

### 8.1 RBAC — least privilege for the operator itself

The manager runs with the narrowest viable ServiceAccount. Notably it does **not** get blanket `nodes/*`:

| Resource | Verbs | Why |
|---|---|---|
| `nodes` | get, list, watch, patch | patch for taints/labels only — not `update` |
| `nodes/status` | — | never; status belongs to kubelet |
| `pods/eviction` | create | drains go through the eviction API, honoring PDBs |
| `fleet.elbadry.dev/*` | full | own CRDs |
| `secrets` | get in `fleetwarden-creds` namespace only | scoped Role, not ClusterRole |
| `events` | create, patch | audit trail |

Split into `ClusterRole` (node-scoped reads) plus a namespaced `Role` for secrets. The distinction is deliberate and worth a sentence in the README: a cluster-wide secrets grant on a fleet operator is a lateral-movement path to every credential in the cluster.

### 8.2 Admission control — blast radius as policy

Two validating webhooks:

**Policy webhook** rejects `NodeHealthPolicy` objects that are unsafe on their face — `maxUnavailable` above the class ceiling, windows shorter than the scrape interval (guaranteed flapping), `Quarantine` actions without `requireManualClear`.

**Rollout webhook** enforces blast-radius rules: first wave must be ≤5%, a `rollbackVersion` is mandatory, a `MaintenanceWindow` reference is required for classes marked `critical`, and creation requires a distinct RBAC verb from routine policy edits. Rolling firmware to 500 GPUs is a categorically different act from editing a threshold, and the authorization model should say so.

### 8.3 Out-of-band credential isolation

BMC credentials are the crown jewels: they grant power control, firmware write, and console access *below* the operating system. Compromise is unrecoverable by anything running in-cluster.

```
┌────────────────────────────────────────────────────────────┐
│ namespace: fleetwarden-creds   (NetworkPolicy: deny all     │
│                                 except oob-broker egress)   │
│                                                             │
│   OOBCredential ──▶ secrets.Provider ──▶ [K8s Secret |      │
│                                           ESO | Vault]      │
└──────────────────────────┬─────────────────────────────────┘
                           │ never crosses this line as plaintext
                  ┌────────▼────────┐
                  │   oob.Broker    │  · mints short-TTL Redfish sessions
                  │                 │  · returns a session handle, not creds
                  │                 │  · per-rack scoping
                  │                 │  · every mint is audited
                  └────────┬────────┘
                           │ session handle (TTL 15m, single-rack scope)
                  ┌────────▼────────┐
                  │  rollout / oob  │
                  │  consumers      │
                  └─────────────────┘
```

Design rules, all enforceable and all testable:

- Credentials never appear in a CRD spec, a status, a log line, or an Event. `OOBCredential` holds a *reference*, never a value.
- Consumers receive a session handle with a 15-minute TTL scoped to a single rack. A compromised rollout controller cannot reach the whole fleet, and cannot reach it tomorrow.
- The broker is the only component with egress to the management network, enforced by NetworkPolicy.
- Every session mint is audited with requester identity, rack scope, and purpose.
- Rotation is a first-class operation, not a manual runbook.

### 8.4 Transport and identity
mTLS between manager and webhooks via cert-manager with automatic rotation. Projected bound ServiceAccount tokens (audience-scoped, short-lived) rather than legacy secret-mounted tokens. Redfish endpoints verified against a pinned CA bundle per rack.

### 8.5 Supply chain
Container images signed with cosign and verified at admission by a policy controller. SBOM generated at build. Reproducible builds via pinned toolchain. Dependencies vendored and reviewed.

### 8.6 Audit
Every disruptive action produces a structured record: `{timestamp, actor, intent, node, triggering signal, budget state before/after, decision, outcome}`. Records are hash-chained so tampering is detectable. Mirrored to Kubernetes Events for operator visibility and exported as OpenTelemetry spans for latency analysis of the full signal→action path.

**Break-glass:** a documented, audited path to bypass budget admission during a genuine emergency, requiring a distinct RBAC verb and emitting a high-severity alert on use. Systems without an escape hatch get bypassed in undocumented ways; better to design the hatch.

---

## 9. Observability and SLOs

| Metric | Type | Purpose |
|---|---|---|
| `fleetwarden_nodes_by_state{class,state}` | gauge | fleet composition |
| `fleetwarden_budget_consumed_ratio{class}` | gauge | headroom — primary alert |
| `fleetwarden_intents_total{module,decision}` | counter | admission behavior |
| `fleetwarden_signal_to_action_seconds` | histogram | detection latency |
| `fleetwarden_rollout_wave_duration_seconds` | histogram | rollout progress |
| `fleetwarden_oob_sessions_minted_total{rack}` | counter | credential use |
| `fleetwarden_reconcile_errors_total{controller}` | counter | operator health |

**SLOs:** critical signal to first action p95 < 60s · budget accounting accuracy 100% (any overshoot is a sev) · zero unaudited disruptive actions.

---

## 10. Weekend implementation plan

The vertical slice: one complete path through every architectural layer.

### Saturday morning — surface
- `kubebuilder init` + scaffold all six APIs, `make generate manifests`
- Full type definitions for all CRDs — the API is the design artifact
- RBAC manifests from §8.1, split ClusterRole/Role

### Saturday afternoon — the spine
- `internal/signal`: `Source` interface + Prometheus and NodeCondition implementations + sliding window
- `internal/budget`: the accountant, quotas, in-flight ledger — **fully unit tested**, no cluster required
- `internal/lifecycle`: state machine — **fully unit tested**, table-driven

### Sunday morning — wire it up
- `nodehealth_controller.go`: reconcile → evaluate → intent → admit → act
- Drain via eviction API with PDB awareness
- `internal/audit`: structured log + Events
- Prometheus metrics from §9

### Sunday afternoon — make it real
- `cmd/fakeredfish`: simulated BMC fleet (§11)
- `kind` + `kwok` demo: 500 nodes, chaos injection, budget cap visibly holding
- One validating webhook (rollout blast radius) — proves the admission layer
- Grafana dashboard, demo GIF

### Explicitly deferred, and say so in the README
`rollout` waves, `maintenance` scheduling, `sizing` resize, the OOB broker's Vault backend. Types and interfaces exist; implementations are stubs returning `ErrNotImplemented`.

> A README section titled **"Implemented vs. designed"** with an honest table is a strength, not an apology. It demonstrates you can scope a large system and stage delivery — which is precisely the "independently scope complex, multi-month projects" line in every staff-level JD you are targeting.

---

## 11. Simulation harness

This is what makes the whole thing modelable on a laptop, and it is where your background shows up in a way nobody can fake.

**`kwok`** provides 500 nodes without kubelets — full Node objects, schedulable, with conditions you control.

**`cmd/fakeredfish`** is a Redfish-conformant HTTP server simulating a rack of BMCs: service root, session auth, `SimpleUpdate` for firmware, power actions, and injectable faults — link degradation, thermal events, session timeouts, and the genuinely nasty ones like a BMC that accepts a firmware update and then never reboots.

Almost nobody writing Kubernetes operators can build a credible BMC simulator, because it requires knowing what BMCs actually do when they misbehave. You spent two years finding out. Make the fault list specific and slightly opinionated — that detail is the strongest signal in the repository.

**`fwctl chaos`** drives scenarios: `--scenario rack-thermal-event`, `--scenario firmware-brick --nodes 12`, `--scenario correlated-ecc --rack r14`.

---

## 12. Deferred / future work

Multi-cluster federation · predictive failure models from historical signals (your Soter anomaly-detection background is the natural bridge) · power-aware scheduling against the `powerEnvelopeWatts` field · integration with cluster-autoscaler rather than reimplementing pool sizing · NVLink topology-aware drain ordering.

---

## 13. Alternatives considered

**Node Problem Detector + Descheduler.** Rejected: NPD detects but does not remediate progressively, and neither component has fleet-wide budget awareness. The gap this system fills is precisely the coordination layer between them.

**Cluster API `MachineHealthCheck`.** Rejected as insufficient: MHC remediates by deleting and reprovisioning machines. For accelerator fleets where a node represents six figures of capital and hours of bring-up, quarantine-and-repair beats destroy-and-recreate. Worth naming this explicitly — it shows you know the ecosystem and chose deliberately rather than in ignorance.

**Extending an existing operator.** Rejected: the budget-as-admission-point architecture is a cross-cutting concern that cannot be bolted onto a system that did not start with it.

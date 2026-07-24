# Build status and first-run checklist

## Verified in development

These packages compile, pass `go vet`, and pass their full test suite under
`-race`. They carry no Kubernetes dependencies by design, which is what makes
them testable without an API server.

| Package | Coverage | Tests |
|---|---|---|
| `internal/budget` | 100.0% | 12 |
| `internal/fleet` | 100.0% | 7 |
| `internal/lifecycle` | 92.1% | 4 (18 table cases) |
| `internal/signal` | 90.1% | 10 |
| `internal/audit` | 88.9% | 7 |
| `internal/policy` | 88.1% | 7 |
| `cmd/fakeredfish` | 77.6% | 12 |

```bash
make test-core   # ~2s, no cluster required
```

## Written, not yet executed

The Kubernetes-coupled layer is written against controller-runtime v0.17 and
Kubernetes 1.29 APIs but has not had its first run. Review it as a design
artifact until `make test` passes locally.

- `api/v1alpha1/types.go`
- `controllers/nodehealth_controller.go`
- `webhooks/validating/policy_webhook.go`
- `cmd/manager/main.go`

## First-run checklist

Work through these in order. Items 1-3 will almost certainly require small
fixes; that is normal for generated-code projects on first build.

1. **Fetch dependencies.**
   ```bash
   make deps
   ```

2. **Generate deepcopy methods.** The API types are annotated with
   `+kubebuilder:object:root=true` but `zz_generated.deepcopy.go` does not
   exist yet, so the types do not satisfy `runtime.Object` until this runs.
   ```bash
   make generate
   ```

3. **Generate CRDs, RBAC and webhook manifests.**
   ```bash
   make manifests
   ```
   This writes `config/crd/*.yaml`. The hand-written `config/rbac/role.yaml`
   is deliberately narrower than what controller-gen produces from the
   `+kubebuilder:rbac` markers — reconcile the two by hand rather than
   letting codegen widen the grants. The narrow version is the one to keep.

4. **Build.**
   ```bash
   make build && make build-sim
   ```
   Likely fixes here: an unused import, or a controller-runtime v0.17 vs
   v0.18 signature difference on `SubResource("eviction").Create`.

5. **Run the full suite.**
   ```bash
   make test
   ```

6. **Demo.**
   ```bash
   make demo && make run
   ```

## Known gaps in the M1 slice

- `prometheusSource.Fetch` and `oobSource.Fetch` return `ErrNotImplemented`.
  The `NodeCondition` and `NodeLabel` sources are complete, and
  `./hack/chaos.sh drift` exercises the full path end to end.
- `hack/chaos.sh` writes annotations that the sample policy's Prometheus
  signals do not read. Either implement `prometheusSource`, or add a
  `NodeCondition` signal keyed on the injected annotation, to make the ECC
  scenario fire without Prometheus.
- No `envtest` suite yet. `test/envtest/` is scaffolded but empty; the
  reconciler's first test should assert that a denied intent leaves the node
  untouched and emits a `BudgetExhausted` event.

#!/usr/bin/env bash
# Inject failure signals into simulated nodes.
#
# Usage:
#   ./hack/chaos.sh ecc 40            # 40 nodes report uncorrectable ECC
#   ./hack/chaos.sh pcie 12
#   ./hack/chaos.sh drift 200         # firmware drift, non-disruptive
#   ./hack/chaos.sh rack r14          # correlated whole-rack failure
#   ./hack/chaos.sh heal              # clear everything
set -euo pipefail
MODE="${1:-ecc}"
ARG="${2:-10}"

nodes_random() {
  kubectl get nodes -l fleet.elbadry.dev/class=h100-sxm-8x \
    -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | shuf | head -n "$1"
}

case "$MODE" in
  ecc)
    for n in $(nodes_random "$ARG"); do
      kubectl annotate node "$n" fleet.elbadry.dev/sim-ecc=9 --overwrite >/dev/null
    done
    echo "injected ECC storm on ${ARG} nodes"
    ;;
  pcie)
    for n in $(nodes_random "$ARG"); do
      kubectl annotate node "$n" fleet.elbadry.dev/sim-pcie=degraded --overwrite >/dev/null
    done
    echo "injected PCIe link degradation on ${ARG} nodes"
    ;;
  drift)
    for n in $(nodes_random "$ARG"); do
      kubectl label node "$n" fleet.elbadry.dev/bmc-version=2.3.9 --overwrite >/dev/null
    done
    echo "injected firmware drift on ${ARG} nodes"
    ;;
  rack)
    # The correlated failure random canary sampling misses.
    for n in $(kubectl get nodes -l "topology.kubernetes.io/rack=${ARG}" \
        -o jsonpath='{.items[*].metadata.name}'); do
      kubectl annotate node "$n" fleet.elbadry.dev/sim-ecc=9 --overwrite >/dev/null
    done
    echo "injected correlated failure across rack ${ARG}"
    ;;
  heal)
    kubectl annotate nodes --all \
      fleet.elbadry.dev/sim-ecc- fleet.elbadry.dev/sim-pcie- >/dev/null 2>&1 || true
    kubectl label nodes --all fleet.elbadry.dev/bmc-version=2.4.1 --overwrite >/dev/null
    echo "cleared all injected faults"
    ;;
  *)
    echo "unknown mode: $MODE" >&2; exit 1 ;;
esac

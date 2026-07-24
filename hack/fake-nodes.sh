#!/usr/bin/env bash
# Create N kwok-simulated GPU nodes. kwok gives us a fleet without kubelets,
# which is what makes a 500-node demo possible on a laptop.
set -euo pipefail
COUNT="${1:-500}"
RACKS=20

for i in $(seq 0 $((COUNT-1))); do
  rack="r$((i % RACKS))"
  cat <<EOF
apiVersion: v1
kind: Node
metadata:
  name: gpu-${i}
  labels:
    type: kwok
    fleet.elbadry.dev/sku: h100-sxm-8x
    fleet.elbadry.dev/class: h100-sxm-8x
    fleet.elbadry.dev/bmc-version: "2.4.1"
    topology.kubernetes.io/rack: ${rack}
  annotations:
    fleet.elbadry.dev/state: Healthy
spec:
  taints:
    - key: kwok.x-k8s.io/node
      value: fake
      effect: NoSchedule
status:
  allocatable:
    cpu: "112"
    memory: 2Ti
    nvidia.com/gpu: "8"
  capacity:
    cpu: "112"
    memory: 2Ti
    nvidia.com/gpu: "8"
  conditions:
    - type: Ready
      status: "True"
      reason: KubeletReady
  nodeInfo:
    kubeletVersion: fake
---
EOF
done | kubectl apply -f -

echo "created ${COUNT} simulated nodes across ${RACKS} racks"

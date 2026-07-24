#!/usr/bin/env bash
# Live view of fleet state and budget consumption.
set -euo pipefail
watch -n2 '
echo "=== NODE STATES ==="
kubectl get nodes -l fleet.elbadry.dev/class=h100-sxm-8x \
  -o jsonpath="{range .items[*]}{.metadata.annotations.fleet\.elbadry\.dev/state}{\"\n\"}{end}" \
  | sort | uniq -c | sort -rn
echo ""
echo "=== BUDGET ==="
kubectl get nodeclass -o custom-columns=CLASS:.metadata.name,TOTAL:.status.total,HEALTHY:.status.healthy,UNAVAIL:.status.unavailable,BUDGET:.status.budgetConsumed
echo ""
echo "=== RECENT EVENTS ==="
kubectl get events --field-selector reason=BudgetExhausted,reason=StateTransition \
  --sort-by=.lastTimestamp 2>/dev/null | tail -12
'

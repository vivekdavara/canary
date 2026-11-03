#!/usr/bin/env bash
# What CI prints when the end-to-end tests fail (make e2e-diagnose): the rollouts, the Deployments and pods, the
# Events, the operator's, Prometheus's and the demo app's logs, and Prometheus's targets. A failed test leaves its
# namespace in place for this.
set -u
k=${KUBECTL:-kubectl}
section() { printf '\n===== %s\n' "$*"; }

section "nodes"; $k get nodes -o wide
section "rollouts"; $k get canaryrollouts -A -o yaml
section "deployments, replica sets and pods"; $k get deployments,replicasets,pods -A -o wide
section "events"; $k events -A
section "operator logs"; $k -n canary-system logs -l app.kubernetes.io/name=canary --all-containers --prefix --tail=-1
section "prometheus logs"; $k -n canary-e2e logs deployment/prometheus --tail=200
section "prometheus targets"; curl -s --max-time 5 http://127.0.0.1:9090/api/v1/targets | head -c 30000; echo
for ns in $($k get namespaces -o name | sed -n 's|^namespace/\(e2e-.*\)|\1|p'); do
	section "logs in $ns"; $k -n "$ns" logs -l app --all-containers --prefix --tail=50 --max-log-requests=20
done
exit 0

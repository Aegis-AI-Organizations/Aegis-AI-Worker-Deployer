#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOPOLOGY_FILE="$ROOT_DIR/examples/sandbox-topology.vulnerable-webapp.json"
SCAN_ID="smoke-sqli-001"
NAMESPACE=""
ENDPOINT=""
CLEANUP="false"
WAIT_SECONDS="90"

usage() {
  cat <<'EOF'
Usage: scripts/sandbox_smoke.sh [options]

Validates a sandbox topology fixture and inspects a deployed sandbox namespace.

Options:
  --scan-id ID        Scan id used by the deployer. Default: smoke-sqli-001
  --namespace NAME    Sandbox namespace. Default: aegis-war-room-<scan-id>
  --endpoint URL      Endpoint returned by CreateSandbox. If omitted, the script
                      tries http://vulnerable-webapp.<namespace>.svc.cluster.local:80
  --topology PATH     Topology JSON to validate.
  --wait SECONDS      Wait timeout for pods and deployments. Default: 90
  --cleanup           Delete the sandbox namespace after inspection.
  -h, --help          Show this help.

Examples:
  scripts/sandbox_smoke.sh --scan-id smoke-sqli-001
  scripts/sandbox_smoke.sh --namespace aegis-war-room-scan-123 --endpoint http://svc-scan-123.aegis-war-room-scan-123.svc.cluster.local:80
  scripts/sandbox_smoke.sh --scan-id smoke-sqli-001 --cleanup
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --scan-id)
      SCAN_ID="$2"
      shift 2
      ;;
    --namespace)
      NAMESPACE="$2"
      shift 2
      ;;
    --endpoint)
      ENDPOINT="$2"
      shift 2
      ;;
    --topology)
      TOPOLOGY_FILE="$2"
      shift 2
      ;;
    --wait)
      WAIT_SECONDS="$2"
      shift 2
      ;;
    --cleanup)
      CLEANUP="true"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$NAMESPACE" ]]; then
  NAMESPACE="aegis-war-room-$SCAN_ID"
fi

if [[ -z "$ENDPOINT" ]]; then
  ENDPOINT="http://vulnerable-webapp.$NAMESPACE.svc.cluster.local:80"
fi

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Missing required command: %s\n' "$1" >&2
    exit 127
  fi
}

section() {
  printf '\n== %s ==\n' "$1"
}

require_command python3
require_command kubectl

section "Validate topology JSON"
python3 -m json.tool "$TOPOLOGY_FILE" >/dev/null
printf 'Topology OK: %s\n' "$TOPOLOGY_FILE"

section "Namespace"
kubectl get namespace "$NAMESPACE"

section "Network policies"
kubectl get networkpolicy -n "$NAMESPACE" -o wide || true

section "Services"
kubectl get service -n "$NAMESPACE" -o wide

section "Deployments"
kubectl get deployment -n "$NAMESPACE" -o wide
kubectl wait --for=condition=Available deployment --all -n "$NAMESPACE" --timeout="${WAIT_SECONDS}s" || true

section "Pods"
kubectl get pods -n "$NAMESPACE" -o wide
kubectl wait --for=condition=Ready pod --all -n "$NAMESPACE" --timeout="${WAIT_SECONDS}s" || true
kubectl get pods -n "$NAMESPACE" -o wide

section "Endpoint smoke"
printf 'Testing endpoint from inside namespace: %s\n' "$ENDPOINT"
kubectl run "aegis-sandbox-smoke-$SCAN_ID" \
  -n "$NAMESPACE" \
  --rm \
  -i \
  --restart=Never \
  --image=curlimages/curl:8.12.1 \
  --command -- sh -c "curl -ksS --max-time 10 '$ENDPOINT/health' || curl -ksS --max-time 10 '$ENDPOINT'"

section "Recent events"
kubectl get events -n "$NAMESPACE" --sort-by=.lastTimestamp || true

if [[ "$CLEANUP" == "true" ]]; then
  section "Cleanup"
  case "$NAMESPACE" in
    aegis-war-room-*)
      kubectl delete namespace "$NAMESPACE"
      ;;
    *)
      printf 'Refusing to delete non-sandbox namespace: %s\n' "$NAMESPACE" >&2
      exit 3
      ;;
  esac
fi

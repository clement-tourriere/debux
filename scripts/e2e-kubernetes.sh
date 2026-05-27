#!/usr/bin/env bash
set -euo pipefail

DEBUX_BIN="${DEBUX_BIN:-./bin/debux}"
DEBUX_IMAGE="${DEBUX_IMAGE:-ghcr.io/clement-tourriere/debux:latest}"
DEBUX_PULL_POLICY="${DEBUX_PULL_POLICY:-IfNotPresent}"
NAMESPACE="${DEBUX_E2E_NAMESPACE:-debux-e2e-default}"
POD="${DEBUX_E2E_POD:-web}"

case "$NAMESPACE" in
  debux-e2e-*) ;;
  *)
    if [[ "${DEBUX_E2E_ALLOW_ARBITRARY_NAMESPACE:-}" != "1" ]]; then
      echo "error: refusing to manage namespace '$NAMESPACE'" >&2
      echo "Set DEBUX_E2E_NAMESPACE to a debux-e2e-* name or DEBUX_E2E_ALLOW_ARBITRARY_NAMESPACE=1 to override." >&2
      exit 2
    fi
    ;;
esac

if [[ ! -x "$DEBUX_BIN" ]]; then
  echo "Building debux test binary at $DEBUX_BIN"
  CGO_ENABLED=0 go build -o "$DEBUX_BIN" ./cmd/debux
fi

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "error: $1 is required" >&2; exit 1; }
}
need kubectl

cleanup() {
  kubectl delete namespace "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup

kubectl create namespace "$NAMESPACE" >/dev/null
kubectl run "$POD" -n "$NAMESPACE" --image=nginx:alpine --restart=Never --port=80 >/dev/null
kubectl wait -n "$NAMESPACE" --for=condition=Ready "pod/$POD" --timeout=180s >/dev/null

echo "Running one-shot debux command against $NAMESPACE/$POD"
"$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" \
  --image "$DEBUX_IMAGE" \
  --fresh \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- curl -fsS http://127.0.0.1 >/dev/null

echo "Checking restricted profile startup"
"$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" \
  --image "$DEBUX_IMAGE" \
  --profile=restricted \
  --fresh \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- id >/dev/null

echo "Kubernetes e2e passed"

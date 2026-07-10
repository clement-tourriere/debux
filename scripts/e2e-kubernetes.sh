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
output="$("$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" \
  --image "$DEBUX_IMAGE" \
  --fresh \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- sh -c 'curl -fsS http://127.0.0.1 >/dev/null && echo debux-e2e-k8s-ok')"
printf '%s\n' "$output"
if ! grep -q 'debux-e2e-k8s-ok' <<<"$output"; then
  echo "error: expected sentinel 'debux-e2e-k8s-ok' in debux output" >&2
  exit 1
fi

echo "Checking debux list shows the ephemeral session"
list_output="$("$DEBUX_BIN" list "k8s://$NAMESPACE/")"
if ! grep -q "$POD" <<<"$list_output"; then
  echo "error: debux list does not show the session for $NAMESPACE/$POD:" >&2
  printf '%s\n' "$list_output" >&2
  exit 1
fi

echo "Checking ephemeral session kill"
"$DEBUX_BIN" kill "k8s://$NAMESPACE/$POD"

echo "Checking restricted profile startup"
"$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" \
  --image "$DEBUX_IMAGE" \
  --profile=restricted \
  --fresh \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- id >/dev/null

echo "Running --copy --keep session"
copy_output="$("$DEBUX_BIN" "k8s://$NAMESPACE/$POD" \
  --image "$DEBUX_IMAGE" \
  --copy --keep --ttl=30m \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- sh -c 'echo debux-e2e-copy-ok')"
printf '%s\n' "$copy_output"
if ! grep -q 'debux-e2e-copy-ok' <<<"$copy_output"; then
  echo "error: expected sentinel 'debux-e2e-copy-ok' in copy-mode output" >&2
  exit 1
fi

echo "Checking the kept copy pod exists with its TTL deadline"
copy_pod="$(kubectl get pods -n "$NAMESPACE" \
  -l "app.kubernetes.io/managed-by=debux,debux.clement-tourriere/mode=copy" \
  -o jsonpath='{.items[0].metadata.name}')"
if [[ -z "$copy_pod" ]]; then
  echo "error: --keep did not leave a copy pod behind" >&2
  exit 1
fi
deadline="$(kubectl get pod -n "$NAMESPACE" "$copy_pod" -o jsonpath='{.spec.activeDeadlineSeconds}')"
if [[ "$deadline" != "1800" ]]; then
  echo "error: copy pod activeDeadlineSeconds = '$deadline', want 1800 (--ttl=30m)" >&2
  exit 1
fi

echo "Checking debux kill deletes the kept copy pod"
"$DEBUX_BIN" kill "k8s://$NAMESPACE/$copy_pod"
for _ in {1..30}; do
  if ! kubectl get pod -n "$NAMESPACE" "$copy_pod" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
if kubectl get pod -n "$NAMESPACE" "$copy_pod" >/dev/null 2>&1; then
  echo "error: copy pod $copy_pod still exists after debux kill" >&2
  exit 1
fi

echo "Kubernetes e2e passed"

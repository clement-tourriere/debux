#!/usr/bin/env bash
set -euo pipefail

DEBUX_BIN="${DEBUX_BIN:-./bin/debux}"
DEBUX_IMAGE="${DEBUX_IMAGE:-ghcr.io/clement-tourriere/debux:latest}"
DEBUX_PULL_POLICY="${DEBUX_PULL_POLICY:-IfNotPresent}"
NAMESPACE="${DEBUX_E2E_NAMESPACE:-debux-e2e-default}"
POD="${DEBUX_E2E_POD:-web}"
created_namespace=0
export DEBUX_CONFIG=/dev/null

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
  if [[ "$created_namespace" == 1 ]]; then
    kubectl --request-timeout=10s delete namespace "$NAMESPACE" --ignore-not-found --timeout=30s >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

# Refuse an existing namespace rather than deleting unrelated workloads.
kubectl create namespace "$NAMESPACE" >/dev/null
created_namespace=1
# Namespace creation can race the service-account controller on a fresh kind
# cluster. Wait for its default account instead of failing pod admission.
for _ in {1..30}; do
  if kubectl --request-timeout=5s get serviceaccount default -n "$NAMESPACE" >/dev/null 2>&1; then break; fi
  sleep 1
done
kubectl --request-timeout=5s get serviceaccount default -n "$NAMESPACE" >/dev/null
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

echo "Checking option changes without --fresh"
for value in first second; do
  output="$("$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" --image "$DEBUX_IMAGE" \
    --pull-policy "$DEBUX_PULL_POLICY" --env "E2E_OPTION=$value" -- printenv E2E_OPTION)"
  [[ "$output" == "$value" ]] || { echo "error: reused stale environment" >&2; exit 1; }
done

echo "Checking command failures propagate"
status=0
"$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" --image "$DEBUX_IMAGE" --pull-policy "$DEBUX_PULL_POLICY" -- sh -c 'exit 42' || status=$?
[[ "$status" == 42 ]] || { echo "error: expected exit 42, got $status" >&2; exit 1; }

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

if [[ "${DEBUX_E2E_TOOL_TESTS:-0}" == 1 ]]; then
  echo "Checking restricted --tools installs and reuse in Kubernetes"
  previous_sessions=""
  for attempt in first reuse; do
    "$DEBUX_BIN" "k8s://$NAMESPACE/$POD/$POD" --image "$DEBUX_IMAGE" \
      --profile=restricted --pull-policy "$DEBUX_PULL_POLICY" \
      --tools yq@4.53.6 -- yq --version | grep -q v4.53.6
    current_sessions="$(kubectl get pod -n "$NAMESPACE" "$POD" -o jsonpath='{.spec.ephemeralContainers[*].name}')"
    if [[ "$attempt" == reuse && "$current_sessions" != "$previous_sessions" ]]; then
      echo 'error: identical --tools options did not reuse the session' >&2
      exit 1
    fi
    previous_sessions="$current_sessions"
  done
fi

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

echo "Checking shared-PID pods are rejected rather than exposing the sandbox root"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: shared-pid
  namespace: $NAMESPACE
spec:
  shareProcessNamespace: true
  containers:
    - name: app
      image: nginx:alpine
EOF
kubectl wait -n "$NAMESPACE" --for=condition=Ready pod/shared-pid --timeout=180s >/dev/null
if output="$("$DEBUX_BIN" "k8s://$NAMESPACE/shared-pid/app" --image "$DEBUX_IMAGE" --pull-policy "$DEBUX_PULL_POLICY" -- true 2>&1)"; then
  echo "error: shared-PID targeting unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'PID namespace' <<<"$output"

echo "Kubernetes e2e passed"

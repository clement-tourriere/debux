#!/usr/bin/env bash
set -euo pipefail

DEBUX_BIN="${DEBUX_BIN:-./bin/debux}"
DEBUX_IMAGE="${DEBUX_IMAGE:-ghcr.io/clement-tourriere/debux:latest}"
DEBUX_PULL_POLICY="${DEBUX_PULL_POLICY:-IfNotPresent}"
TARGET_NAME="${DEBUX_TARGET_NAME:-debux-e2e-docker-$$}"

if [[ ! -x "$DEBUX_BIN" ]]; then
  echo "Building debux test binary at $DEBUX_BIN"
  CGO_ENABLED=0 go build -o "$DEBUX_BIN" ./cmd/debux
fi

cleanup() {
  docker rm -f "$TARGET_NAME" "debux-$TARGET_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup

echo "Starting Docker target $TARGET_NAME"
docker run -d --name "$TARGET_NAME" nginx:alpine >/dev/null

# Wait for nginx inside the target to be healthy before debugging it.
healthy=0
for _ in {1..60}; do
  if docker exec "$TARGET_NAME" wget -qO- http://127.0.0.1 >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 1
done
if [[ "$healthy" != 1 ]]; then
  echo "error: target $TARGET_NAME did not become healthy within 60s" >&2
  docker logs "$TARGET_NAME" >&2 || true
  exit 1
fi

echo "Running one-shot debux command against $TARGET_NAME"
output="$("$DEBUX_BIN" "docker://$TARGET_NAME" \
  --image "$DEBUX_IMAGE" \
  --fresh \
  --pull-policy "$DEBUX_PULL_POLICY" \
  -- sh -lc 'test -d "$DEBUX_TARGET_ROOT" && curl -fsS http://127.0.0.1 >/dev/null && echo debux-e2e-docker-ok')"
printf '%s\n' "$output"
if ! grep -q 'debux-e2e-docker-ok' <<<"$output"; then
  echo "error: expected sentinel 'debux-e2e-docker-ok' in debux output" >&2
  exit 1
fi
# Status/progress messages must stay on stderr so scripts can consume stdout.
if [[ "$(tr -d '[:space:]' <<<"$output")" != "debux-e2e-docker-ok" ]]; then
  echo "error: one-shot stdout contains more than the command output:" >&2
  printf '%s\n' "$output" >&2
  exit 1
fi

echo "Checking debux list shows the active session"
list_output="$("$DEBUX_BIN" list "docker://")"
if ! grep -q "$TARGET_NAME" <<<"$list_output"; then
  echo "error: debux list does not show the session for $TARGET_NAME:" >&2
  printf '%s\n' "$list_output" >&2
  exit 1
fi

echo "Checking debux cp out of the target"
cp_dir="$(mktemp -d)"
trap 'rm -rf "$cp_dir"; cleanup' EXIT INT TERM
"$DEBUX_BIN" cp "$TARGET_NAME:/etc/nginx/nginx.conf" "$cp_dir/nginx.conf"
if [[ ! -s "$cp_dir/nginx.conf" ]]; then
  echo "error: debux cp produced an empty or missing file" >&2
  exit 1
fi

echo "Checking debux cp into the target"
echo debux-e2e-cp-ok > "$cp_dir/probe.txt"
"$DEBUX_BIN" cp "$cp_dir/probe.txt" "$TARGET_NAME:/tmp"
docker exec "$TARGET_NAME" cat /tmp/probe.txt | grep -q debux-e2e-cp-ok

echo "Checking session cleanup command"
"$DEBUX_BIN" kill "docker://$TARGET_NAME"
if [[ -n "$(docker ps -q -f "name=^debux-$TARGET_NAME$")" ]]; then
  echo "error: debux kill exited 0 but the sidecar container is still running" >&2
  exit 1
fi

echo "Docker e2e passed"

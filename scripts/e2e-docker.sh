#!/usr/bin/env bash
set -euo pipefail

DEBUX_BIN="${DEBUX_BIN:-./bin/debux}"
DEBUX_IMAGE="${DEBUX_IMAGE:-ghcr.io/clement-tourriere/debux:latest}"
DEBUX_PULL_POLICY="${DEBUX_PULL_POLICY:-IfNotPresent}"
TARGET_NAME="${DEBUX_TARGET_NAME:-debux-e2e-docker-$$}"
TARGET_ID=""
TEST_IMAGE=""
TEST_GUARD_ID=""
TEST_IMAGE_SUFFIX=""
export DEBUX_CONFIG=/dev/null

if [[ ! -x "$DEBUX_BIN" ]]; then
  echo "Building debux test binary at $DEBUX_BIN"
  CGO_ENABLED=0 go build -o "$DEBUX_BIN" ./cmd/debux
fi

cleanup() {
  local id
  if [[ -n "$TARGET_ID" ]]; then
    while IFS= read -r id; do
      [[ -z "$id" ]] || docker rm -f "$id" >/dev/null 2>&1 || true
    done < <(docker ps -aq --filter "label=debux.clement-tourriere/target-id=$TARGET_ID" --filter "label=app.kubernetes.io/managed-by=debux")
    docker rm -f "$TARGET_ID" >/dev/null 2>&1 || true
  fi
  # Only stores belonging to this invocation's unique image identity. Never
  # remove the caller's existing image/tools, or force removal of a busy volume.
  if [[ -n "$TEST_IMAGE_SUFFIX" ]]; then
    while IFS= read -r id; do
      case "$id" in
        debux-tools-"$TEST_IMAGE_SUFFIX"-*|debux-nix-store-"$TEST_IMAGE_SUFFIX"|debux-nix-var-"$TEST_IMAGE_SUFFIX")
          docker volume rm "$id" >/dev/null 2>&1 || true;;
      esac
    done < <(docker volume ls --filter label=managed-by=debux --format '{{.Name}}')
  fi
  [[ -z "$TEST_GUARD_ID" ]] || docker rm -f "$TEST_GUARD_ID" >/dev/null 2>&1 || true
  [[ -z "$TEST_IMAGE" ]] || docker image rm "$TEST_IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Tool stores are image-scoped, not target-scoped. A unique label-only image
# isolates installs from the developer's stores and makes repeated tests safe.
case "$DEBUX_PULL_POLICY" in
  Always) docker pull "$DEBUX_IMAGE" >/dev/null;;
  IfNotPresent) docker image inspect "$DEBUX_IMAGE" >/dev/null 2>&1 || docker pull "$DEBUX_IMAGE" >/dev/null;;
  Never) docker image inspect "$DEBUX_IMAGE" >/dev/null;;
  *) echo "Invalid pull policy: $DEBUX_PULL_POLICY" >&2; exit 2;;
esac
TEST_IMAGE="debux:e2e-$$-$RANDOM"
docker build --pull=false --build-arg "BASE=$DEBUX_IMAGE" \
  --label "io.debux.e2e=$TEST_IMAGE" -t "$TEST_IMAGE" - <<'DOCKERFILE'
ARG BASE=scratch
FROM ${BASE}
DOCKERFILE
DEBUX_IMAGE="$(docker image inspect "$TEST_IMAGE" --format '{{.Id}}')"
TEST_IMAGE_SUFFIX="${DEBUX_IMAGE#sha256:}"
[[ "$TEST_IMAGE_SUFFIX" =~ ^[a-f0-9]{64}$ ]] || { TEST_IMAGE_SUFFIX=""; exit 1; }
TEST_IMAGE_SUFFIX="${TEST_IMAGE_SUFFIX:0:12}"
DEBUX_PULL_POLICY=Never
# Use the immutable ID and keep it referenced throughout session replacement.
# Local image GC must not remove the fixture between two sidecar creations.
TEST_GUARD_ID="$(docker run -d --network none --read-only --cap-drop=ALL \
  --security-opt no-new-privileges:true --entrypoint /bin/sh "$DEBUX_IMAGE" -c 'exec sleep 1800')"

echo "Starting Docker target $TARGET_NAME"
# Never delete an existing target merely because its name matches the fixture.
TARGET_ID="$(docker run -d --name "$TARGET_NAME" nginx:alpine)"

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
# Expand DEBUX_TARGET_ROOT in the container, not the test harness.
# shellcheck disable=SC2016
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

if [[ "$(docker image inspect "$DEBUX_IMAGE" --format '{{index .Config.Labels "io.debux.toolbox"}}')" == mise-v1 ]]; then
  echo "Checking --tools, fresh-session persistence, and user-separated stores"
  output="$("$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" \
    --pull-policy "$DEBUX_PULL_POLICY" --tools yq@4.53.6 -- yq --version)"
  grep -q 'v4.53.6' <<< "$output"
  "$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --fresh \
    --pull-policy "$DEBUX_PULL_POLICY" -- yq --version | grep -q v4.53.6
  # A different UID must not inherit the root user's mutable tool profile.
  "$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --fresh --user 65534 \
    --pull-policy "$DEBUX_PULL_POLICY" -- sh -c 'if command -v yq; then exit 1; fi'
  "$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --user 65534 \
    --pull-policy "$DEBUX_PULL_POLICY" --tools yq@4.53.6 -- yq --version | grep -q v4.53.6
  "$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --fresh \
    --pull-policy "$DEBUX_PULL_POLICY" -- yq --version | grep -q v4.53.6
fi

echo "Checking option changes without --fresh"
for value in first second; do
  output="$("$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" \
    --pull-policy "$DEBUX_PULL_POLICY" --env "E2E_OPTION=$value" -- printenv E2E_OPTION)"
  [[ "$output" == "$value" ]] || { echo "error: reused stale environment" >&2; exit 1; }
done
"$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --pull-policy "$DEBUX_PULL_POLICY" --privileged -- true
"$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --pull-policy "$DEBUX_PULL_POLICY" -- true
[[ "$(docker inspect "debux-$TARGET_NAME" --format '{{.HostConfig.Privileged}}')" == false ]] || { echo "error: reused privileged session" >&2; exit 1; }

echo "Checking command failures propagate"
status=0
"$DEBUX_BIN" "docker://$TARGET_NAME" --image "$DEBUX_IMAGE" --pull-policy "$DEBUX_PULL_POLICY" -- sh -c 'exit 42' || status=$?
[[ "$status" == 42 ]] || { echo "error: expected exit 42, got $status" >&2; exit 1; }

echo "Checking debux list shows the active session"
list_output="$("$DEBUX_BIN" list "docker://")"
if ! grep -q "$TARGET_NAME" <<<"$list_output"; then
  echo "error: debux list does not show the session for $TARGET_NAME:" >&2
  printf '%s\n' "$list_output" >&2
  exit 1
fi

echo "Checking debux cp out of the target"
cp_dir="$(mktemp -d)"
trap 'rm -rf "$cp_dir"; cleanup' EXIT
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

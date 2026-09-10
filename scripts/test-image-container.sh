#!/usr/bin/env bash
# Executed only inside an isolated candidate-image container by test-image.sh.
set -euo pipefail
mkdir -p "$HOME"
# shellcheck disable=SC1091
source /etc/debux/environment.sh
export DEBUX_TARGET_ROOT=/dev/null DEBUX_TARGET_ENVIRON=/dev/null DEBUX_TARGET_CWD_LINK=/dev/null

# Exercise the actual baked libcurl, including during the network-isolated
# resume phase. Use ggshield's bundled Python; no extra runtime is installed.
/usr/share/ggshield/bin/python -I /debux-curl-cookies.py

if [[ "${1:-}" == resume ]]; then
  # The host recreates this container with --network none and a fresh /tmp.
  # Both unversioned --tools-style installs and explicit pins reuse the volume.
  dctl install ggshield dbcrust jq gcc
  ggshield --version | grep '^ggshield, version '
  dbcrust sqlite://"$DEBUX_DATA_DIR/.dbcrust-smoke.sqlite" --no-input -o json \
    -c 'SELECT 42 AS debux_dbcrust_smoke' | /usr/bin/jq -e '.rows == [["42"]]'
  sha256sum -c "$DEBUX_DATA_DIR/.dbcrust-hashes"
  cmp "$DEBUX_DATA_DIR/.dbcrust-stats" <(stat -c '%Y %s' /usr/local/bin/dbcrust)
  dctl install yq
  dctl install yq@4.53.6
  yq --version | grep 'v4.53.6'
  grep -q debux-history-smoke "$DEBUX_DATA_DIR/.zsh_history"
  dctl remove yq
  hash -r
  if command -v yq; then echo 'Removed command still available' >&2; exit 1; fi
  jq --version
  exit 0
fi

for tool in bash zsh curl wget ps ip ss tcpdump jq tar bsdtar chroot gzip awk grep sed \
  nsenter which htop strace ltrace gdb tmux ggshield dbcrust dbc vim less file tree dig nmap ncat nc socat ssh openssl git mise dctl \
  cc c++ make cmake ninja pkg-config autoconf automake libtool bison flex; do
  command -v "$tool"
done
if command -v nix; then echo 'Unexpected Nix runtime' >&2; exit 1; fi
test ! -L /etc/passwd
test ! -L /etc/group
test "$(awk -F: '$3 == 65534 { n++ } END { print n }' /etc/passwd)" = 1
/bin/sh -c 'test -n "$BASHPID"'
mise --version | grep '2026.9.3'
test "$(mise settings get system_deps)" = warn
test "$(mise settings get system_packages.sudo)" = false
test "$(mise settings get exec_auto_install)" = false
test ! -e /usr/lib/libbfd.a
test -z "$(find /usr -xdev -type f \( -perm -4000 -o -perm -2000 \) -print)"
curl --fail --silent --show-error --max-time 60 https://mise.jdx.dev >/dev/null
dctl list | grep 'curl-'
ggshield --version | grep '^ggshield, version '
# Query an isolated local database; no database server, source build or network
# is needed. The same file and baked binary are reused in the offline phase.
dbcrust --version | grep '0.37.2'
dbc --version | grep '0.37.2'
dctl install dbcrust dbc | grep 'provided by the image'
sha256sum -c /usr/share/debux/dbcrust.sha256
sha256sum /usr/local/bin/dbcrust > "$DEBUX_DATA_DIR/.dbcrust-hashes"
stat -c '%Y %s' /usr/local/bin/dbcrust > "$DEBUX_DATA_DIR/.dbcrust-stats"
touch "$DEBUX_DATA_DIR/.dbcrust-smoke.sqlite"
dbcrust sqlite://"$DEBUX_DATA_DIR/.dbcrust-smoke.sqlite" --no-input -o json \
  -c 'SELECT 42 AS debux_dbcrust_smoke' | /usr/bin/jq -e '.rows == [["42"]]'
dctl install --help | grep 'Usage: dctl install'
dctl help install | grep 'Usage: dctl install'
zsh -df -c 'source /etc/debux/zshrc; [[ $HISTFILE == /var/lib/debux/.zsh_history && -n ${functions[_zsh_highlight]:-} && -n ${functions[_zsh_autosuggest_start]:-} ]]'
sha256sum /usr/bin/jq /usr/bin/curl /usr/local/bin/mise /usr/bin/gcc /usr/bin/ggshield > /tmp/base-hashes
# Stripping dependency DWARF must retain static linking and LTO functionality.
printf '%s\n' '#include <stdio.h>' 'int main(void) { puts("static-c-ok"); }' > /tmp/probe.c
cc -static /tmp/probe.c -o "$DEBUX_DATA_DIR/.probe-c"
"$DEBUX_DATA_DIR/.probe-c" | grep static-c-ok
printf '%s\n' '#include <iostream>' 'int main() { std::cout << "static-cxx-ok"; }' > /tmp/probe.cc
c++ -flto -static-libstdc++ -static-libgcc /tmp/probe.cc -o "$DEBUX_DATA_DIR/.probe-cxx"
"$DEBUX_DATA_DIR/.probe-cxx" | grep static-cxx-ok
rm "$DEBUX_DATA_DIR/.probe-c" "$DEBUX_DATA_DIR/.probe-cxx"

dctl install yq@4.53.6 jq@1.8.2
yq --version | grep 'v4.53.6'
jq --version | grep 'jq-1.8.2'
dctl list | grep yq
dctl search yq | grep yq
# Removing an override must restore the baked command, not remove base files.
dctl remove jq
hash -r
jq --version
sha256sum -c /tmp/base-hashes

# Familiar Python CLI names work without asking users to bootstrap uv/pipx.
dctl install httpie@3.2.4
http --version | grep '3.2.4'
dctl search httpie | grep pipx:httpie
# Bare ggshield is baked; version overrides use PyPI, not Aqua's x64-only build.
dctl install ggshield | grep 'provided by the image'
dctl install ggshield@1.54.0
test "$(command -v ggshield)" = "$DEBUX_TOOL_BIN/ggshield"
ggshield --version | grep '1.54.0'
dctl install ggshield | grep 'saved tool store'
dctl remove ggshield
hash -r
test "$(command -v ggshield)" != "$DEBUX_TOOL_BIN/ggshield"
ggshield --version | grep '^ggshield, version '
sha256sum -c /tmp/base-hashes
dctl remove httpie uv
hash -r
if command -v http; then echo 'Removed Python CLI still available' >&2; exit 1; fi

mkdir -p /tmp/untrusted-target
printf '%s\n' 'invalid TOML: do not load target settings' > /tmp/untrusted-target/.miserc.toml
printf '%s\n' '[tools]' 'yq = "0.0.0"' > /tmp/untrusted-target/mise.toml
printf '%s\n' 'yq 0.0.0' > /tmp/untrusted-target/.tool-versions
printf '%s\n' 'debux: preserved-cwd' > /tmp/untrusted-target/value.yaml
cd /tmp/untrusted-target
# Both discovery and execution must ignore target configuration; execution must
# still see the original cwd. This also covers mise's early .miserc discovery.
test "$(yq -r .debux value.yaml)" = preserved-cwd
dctl list | grep yq
if dctl install --postinstall=bad; then echo 'Accepted a tool-option injection' >&2; exit 1; fi
if dctl install debux-nonexistent-security-test-893154; then echo 'False install success' >&2; exit 1; fi
if [[ "$(id -u)" == 65534 ]]; then
  dctl install nodejs@24.20.0 python3@3.14.7
  node --version | grep 'v24.20.0'
  python3 --version | grep '3.14.7'
  python3 -c 'import ssl; print(ssl.OPENSSL_VERSION)'
  # Language runtimes still work from the hostile target directory too.
  test "$(node -p 'process.cwd()')" = /tmp/untrusted-target
  dctl remove nodejs python3
  sha256sum -c /tmp/base-hashes
fi
printf '%s\n' debux-history-smoke >> "$DEBUX_DATA_DIR/.zsh_history"

#!/usr/bin/env sh
set -eu

# The entire installer runs from main() (invoked at the very bottom) so that a
# truncated download via `curl ... | sh` can never execute a partial script.
main() {
  repo="${DEBUX_REPO:-clement-tourriere/debux}"
  binary="debux"
  version="${DEBUX_VERSION:-latest}"
  bin_dir="${DEBUX_INSTALL_DIR:-${HOME}/.local/bin}"

  usage() {
    cat <<EOF
Install debux from GitHub Releases.

Release assets are verified against checksums.txt. If cosign is installed and
signature assets are present, checksums.txt is verified before use.

Usage:
  curl -fsSL https://raw.githubusercontent.com/${repo}/main/install.sh | sh
  curl -fsSL https://raw.githubusercontent.com/${repo}/main/install.sh | sh -s -- --version v1.2.3

Options:
  -b, --bin-dir DIR     Install directory (default: ${bin_dir})
  -v, --version VERSION Version to install: latest, v1.2.3, or 1.2.3 (default: ${version})
  -h, --help            Show this help

Environment:
  DEBUX_INSTALL_DIR     Install directory
  DEBUX_VERSION         Version to install
  DEBUX_REPO            GitHub repo, owner/name (default: ${repo})
  DEBUX_ALLOW_SOURCE_BUILD=1
                         Allow fallback to go install if a release asset is missing
EOF
  }

  while [ "$#" -gt 0 ]; do
    case "$1" in
      -b|--bin-dir)
        [ "$#" -ge 2 ] || { echo "error: $1 requires a directory" >&2; exit 2; }
        bin_dir="$2"
        shift 2
        ;;
      -v|--version)
        [ "$#" -ge 2 ] || { echo "error: $1 requires a version" >&2; exit 2; }
        version="$2"
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "error: unknown option: $1" >&2
        usage >&2
        exit 2
        ;;
    esac
  done

  need() {
    if ! command -v "$1" >/dev/null 2>&1; then
      echo "error: required command not found: $1" >&2
      exit 1
    fi
  }

  need uname
  need tar

  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$os" in
    darwin|linux) ;;
    *) echo "error: unsupported OS: $os (supported: linux, darwin)" >&2; exit 1 ;;
  esac

  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) echo "error: unsupported architecture: $arch (supported: amd64, arm64)" >&2; exit 1 ;;
  esac

  download_status="000"
  if command -v curl >/dev/null 2>&1; then
    download() { curl -fsSL "$1" -o "$2"; }
    fetch_release_json() { curl -fsSL -H 'Accept: application/json' "$1"; }
    # Download $1 to $2 and record the final HTTP status code in
    # download_status, so callers can tell a missing asset (404) apart from a
    # transient network or server failure.
    download_archive() {
      if download_status="$(curl -fsSL -o "$2" -w '%{http_code}' "$1" 2>/dev/null)"; then
        return 0
      fi
      [ -n "$download_status" ] || download_status="000"
      return 1
    }
  elif command -v wget >/dev/null 2>&1; then
    download() { wget -qO "$2" "$1"; }
    fetch_release_json() { wget -qO- --header='Accept: application/json' "$1"; }
    download_archive() {
      if wget -qO "$2" "$1"; then
        download_status="200"
        return 0
      fi
      # wget does not expose HTTP status codes directly; probe the URL again
      # and look for a 404 in the diagnostics to tell a missing asset apart
      # from a transient failure.
      if wget --spider "$1" 2>&1 | grep -q ' 404 '; then
        download_status="404"
      else
        download_status="000"
      fi
      return 1
    }
  else
    echo "error: curl or wget is required" >&2
    exit 1
  fi

  archive="${binary}_${os}_${arch}.tar.gz"
  if [ "$version" = "latest" ]; then
    # Resolve "latest" to a concrete tag up front so the download, the cosign
    # identity check, and any source-build fallback all pin the same release.
    tag="$(fetch_release_json "https://github.com/${repo}/releases/latest" 2>/dev/null \
      | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      | head -n 1)"
    if [ -z "$tag" ]; then
      echo "error: could not resolve the latest release tag for ${repo}; retry or pass --version vX.Y.Z" >&2
      exit 1
    fi
    case "$tag" in
      v[0-9]*) ;;
      *) echo "error: unexpected latest release tag: ${tag}" >&2; exit 1 ;;
    esac
    echo "Resolved latest release: ${tag}"
  else
    case "$version" in
      v*) tag="$version" ;;
      *) tag="v$version" ;;
    esac
  fi
  base_url="https://github.com/${repo}/releases/download/${tag}"
  go_ref="$tag"

  tmp="$(mktemp -d 2>/dev/null || mktemp -d -t debux)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  archive_path="$tmp/$archive"
  checksums_path="$tmp/checksums.txt"
  checksums_sig_path="$tmp/checksums.txt.sig"
  checksums_cert_path="$tmp/checksums.txt.pem"

  verify_checksums_signature() {
    if ! command -v cosign >/dev/null 2>&1; then
      return 0
    fi
    if ! download "${base_url}/checksums.txt.sig" "$checksums_sig_path" 2>/dev/null; then
      echo "warning: cosign is installed but checksums.txt.sig was not found; continuing with checksum-only verification" >&2
      return 0
    fi
    if ! download "${base_url}/checksums.txt.pem" "$checksums_cert_path" 2>/dev/null; then
      echo "warning: cosign is installed but checksums.txt.pem was not found; continuing with checksum-only verification" >&2
      return 0
    fi
    # The release tag is always resolved by now (including for "latest"), so
    # pin the signing identity to the exact tag in every case.
    cosign verify-blob \
      --certificate "$checksums_cert_path" \
      --signature "$checksums_sig_path" \
      --certificate-identity "https://github.com/${repo}/.github/workflows/release.yml@refs/tags/${tag}" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      "$checksums_path" >/dev/null
  }

  # Downloads via curl/wget are normally not quarantined, but clear xattrs and
  # ad-hoc sign on macOS when available to keep local development installs smooth.
  prepare_macos_binary() {
    [ "$os" = "darwin" ] || return 0
    if command -v xattr >/dev/null 2>&1; then
      xattr -c "$1" 2>/dev/null || true
    fi
    if command -v codesign >/dev/null 2>&1; then
      codesign --force --sign - "$1" >/dev/null 2>&1 || true
    fi
  }

  echo "Installing debux ${tag} for ${os}/${arch}..."
  installed_from_source=0
  install_path="$bin_dir/$binary"

  echo "Downloading ${archive}"
  if ! download_archive "${base_url}/${archive}" "$archive_path"; then
    if [ "$download_status" = "404" ]; then
      echo "warning: release asset not found (HTTP 404): ${base_url}/${archive}" >&2
      if [ "${DEBUX_ALLOW_SOURCE_BUILD:-}" = "1" ] && command -v go >/dev/null 2>&1; then
        echo "No release asset found; falling back to source build with Go (${go_ref})." >&2
        mkdir -p "$bin_dir"
        GOBIN="$bin_dir" go install "github.com/${repo}/cmd/debux@${go_ref}"
        installed_from_source=1
      else
        echo "error: no release asset exists for ${os}/${arch}." >&2
        echo "Set DEBUX_ALLOW_SOURCE_BUILD=1 to opt into building from source with Go." >&2
        exit 1
      fi
    else
      echo "error: download failed (HTTP status ${download_status}): ${base_url}/${archive}" >&2
      echo "This looks like a transient network or server error; please retry the install." >&2
      exit 1
    fi
  fi

  if [ "$installed_from_source" -eq 0 ]; then
    if ! download "${base_url}/checksums.txt" "$checksums_path" 2>/dev/null; then
      echo "error: checksums.txt not found for ${tag}; refusing to install unverifiable release asset" >&2
      exit 1
    fi
    verify_checksums_signature
    expected="$(awk -v file="$archive" '$2 == file {print $1; exit}' "$checksums_path")"
    if [ -z "$expected" ]; then
      echo "error: checksum for ${archive} not found in checksums.txt" >&2
      exit 1
    fi
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "$archive_path" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      actual="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
    else
      echo "error: sha256sum or shasum is required to verify release checksums" >&2
      exit 1
    fi
    if [ "$actual" != "$expected" ]; then
      echo "error: checksum mismatch for ${archive}" >&2
      echo "expected: $expected" >&2
      echo "actual:   $actual" >&2
      exit 1
    fi
  fi

  if [ "$installed_from_source" -eq 0 ]; then
    # The leading "(" in the case patterns keeps the parentheses balanced;
    # bash 3.2 (macOS /bin/sh) cannot parse unbalanced ")" inside "$(...)".
    archive_member="$({ tar -tzf "$archive_path" 2>/dev/null || true; } | while IFS= read -r member; do
      case "$member" in
        ("$binary"|*/"$binary")
          case "$member" in
            (/*|../*|*/../*) continue ;;
          esac
          printf '%s\n' "$member"
          break
          ;;
      esac
    done)"
    if [ -z "$archive_member" ]; then
      echo "error: archive did not contain ${binary}" >&2
      exit 1
    fi
    tar -xzf "$archive_path" -C "$tmp" "$archive_member"
    if [ -L "$tmp/$archive_member" ]; then
      echo "error: archive member ${archive_member} is a symlink" >&2
      exit 1
    fi
    found="$tmp/$archive_member"
    if [ ! -f "$found" ]; then
      echo "error: archive member ${archive_member} did not extract to a regular file" >&2
      exit 1
    fi

    prepare_macos_binary "$found"
    # Smoke-test the extracted binary before it replaces anything, so a broken
    # download never clobbers a working install.
    if ! "$found" --help >/dev/null 2>&1; then
      echo "error: downloaded binary failed smoke test: ${found} --help" >&2
      echo "Keeping any existing install at ${install_path}." >&2
      exit 1
    fi

    mkdir -p "$bin_dir"
    install_tmp="$(mktemp "${bin_dir}/.debux.XXXXXX")" || {
      echo "error: failed to create temporary install file in ${bin_dir}" >&2
      exit 1
    }
    cp "$found" "$install_tmp" || { rm -f "$install_tmp"; exit 1; }
    chmod 0755 "$install_tmp" || { rm -f "$install_tmp"; exit 1; }
    mv "$install_tmp" "$install_path" || { rm -f "$install_tmp"; exit 1; }
  else
    prepare_macos_binary "$install_path"
    if ! "$install_path" --help >/dev/null 2>&1; then
      echo "error: installed binary failed smoke test: ${install_path} --help" >&2
      exit 1
    fi
  fi

  echo "Installed debux to ${install_path}"
  case ":${PATH}:" in
    *":${bin_dir}:"*) ;;
    *)
      echo ""
      echo "Add this directory to your PATH:"
      echo "  export PATH=\"${bin_dir}:\$PATH\""
      ;;
  esac

  echo ""
  echo "Try it:"
  echo "  debux docker://"
}

main "$@"

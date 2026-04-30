#!/usr/bin/env sh
set -eu

repo="${DEBUX_REPO:-clement-tourriere/debux}"
binary="debux"
version="${DEBUX_VERSION:-latest}"
bin_dir="${DEBUX_INSTALL_DIR:-${HOME}/.local/bin}"

usage() {
  cat <<EOF
Install debux from GitHub Releases.

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

archive="${binary}_${os}_${arch}.tar.gz"
if [ "$version" = "latest" ]; then
  base_url="https://github.com/${repo}/releases/latest/download"
else
  case "$version" in
    v*) tag="$version" ;;
    *) tag="v$version" ;;
  esac
  base_url="https://github.com/${repo}/releases/download/${tag}"
fi

if command -v curl >/dev/null 2>&1; then
  download() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  download() { wget -qO "$2" "$1"; }
else
  echo "error: curl or wget is required" >&2
  exit 1
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t debux)"
trap 'rm -rf "$tmp"' EXIT INT TERM

archive_path="$tmp/$archive"
checksums_path="$tmp/checksums.txt"

echo "Installing debux ${version} for ${os}/${arch}..."
echo "Downloading ${archive}"
download "${base_url}/${archive}" "$archive_path" || {
  echo "error: failed to download ${base_url}/${archive}" >&2
  echo "Make sure a debux GitHub Release exists for ${os}/${arch}." >&2
  exit 1
}

if download "${base_url}/checksums.txt" "$checksums_path" 2>/dev/null; then
  expected="$(awk -v file="$archive" '$2 == file {print $1; exit}' "$checksums_path")"
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "$archive_path" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      actual="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
    else
      actual=""
      echo "warning: sha256sum/shasum not found; skipping checksum verification" >&2
    fi
    if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
      echo "error: checksum mismatch for ${archive}" >&2
      echo "expected: $expected" >&2
      echo "actual:   $actual" >&2
      exit 1
    fi
  else
    echo "warning: checksum for ${archive} not found; skipping verification" >&2
  fi
else
  echo "warning: checksums.txt not found; skipping verification" >&2
fi

tar -xzf "$archive_path" -C "$tmp"
if [ ! -f "$tmp/$binary" ]; then
  found="$(find "$tmp" -type f -name "$binary" 2>/dev/null | head -n 1 || true)"
  if [ -z "$found" ]; then
    echo "error: archive did not contain ${binary}" >&2
    exit 1
  fi
else
  found="$tmp/$binary"
fi

mkdir -p "$bin_dir"
install_path="$bin_dir/$binary"
rm -f "$install_path"
cp "$found" "$install_path"
chmod 0755 "$install_path"

# Downloads via curl/wget are normally not quarantined, but clear xattrs and
# ad-hoc sign on macOS when available to keep local development installs smooth.
if [ "$os" = "darwin" ]; then
  if command -v xattr >/dev/null 2>&1; then
    xattr -c "$install_path" 2>/dev/null || true
  fi
  if command -v codesign >/dev/null 2>&1; then
    codesign --force --sign - "$install_path" >/dev/null 2>&1 || true
  fi
fi

if ! "$install_path" --help >/dev/null 2>&1; then
  echo "error: installed binary failed smoke test: ${install_path} --help" >&2
  exit 1
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

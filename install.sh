#!/usr/bin/env sh
# pxe-beacon installer.
#
# Quick install (latest release, current directory):
#   curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh
#
# Pin a version:
#   curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh -s -- --version v0.1.2
#
# Install to /usr/local/bin (requires sudo password):
#   curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh -s -- --dir /usr/local/bin
#
# Env overrides (same effect as flags):
#   PXE_BEACON_VERSION  default: latest
#   PXE_BEACON_DIR      default: . (current directory)
#   PXE_BEACON_REPO     default: venkatamutyala/pxe-beacon

set -eu

REPO="${PXE_BEACON_REPO:-venkatamutyala/pxe-beacon}"
VERSION="${PXE_BEACON_VERSION:-latest}"
INSTALL_DIR="${PXE_BEACON_DIR:-.}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --dir)     INSTALL_DIR="$2"; shift 2 ;;
    --repo)    REPO="$2"; shift 2 ;;
    -h|--help)
      cat <<'EOF'
pxe-beacon installer

Quick install (latest release, current directory):
  curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh

Pin a version:
  curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh -s -- --version v0.1.2

Install to /usr/local/bin (requires sudo password):
  curl -sSL https://raw.githubusercontent.com/venkatamutyala/pxe-beacon/main/install.sh | sh -s -- --dir /usr/local/bin

Env overrides (same effect as flags):
  PXE_BEACON_VERSION  default: latest
  PXE_BEACON_DIR      default: . (current directory)
  PXE_BEACON_REPO     default: venkatamutyala/pxe-beacon
EOF
      exit 0 ;;
    *) echo "install.sh: unknown arg: $1 (try --help)" >&2; exit 2 ;;
  esac
done

# ---- detect OS/arch and map to release-asset name ----
case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "install.sh: unsupported OS: $(uname -s) (Linux and macOS only)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "install.sh: unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
case "$OS-$ARCH" in
  linux-amd64|linux-arm64|darwin-arm64) ;;
  darwin-amd64)
    echo "install.sh: darwin/amd64 (Intel Mac) is not published." >&2
    echo "             Build from source: git clone https://github.com/$REPO && cd pxe-beacon && make" >&2
    exit 1 ;;
  *)
    echo "install.sh: no prebuilt artifact for ${OS}/${ARCH}" >&2
    exit 1 ;;
esac

ASSET="pxe-beacon-${OS}-${ARCH}"
if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi

# ---- downloader + sha256 verifier (work on both macOS and Linux) ----
if command -v curl >/dev/null 2>&1; then
  dl() { curl --fail --silent --show-error --location --output "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  dl() { wget --quiet --output-document="$2" "$1"; }
else
  echo "install.sh: need curl or wget" >&2; exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  echo "install.sh: need sha256sum or shasum" >&2; exit 1
fi

# ---- download to a tempdir, verify, atomically move into place ----
TMP="$(mktemp -d 2>/dev/null || mktemp -d -t pxe-beacon)"
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "install.sh: ${OS}/${ARCH} ${VERSION} -> ${INSTALL_DIR}/pxe-beacon"
dl "${BASE}/${ASSET}"     "$TMP/$ASSET"     || { echo "install.sh: download failed: ${BASE}/${ASSET}" >&2; exit 1; }
dl "${BASE}/SHA256SUMS"   "$TMP/SHA256SUMS" || { echo "install.sh: download failed: ${BASE}/SHA256SUMS" >&2; exit 1; }

expected="$(awk -v a="$ASSET" '$2 == a { print $1 }' "$TMP/SHA256SUMS")"
[ -n "$expected" ] || { echo "install.sh: no checksum entry for $ASSET in SHA256SUMS" >&2; exit 1; }
actual="$(sha "$TMP/$ASSET")"
if [ "$expected" != "$actual" ]; then
  echo "install.sh: SHA256 mismatch for $ASSET" >&2
  echo "  expected: $expected" >&2
  echo "  actual:   $actual"   >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR" 2>/dev/null || true
TARGET="$INSTALL_DIR/pxe-beacon"
if [ -w "$INSTALL_DIR" ] || { [ ! -e "$INSTALL_DIR" ] && mkdir -p "$INSTALL_DIR" 2>/dev/null; }; then
  mv "$TMP/$ASSET" "$TARGET"
  chmod +x "$TARGET"
elif command -v sudo >/dev/null 2>&1; then
  echo "install.sh: $INSTALL_DIR not writable; using sudo"
  sudo mv "$TMP/$ASSET" "$TARGET"
  sudo chmod +x "$TARGET"
else
  echo "install.sh: $INSTALL_DIR not writable and sudo unavailable" >&2; exit 1
fi

# Strip macOS Gatekeeper quarantine xattr so `./pxe-beacon` runs without
# the "Apple could not verify..." dialog. Silent if attr isn't set or
# we're not on macOS.
if [ "$OS" = "darwin" ]; then
  xattr -d com.apple.quarantine "$TARGET" 2>/dev/null || true
fi

echo "install.sh: installed:"
ls -lh "$TARGET"
"$TARGET" -version || echo "install.sh: (installed but '$TARGET -version' failed — perhaps wrong arch?)"

# PATH hint if we put it somewhere not on PATH and it's not just `.`.
case ":${PATH:-}:" in
  *:"$INSTALL_DIR":*|*:".":*) ;;
  *)
    if [ "$INSTALL_DIR" != "." ] && [ "$INSTALL_DIR" != "$(pwd)" ]; then
      echo "install.sh: note: $INSTALL_DIR is not on your PATH — invoke $TARGET directly or add it"
    fi
  ;;
esac

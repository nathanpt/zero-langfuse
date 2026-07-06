#!/usr/bin/env bash
set -euo pipefail

ZLF_REPO="${ZLF_REPO:-nathanpt/zero-langfuse}"
ZLF_VERSION="${ZLF_VERSION:-latest}"
ZLF_INSTALL_DIR="${ZLF_INSTALL_DIR:-$HOME/.local/bin}"
ZLF_GITHUB_API="${ZLF_GITHUB_API:-https://api.github.com}"
ZLF_GITHUB_BASE_URL="${ZLF_GITHUB_BASE_URL:-https://github.com}"

usage() {
  cat <<'EOF'
Install zero-langfuse from GitHub Releases.

Usage:
  scripts/install.sh [--version <version>] [--repo <owner/repo>] [--install-dir <path>]

Environment:
  ZLF_VERSION          Release version or tag. Defaults to latest.
  ZLF_REPO             GitHub repository. Defaults to nathanpt/zero-langfuse.
  ZLF_INSTALL_DIR      Directory for the zero-langfuse binary. Defaults to ~/.local/bin.
  ZLF_GITHUB_API       GitHub API base URL. Defaults to https://api.github.com.
  ZLF_GITHUB_BASE_URL  GitHub web base URL. Defaults to https://github.com.
EOF
}

fail() {
  echo "zero-langfuse install: $*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || fail "--version requires a value"
      ZLF_VERSION="$2"
      shift 2
      ;;
    --repo)
      [ "$#" -ge 2 ] || fail "--repo requires a value"
      ZLF_REPO="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || fail "--install-dir requires a value"
      ZLF_INSTALL_DIR="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

download() {
  local url="$1"
  local output="$2"

  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --show-error --silent "$url" --output "$output"
  elif command -v wget >/dev/null 2>&1; then
    wget --quiet "$url" --output-document="$output"
  else
    fail "curl or wget is required"
  fi
}

download_json() {
  local url="$1"
  local output="$2"

  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --show-error --silent --header 'Accept: application/vnd.github+json' "$url" --output "$output"
  elif command -v wget >/dev/null 2>&1; then
    wget --quiet --header='Accept: application/vnd.github+json' "$url" --output-document="$output"
  else
    fail "curl or wget is required"
  fi
}

detect_platform() {
  case "$(uname -s)" in
    Linux) echo "linux" ;;
    Darwin) echo "darwin" ;;
    *) fail "unsupported platform: $(uname -s)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

latest_tag() {
  local metadata_file="$1"
  local api_url="${ZLF_GITHUB_API%/}/repos/${ZLF_REPO}/releases/latest"
  local tag

  download_json "$api_url" "$metadata_file"
  tag="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$metadata_file" | head -n 1)"
  [ -n "$tag" ] || fail "could not read tag_name from $api_url"
  echo "$tag"
}

# Verify the archive against a single line pulled from the combined checksums
# file: <hash>  <archive_name>. Computes the archive's sha256 and compares.
verify_checksum() {
  local archive_path="$1"
  local expected="$2"

  if command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
  elif command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$archive_path" | awk '{print $1}')"
  else
    fail "shasum or sha256sum is required"
  fi

  [ "$actual" = "$expected" ] || fail "checksum mismatch for $(basename "$archive_path"): expected $expected, got $actual"
}

find_extracted_entry() {
  local root="$1"
  local name="$2"
  local kind="$3"
  local candidate

  if [ "$kind" = "file" ] && [ -f "$root/$name" ]; then
    echo "$root/$name"
    return 0
  fi

  for candidate in "$root"/*/"$name"; do
    if [ "$kind" = "file" ] && [ -f "$candidate" ]; then
      echo "$candidate"
      return 0
    fi
  done

  return 1
}

find_extracted_binary() {
  find_extracted_entry "$1" "zero-langfuse" "file"
}

need_command uname
need_command sed
need_command tar
need_command mktemp

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/zero-langfuse-install.XXXXXX")"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

if [ "$ZLF_VERSION" = "latest" ]; then
  tag="$(latest_tag "$tmp_dir/latest.json")"
else
  case "$ZLF_VERSION" in
    v*) tag="$ZLF_VERSION" ;;
    *) tag="v$ZLF_VERSION" ;;
  esac
fi

version="${tag#v}"
platform="$(detect_platform)"
arch="$(detect_arch)"
archive_name="zero-langfuse_${version}_${platform}_${arch}.tar.gz"
checksum_name="zero-langfuse_${version}_checksums.txt"
release_url="${ZLF_GITHUB_BASE_URL%/}/${ZLF_REPO}/releases/download/${tag}"
archive_path="$tmp_dir/$archive_name"
checksum_path="$tmp_dir/$checksum_name"
extract_dir="$tmp_dir/extract"

echo "Installing zero-langfuse ${tag} for ${platform}-${arch}"
download "${release_url}/${archive_name}" "$archive_path"
download "${release_url}/${checksum_name}" "$checksum_path"

# The checksums file is one combined sha256sum listing: <hash>  <filename>.
# Pull the single line for our archive and take its hash.
expected_hash="$(grep -E "[[:space:]]${archive_name}\$" "$checksum_path" | awk '{print $1}' | head -n 1)"
[ -n "$expected_hash" ] || fail "no checksum entry for ${archive_name} in ${checksum_name}"
verify_checksum "$archive_path" "$expected_hash"

mkdir -p "$extract_dir"
tar -xzf "$archive_path" -C "$extract_dir"

binary_path="$(find_extracted_binary "$extract_dir")" || fail "release archive did not contain zero-langfuse"

mkdir -p "$ZLF_INSTALL_DIR"
cp "$binary_path" "$ZLF_INSTALL_DIR/zero-langfuse"
chmod 755 "$ZLF_INSTALL_DIR/zero-langfuse"

echo "Installed $ZLF_INSTALL_DIR/zero-langfuse"

case ":$PATH:" in
  *":$ZLF_INSTALL_DIR:"*) ;;
  *) echo "Add $ZLF_INSTALL_DIR to PATH to run zero-langfuse from any directory." ;;
esac

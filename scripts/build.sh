#!/usr/bin/env bash
# Build the web console and embed it in a deployable MITMRouter binary.
set -Eeuo pipefail

readonly ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly WEB_DIR="$ROOT_DIR/web"
readonly WEB_DIST="$ROOT_DIR/internal/webui/dist"

target_os=linux
target_arch=amd64
output=""
STRIP_BINARY=true

usage() {
  cat <<'EOF'
Usage: scripts/build.sh [options]

Build the Vue console, embed it in MITMRouter, and produce a static binary.

Options:
  --os OS               Target operating system (default: linux).
  --arch ARCH           Target architecture (default: amd64).
  -o, --output PATH     Binary output path (default: bin/mitmrouter-OS-ARCH).
  --debug               Keep Go debug symbols (omit -s -w).
  --skip-web            Reuse an existing internal/webui/dist build.
  -h, --help            Show this help.

Examples:
  ./scripts/build.sh
  ./scripts/build.sh --os linux --arch arm64
  ./scripts/build.sh --output ./mitmrouter --debug
EOF
}

build_web=true
require_option_value() {
  local option="$1"
  local value="${2-}"
  if [[ -z "$value" ]]; then
    printf 'Option %s requires a value.\n' "$option" >&2
    exit 2
  fi
}

while (($# > 0)); do
  case "$1" in
    --os)
      require_option_value "$1" "${2-}"
      target_os="$2"
      shift
      ;;
    --arch)
      require_option_value "$1" "${2-}"
      target_arch="$2"
      shift
      ;;
    -o|--output)
      require_option_value "$1" "${2-}"
      output="$2"
      shift
      ;;
    --debug) STRIP_BINARY=false ;;
    --skip-web) build_web=false ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'Unknown option: %s\n\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

if [[ -z "$output" ]]; then
  output="$ROOT_DIR/bin/mitmrouter-${target_os}-${target_arch}"
fi

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Required command not found: %s\n' "$1" >&2
    exit 127
  fi
}

require_command go
if "$build_web"; then
  require_command pnpm
fi

if "$build_web"; then
  printf '==> Installing locked web dependencies\n'
  pnpm --dir "$WEB_DIR" install --frozen-lockfile

  printf '==> Building web console\n'
  pnpm --dir "$WEB_DIR" run build
fi

if [[ ! -f "$WEB_DIST/index.html" ]]; then
  printf 'Web build is missing: %s\n' "$WEB_DIST/index.html" >&2
  printf 'Run without --skip-web to build it first.\n' >&2
  exit 1
fi

output_dir="$(dirname -- "$output")"
mkdir -p "$output_dir"
tmp_output="${output}.tmp.$$"
trap 'rm -f -- "$tmp_output"' EXIT

build_args=(-trimpath -o "$tmp_output")
if "$STRIP_BINARY"; then
  build_args+=(-ldflags='-s -w')
fi

printf '==> Building %s/%s binary: %s\n' "$target_os" "$target_arch" "$output"
(
  cd "$ROOT_DIR"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build "${build_args[@]}" ./cmd/mitmrouter
)
mv -f -- "$tmp_output" "$output"
trap - EXIT

printf '==> Build complete: %s\n' "$output"

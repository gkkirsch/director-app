#!/usr/bin/env bash
# Build the Director.app bundle with fleetview + satellites baked in.
#
# We pull every binary fleet runs (roster, camux, amux, fleetview) into
# Contents/MacOS/. main.go prepends that dir to PATH before spawning
# fleetview so a user who installed via the .app never needs ~/.local/bin.
#
# Usage:
#   ./build.sh                                 # rebuild fleetview from
#                                              # ../fleetview (and its
#                                              # web/ assets), then build
#                                              # the .app bundle
#   ./build.sh --no-rebuild                    # use whatever fleetview
#                                              # is currently on PATH
#                                              # (faster; trust caller)
#   FLEETVIEW=/abs/path … ./build.sh           # override the binary
#                                              # location entirely
set -euo pipefail

cd "$(dirname "$0")"

REBUILD_FLEETVIEW=1
INSTALL_TO_APPLICATIONS=0
for arg in "$@"; do
  case "$arg" in
    --no-rebuild) REBUILD_FLEETVIEW=0 ;;
    --install)    INSTALL_TO_APPLICATIONS=1 ;;
    *) echo "✗ unknown flag: $arg" >&2; exit 2 ;;
  esac
done

# Find the fleetview source tree. Prefer ../fleetview (the canonical
# monorepo layout); fall back to $FLEETVIEW_SRC for unusual setups.
fleetview_src() {
  if [[ -n "${FLEETVIEW_SRC:-}" ]]; then
    [[ -d "$FLEETVIEW_SRC" ]] || { echo "✗ FLEETVIEW_SRC=$FLEETVIEW_SRC not a directory" >&2; exit 1; }
    printf '%s\n' "$FLEETVIEW_SRC"; return
  fi
  if [[ -d "../fleetview" ]]; then
    (cd ../fleetview && pwd); return
  fi
  return 1
}

# Rebuild fleetview's frontend + binary from source. This is the
# "footgun-proof" path — caller never has to remember which order to
# rebuild things in. If ../fleetview isn't there, we fall through to
# the legacy "use whatever's on PATH" path with a warning.
if [[ "$REBUILD_FLEETVIEW" == "1" ]]; then
  if SRC="$(fleetview_src)"; then
    echo "→ rebuilding fleetview from $SRC"
    (cd "$SRC/web" && npm run build) >/dev/null
    (cd "$SRC" && go build -o "$HOME/.local/bin/fleetview" .)
    echo "    ✓ ~/.local/bin/fleetview"
  else
    echo "⚠ ../fleetview not found and FLEETVIEW_SRC not set — using whatever fleetview is on PATH" >&2
  fi
fi

resolve() {
  local name="$1" upper
  upper="$(printf '%s' "$name" | tr '[:lower:]' '[:upper:]')"
  local override="${!upper:-}"
  if [[ -n "$override" ]]; then
    [[ -x "$override" ]] || { echo "✗ $upper=$override not executable" >&2; exit 1; }
    printf '%s\n' "$override"
    return
  fi
  if path="$(command -v "$name" 2>/dev/null)"; then
    printf '%s\n' "$path"
    return
  fi
  echo "✗ $name not on PATH and $upper not set." >&2
  echo "  Install it first or pass $upper=/path/to/$name ./build.sh" >&2
  exit 1
}

FLEETVIEW="$(resolve fleetview)"
ROSTER="$(resolve roster)"
CAMUX="$(resolve camux)"
AMUX="$(resolve amux)"

echo "→ bundling:"
for bin in "$FLEETVIEW" "$ROSTER" "$CAMUX" "$AMUX"; do
  echo "    $(basename "$bin")  ($bin)"
done

~/go/bin/wails build

APP="build/bin/Director.app"
MACOS="$APP/Contents/MacOS"

for bin in "$FLEETVIEW" "$ROSTER" "$CAMUX" "$AMUX"; do
  install -m 0755 "$bin" "$MACOS/$(basename "$bin")"
done

# Re-sign so the codesign seal includes the new siblings. ad-hoc is
# fine for local distribution — public release will swap in a Dev ID.
codesign --force --deep --sign - "$APP" 2>/dev/null || true

echo
echo "✓ Built $APP"

if [[ "$INSTALL_TO_APPLICATIONS" == "1" ]]; then
  pkill -f "/Applications/Director.app/Contents/MacOS/" 2>/dev/null || true
  sleep 1
  rm -rf /Applications/Director.app
  cp -R "$APP" /Applications/Director.app
  echo "✓ Installed /Applications/Director.app"
else
  echo "  open $APP    or drag to /Applications"
  echo "  (or pass --install to copy into /Applications/ for you)"
fi

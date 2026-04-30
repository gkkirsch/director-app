#!/usr/bin/env bash
# Build the Director.app bundle with fleetview + satellites baked in.
#
# We pull every binary fleet runs (roster, camux, amux, fleetview) into
# Contents/MacOS/. main.go prepends that dir to PATH before spawning
# fleetview so a user who installed via the .app never needs ~/.local/bin.
#
# Usage:
#   ./build.sh                                 # picks up binaries from PATH
#   FLEETVIEW=/abs/path AMUX=/abs/path … ./build.sh
set -euo pipefail

cd "$(dirname "$0")"

# Resolve a binary by name. Honors $UPPERCASE override; otherwise falls
# back to PATH; otherwise exits with a clear message.
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

# Build the wrapper.
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
echo "  open $APP    or drag to /Applications"

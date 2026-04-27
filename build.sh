#!/usr/bin/env bash
# Build the FleetApp .app bundle with fleetview baked in.
#
# Usage:
#   ./build.sh                   # uses fleetview from PATH
#   FLEETVIEW=/abs/path ./build.sh
set -euo pipefail

cd "$(dirname "$0")"

FLEETVIEW="${FLEETVIEW:-}"
if [[ -z "$FLEETVIEW" ]]; then
  if ! FLEETVIEW="$(command -v fleetview)"; then
    echo "✗ fleetview not on PATH and FLEETVIEW not set." >&2
    echo "  Install fleetview first or pass FLEETVIEW=/path/to/fleetview ./build.sh" >&2
    exit 1
  fi
fi
if [[ ! -x "$FLEETVIEW" ]]; then
  echo "✗ FLEETVIEW=$FLEETVIEW is not executable." >&2
  exit 1
fi
echo "→ bundling $(basename "$FLEETVIEW") from $FLEETVIEW"

# Build the wrapper.
~/go/bin/wails build

# Bundle fleetview as a sibling of FleetApp inside the .app.
APP="build/bin/FleetApp.app"
DEST="$APP/Contents/MacOS/fleetview"
cp "$FLEETVIEW" "$DEST"
chmod +x "$DEST"

# Re-sign so the codesign seal includes the new sibling. ad-hoc is
# fine for local distribution — public release will swap in a Dev ID.
codesign --force --deep --sign - "$APP" 2>/dev/null || true

echo
echo "✓ Built $APP"
echo "  open $APP    or drag to /Applications"

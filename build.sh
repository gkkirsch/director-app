#!/usr/bin/env bash
# Build the Director.app bundle with director-server + satellites baked in.
#
# We pull every binary Director needs (roster, camux, amux, director-server)
# into Contents/MacOS/. main.go prepends that dir to PATH before spawning
# director-server so a user who installed via the .app never needs ~/.local/bin.
#
# Usage:
#   ./build.sh                                       # rebuild director-server
#                                                    # from ../fleetview (the
#                                                    # local source dir, repo
#                                                    # gkkirsch/director), then
#                                                    # build the .app bundle
#   ./build.sh --no-rebuild                          # use whatever
#                                                    # director-server is
#                                                    # currently on PATH
#   DIRECTOR_SERVER=/abs/path … ./build.sh           # override the binary
#                                                    # location entirely
set -euo pipefail

cd "$(dirname "$0")"

REBUILD_DIRECTOR_SERVER=1
INSTALL_TO_APPLICATIONS=0
for arg in "$@"; do
  case "$arg" in
    --no-rebuild) REBUILD_DIRECTOR_SERVER=0 ;;
    --install)    INSTALL_TO_APPLICATIONS=1 ;;
    *) echo "✗ unknown flag: $arg" >&2; exit 2 ;;
  esac
done

# Find the director-server source tree. Prefer ../director (the
# canonical monorepo layout); fall back to $DIRECTOR_SERVER_SRC for
# unusual setups.
director_server_src() {
  if [[ -n "${DIRECTOR_SERVER_SRC:-}" ]]; then
    [[ -d "$DIRECTOR_SERVER_SRC" ]] || { echo "✗ DIRECTOR_SERVER_SRC=$DIRECTOR_SERVER_SRC not a directory" >&2; exit 1; }
    printf '%s\n' "$DIRECTOR_SERVER_SRC"; return
  fi
  if [[ -d "../director" ]]; then
    (cd ../director && pwd); return
  fi
  return 1
}

# Rebuild director-server's frontend + binary from source. This is the
# "footgun-proof" path — caller never has to remember which order to
# rebuild things in. If ../fleetview isn't there, we fall through to
# the legacy "use whatever's on PATH" path with a warning.
if [[ "$REBUILD_DIRECTOR_SERVER" == "1" ]]; then
  if SRC="$(director_server_src)"; then
    echo "→ rebuilding director-server from $SRC"
    (cd "$SRC/web" && npm run build) >/dev/null
    (cd "$SRC" && go build -o "$HOME/.local/bin/director-server" .)
    echo "    ✓ ~/.local/bin/director-server"
  else
    echo "⚠ ../fleetview not found and DIRECTOR_SERVER_SRC not set — using whatever director-server is on PATH" >&2
  fi
fi

resolve() {
  local name="$1" upper
  # Hyphens aren't valid in shell var names. Translate them to
  # underscores so `director-server` → DIRECTOR_SERVER.
  upper="$(printf '%s' "$name" | tr '[:lower:]-' '[:upper:]_')"
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

DIRECTOR_SERVER="$(resolve director-server)"
ROSTER="$(resolve roster)"
CAMUX="$(resolve camux)"
AMUX="$(resolve amux)"

# Wails is incremental — without this, stale siblings from previous
# builds (e.g. an old `fleetview` before the binary rename) hang
# around in Contents/MacOS forever. Always start clean.
rm -rf build/bin

echo "→ bundling:"
for bin in "$DIRECTOR_SERVER" "$ROSTER" "$CAMUX" "$AMUX"; do
  echo "    $(basename "$bin")  ($bin)"
done

~/go/bin/wails build

APP="build/bin/Director.app"
MACOS="$APP/Contents/MacOS"

for bin in "$DIRECTOR_SERVER" "$ROSTER" "$CAMUX" "$AMUX"; do
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

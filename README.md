# Flow

Native macOS app for [fleet](https://github.com/gkkirsch/director) — a
Claude Code agent orchestrator. Wraps `director-server` in a Wails webview
and bundles `roster`, `camux`, `amux` next to it so the .app is
self-contained.

## Install

One-liner (latest release):

```bash
curl -fsSL https://github.com/gkkirsch/director-app/releases/latest/download/Director.app.zip -o /tmp/Director.zip \
  && ditto -xk /tmp/Director.zip /Applications/ \
  && xattr -dr com.apple.quarantine /Applications/Director.app \
  && open /Applications/Director.app
```

The `xattr` line is required because the build is ad-hoc signed (no
Apple Developer cert yet). Once we notarize, this drops to a clean
download.

On first launch Flow checks for `tmux`, `node`, `claude` (the Claude
Code CLI), and the keychain login. If anything's missing it shows a
setup screen with copy / install-in-Terminal buttons.

Apple Silicon only. Intel build comes when there's demand.

## Build from source

Prereqs: `go 1.22+`, `node 20+`, `wails` CLI, plus the four satellites
on `$PATH` (`roster`, `camux`, `amux`, `director-server` — install fleet
first via the [main installer](https://github.com/gkkirsch/director)).

```bash
./build.sh                                    # picks up satellites from PATH
FLEETVIEW=/abs/path AMUX=… ROSTER=… ./build.sh  # explicit overrides
```

Output: `build/bin/Director.app`. The script bundles each satellite into
`Director.app/Contents/MacOS/` and re-signs ad-hoc.

## Layout inside the bundle

```
Director.app/Contents/MacOS/
  Flow         ← Wails wrapper (this repo)
  fleetview    ← HTTP backend, serves the SPA + APIs
  roster       ← agent registry / spawn / notify
  camux        ← Claude Code TUI orchestration
  amux         ← tmux orchestration primitives
```

`Flow` prepends its bundle dir + the user's login-shell `PATH` to
`$PATH` before spawning `director-server`, so satellites resolve cleanly
regardless of how the .app was launched.

## Release

Tag-driven. Push `v*` and the GH Actions workflow:

1. Pulls the latest release of each satellite from their own repos.
2. Runs `build.sh` against those binaries.
3. Zips `Director.app` (via `ditto` to preserve bundle metadata) and
   attaches it to the release with a SHA-256 sum.

```bash
git tag v0.2.0 && git push origin v0.2.0
```

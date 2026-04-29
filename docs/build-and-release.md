# Build + release

## Local dev iteration loop

Most common: edit fleetview UI/Go, push fresh binaries into the running
.app, codesign, relaunch.

```bash
# fleetview
cd ~/dev/fleetview
make build                              # rebuilds web/dist THEN go binary (order matters)

# update Flow.app's bundled copy
cp fleetview ~/dev/fleet-app/build/bin/Flow.app/Contents/MacOS/fleetview
codesign --force --deep --sign - ~/dev/fleet-app/build/bin/Flow.app
pkill -f Flow.app/Contents
open ~/dev/fleet-app/build/bin/Flow.app
```

For roster changes: same pattern, copy `roster` into the bundle.

For prompt template changes: roster reads from `~/.config/roster/prompts/`
(disk override) AND embedded fallback. **Disk override wins** — re-copy
`prompts/<kind>.md` into `~/.config/roster/prompts/` after editing.

For plugin changes: edit in `~/dev/gkkirsch-claude-plugins/plugins/<name>/`,
push to GitHub, then re-publish to supercharge (see plugins.md and gotchas.md
re: the supercharge `.git` cache issue).

## Build order matters (fleetview Makefile)

```makefile
build: ui backend    ← UI must rebuild BEFORE Go because go:embed snapshots web/dist
```

Forgetting this means the binary ships yesterday's UI. Every "I changed
the SPA but my changes don't show" bug is this.

## Bundle layout

```
Flow.app/Contents/
  MacOS/
    Flow            ← Wails wrapper (this repo)
    fleetview       ← HTTP backend, serves the SPA + APIs
    roster          ← agent registry / spawn / notify
    camux           ← Claude Code TUI orchestration
    amux            ← tmux primitives
  Resources/
    iconfile.icns   ← generated from build/appicon.png by Wails
  Info.plist
```

`Flow` prepends `Contents/MacOS/` to PATH and merges in the user's login-shell
PATH so Finder-launched apps see brew/npm-installed tools (`tmux`, `claude`,
`node`). This is in `bootstrapPATH()` (`fleet-app/main.go`).

## Release pipeline (per repo)

Tag `v*` triggers `.github/workflows/release.yml` on macos-14:

```
roster, camux, amux  →  goreleaser  →  <repo>_darwin_arm64.tar.gz  containing the bare binary
fleet (fleetview)    →  goreleaser  →  fleet_darwin_arm64.tar.gz   containing the `fleetview` binary
                                       ^^^^^                       ^^^^^^^^^
                                       repo name ≠                 binary name
```

This **asymmetry** is the #1 trip-up:

| Repo (GitHub) | Tarball asset prefix | Binary inside |
|---------------|---------------------|---------------|
| `gkkirsch/roster` | `roster_*` | `roster` |
| `gkkirsch/camux`  | `camux_*`  | `camux`  |
| `gkkirsch/amux`   | `amux_*`   | `amux`   |
| `gkkirsch/fleet`  | `fleet_*`  | `fleetview` |

`fleet-app`'s release workflow handles this with an explicit `repo:binary`
mapping in its YAML. Don't write `for repo in roster camux amux fleetview`
loops — they 404 on `fleetview_*.tar.gz`.

## fleet-app release workflow

Tag fleet-app → workflow:
1. `actions/setup-go`, `actions/setup-node`
2. Install Wails CLI: `go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0`
3. Pull the latest release of each satellite (mapping above) into `satellites/`
4. Run `./build.sh` with `FLEETVIEW=satellites/fleetview ROSTER=...` env vars
5. `ditto -ck Flow.app Flow.app.zip` (preserves bundle metadata + symlinks
   — `zip` strips them)
6. `softprops/action-gh-release@v2` attaches `Flow.app.zip` + `.sha256`

## Order of operations for cutting a public release

1. **Tag every satellite that changed first**, in parallel. Wait for all
   workflows to publish releases.
2. Tag `fleet-app` only after all satellites are up — its workflow downloads
   the *latest* of each.
3. Verify the published `Flow.app.zip` actually has fresh binaries:

   ```bash
   ditto -xk /tmp/Flow.zip /tmp/verify/
   ls -la /tmp/verify/Flow.app/Contents/MacOS/   # check sizes/timestamps
   ```

## Public install

```bash
curl -fsSL https://github.com/gkkirsch/fleet-app/releases/latest/download/Flow.app.zip -o /tmp/Flow.zip \
  && ditto -xk /tmp/Flow.zip /Applications/ \
  && xattr -dr com.apple.quarantine /Applications/Flow.app \
  && open /Applications/Flow.app
```

The `xattr -dr` is required because the build is **ad-hoc signed**, not
notarized. Once we have an Apple Developer cert + `notarytool` integration,
that line goes away.

## What's not yet automated

- Apple Developer signing + notarization
- Sparkle auto-update inside the .app
- Custom Lucide-derived icon variants per release
- Linux/Windows builds

These all require a Developer cert ($99/yr) or design work; tracked but not
blocking v0.x.

# Gotchas

Things that bit us. Each entry: symptom → root cause → fix.

## Auth

### "Not logged in · Run /login" in a fresh orch
- **Symptom:** orch boots, replies fail with "Not logged in" inline,
  user is signed in globally.
- **Cause:** claude-code reads keychain via Cocoa Keychain Services API,
  not by shelling out to `security`. The PATH-prepended shim doesn't
  intercept Cocoa-direct reads. claude-code looks for
  `Claude Code-credentials-<sha256[:8] of CLAUDE_CONFIG_DIR>`, finds
  nothing, gives up.
- **Fix:** `mirrorClaudeCredsToOrch` writes the canonical entry into
  the per-orch suffixed entry on every spawn/resume. See `auth.md`.

## roster + claude-code

### Resume strands the orch
- **Symptom:** `roster resume <id>` returns
  `camux spawn: window <id>:cc disappeared` — Claude crashes immediately.
- **Cause:** roster passed `--resume <session_uuid>`, but the JSONL was
  rolled (Claude `/clear`) or deleted. claude-code can't resume a
  missing session.
- **Fix:** `cmdResume` now retries without `--resume` and clears
  `session_uuid` on the agent record. Persists so the next resume
  isn't stranded.

### orch + worker show the same chat
- **Symptom:** click an orch, click its worker — same messages in both.
- **Cause:** workers inherit the orch's `CLAUDE_CONFIG_DIR`. Each agent
  has its OWN `session_uuid` and JSONL, but they live in the same
  `projects/<encoded-cwd>/` dir. We were resolving JSONL by "newest in
  dir" which collapsed both onto whichever one was modified last.
- **Fix:** `findJSONLPath` always trusts the registered `session_uuid`
  when its file exists. "Newest in dir" is now only a last-resort
  fallback for the case when the registered file is gone.

### `permission-dialog` false positive
- **Symptom:** orch shows "permission-dialog" status; notify times out
  after 30s with `recipient not ready`.
- **Cause:** camux scanned the FULL 200-line capture for
  `Do you want to`. The orch's own assistant text contained
  "What do you want to do with it?" + a numbered list, which matched.
- **Fix:** `detectState` now requires THREE signals together — trigger
  phrase + numbered options + box-drawing frame `╭─/╰─/│ Do you want to`
  — all in the last 20 lines.

### Spawn + serial plugin install races
- **Symptom:** `seedFlowCore` ran two `claude plugin install` calls.
  Only the first one persisted to `installed_plugins.json`.
- **Cause:** ran in a goroutine; the goroutine got killed when the
  `roster spawn` CLI process exited.
- **Fix:** run synchronously. Adds ~5–10s to first spawn but every
  subsequent spawn short-circuits on "already installed."

## Plugin publishing

### Supercharge `.git` endpoint serves stale content
- **Symptom:** push to GitHub, run sync (or even delete + import-url),
  `git clone https://superchargeclaudecode.com/api/plugins/<slug>.git`
  shows yesterday's plugin.json. Server returns `{success: true,
  filesUpdated: N}` but the served bytes don't match.
- **Cause:** unconfirmed CDN/server cache between supercharge's DB and
  the `.git` HTTP endpoint.
- **Fix:** for any ship-critical change, use the **file-upload flow**:
  1. `DELETE /plugins/<id>`
  2. `POST /plugins` (create draft)
  3. `POST /plugins/<id>/files` for each file (multipart)
  4. `POST /plugins/<id>/submit`

  Then `git clone` the served `.git` and verify. Sync alone is unreliable.
  Skill at `~/.claude/skills/supercharge-api/SKILL.md` has been updated
  with this warning.

### Plugin manifest "agents: Invalid input"
- **Symptom:** `claude plugin install` fails with
  `Validation errors: agents: Invalid input`.
- **Cause:** `plugin.json` had bare-string globs for skills/agents
  (e.g. `"skills": "./skills/"`). claude-code's schema requires arrays
  of explicit paths.
- **Fix:** `"skills": ["./skills/<dir>"]`, `"agents": ["./agents/<file>.md"]`.

### Re-import re-uses old slug
- **Symptom:** delete then re-`import-url` — the plugin still serves old
  content.
- **Cause:** sometimes the slug gets a `-1` suffix on the new import,
  collisions left the old record around.
- **Fix:** delete every record with the slug prefix
  (`advanced-memory`, `advanced-memory-1`, …), THEN re-create via the
  file-upload flow.

## UI / Wails

### "Not draggable" macOS window
- **Symptom:** transparent title bar but the window won't drag.
- **Cause:** Wails v2 needs CSS to define drag regions; no built-in
  drag without CSS. Reverse-proxied content can't easily be modified.
- **Fix:** `FullSizeContent: true` + inject a 28px `--wails-draggable: drag`
  overlay div via `proxy.ModifyResponse`. See `fleet-app/main.go`.

### I-beam flash + text selection during drag
- **Symptom:** drag a Wails app window, I-beam cursor flickers, text
  briefly highlights below the drag strip.
- **Fix:** `cursor: default; user-select: none; -webkit-user-select: none`
  on the drag region, plus `onmousedown="event.preventDefault(); window.getSelection().removeAllRanges()"`.

### `target="_blank"` opens nothing
- **Symptom:** click external link in the app, browser doesn't open.
- **Cause:** WKWebView ignores `_blank` by default.
- **Fix:** click handler injected via `proxy.ModifyResponse` catches
  `a[target="_blank"]` clicks and `fetch('/__open?url=...')`. The Go
  handler shells `open <url>`. See `fleet-app/main.go`.

### Wails AssetServer `Assets` overrides `Handler`
- **Symptom:** "no `index.html` could be found in your Assets fs.FS".
- **Fix:** when using `Handler` (reverse proxy), set ONLY `Handler` —
  don't include `Assets` even with a placeholder.

## Process / OS

### Stale `/tmp/fleetview` squats :47821
- **Symptom:** Flow launches, rebuild updates the binary, but the running
  fleetview is still old.
- **Cause:** an old `director-server` from a debugging session is listening on
  :47821. Flow's `ensureFleetview` sees the port alive and skips spawning
  the new one.
- **Fix:** `pkill -f "fleetview$"` before relaunching.

### macOS runs ONE Chrome per profile dir
- **Symptom:** click an orch's "Browser" button, your normal Chrome
  comes up — not a separate window.
- **Cause:** `open -a "Google Chrome"` reuses the live process. With
  `--user-data-dir` set, macOS still merges into the running instance
  if one exists.
- **Workaround:** `open -na "Google Chrome"` forces a new instance.
  The orch already runs Chrome with `--user-data-dir=<profile>` so its
  session is isolated; the user's Chrome is separate.

### Bundled `roster` is stale even after rebuild
- **Symptom:** rebuild roster, copy to `~/.local/bin/roster`, behavior
  unchanged.
- **Cause:** Flow.app's `Contents/MacOS/` is FIRST on PATH inside Flow.
  Flow processes use the BUNDLED roster, not `~/.local/bin/roster`.
- **Fix:** copy the new binary into BOTH locations (or just the bundle)
  + `codesign --force --deep --sign -` + relaunch.

### Goroutines die with the CLI process
- **Symptom:** `roster spawn <id>` returns, but background work
  (e.g. plugin install) didn't finish.
- **Cause:** roster CLI exits → all goroutines killed.
- **Fix:** for spawn/resume work that NEEDS to complete, run
  synchronously even if it adds latency.

## Models

### "Sonnet 4.7" doesn't exist
- Latest sonnet is `claude-sonnet-4-6`. Latest opus is `claude-opus-4-7`.
  Easy to mix up — verify against
  `https://api.anthropic.com/v1/models` (auth'd) or
  `https://docs.claude.com/en/docs/about-claude/models/all-models`.

## Prompt overrides

### Edits to embedded prompt templates don't take effect
- **Cause:** roster reads from `~/.config/roster/prompts/<kind>.md`
  (disk override) FIRST, embedded fallback only if absent.
- **Fix:** after editing `roster/prompts/<kind>.md`, copy it to
  `~/.config/roster/prompts/<kind>.md` and respawn the affected agents.
  (Tagged release builds re-embed automatically; local dev needs the
  copy.)

## Misc

### Cursor doesn't update on agent click
- **Cause:** when the new agent's first scan returns,
  `effectiveClaudeDir` for `dispatcher` was returning `~/.claude` until
  we added the dispatcher case to the switch. Without that, dispatcher
  showed inherited skills.
- **Fix:** `case "orchestrator", "dispatcher":` returns own dir in
  `claude_scan.go`'s `effectiveClaudeDir`.

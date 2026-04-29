---
name: fleet-architecture
description: "Use when working in any of the fleet stack repos (fleet-app, fleet/fleetview, roster, camux, amux, gkkirsch-claude-plugins). Briefs the agent on the three-tier dispatcher/orchestrator/worker model, who owns what binary, where state lives on disk, and the asymmetric repo↔binary names so changes don't accidentally break isolation, auth mirroring, or the build pipeline."
---

# fleet-architecture

You are about to make changes inside the **fleet** stack. Read this first.

## Tier model

```
dispatcher (one)         routes via `roster notify`
   "director"
       │
       ▼
orchestrator (per domain)   delegates to workers, integrates results
       │
       ▼
worker (per task)            does one thing, reports up
```

- **Dispatcher** has its own `CLAUDE_CONFIG_DIR` with no plugins/skills.
  Pinned to `claude-sonnet-4-6`. Display name "director" but on-disk id
  is `dispatch`.
- **Orchestrators** have their own dir + flow-core marketplace
  (advanced-memory + advanced-knowledge auto-installed) + a dedicated
  headed Chrome on a deterministic CDP port.
- **Workers** inherit their orch's dir (skills, plugins, browser).

When introducing a new agent kind, update BOTH `roster/claudedir.go`
(`claudeDirFor`) AND `fleetview/claude_scan.go` (`effectiveClaudeDir`).
They mirror each other.

## Repo → binary mapping (asymmetric, watch out)

| GitHub repo | Tarball asset | Binary inside |
|---|---|---|
| `gkkirsch/roster` | `roster_*.tar.gz` | `roster` |
| `gkkirsch/camux`  | `camux_*.tar.gz`  | `camux`  |
| `gkkirsch/amux`   | `amux_*.tar.gz`   | `amux`   |
| `gkkirsch/fleet`  | `fleet_*.tar.gz`  | `fleetview` |

The `fleet` ↔ `fleetview` mismatch is the most common trip-up. Don't
write loops that assume `repo == binary`.

`gkkirsch/fleet-app` is the umbrella. Its release pipeline downloads the
latest of the four satellites and bundles them into `Flow.app/Contents/MacOS/`.

## Communication protocol

Inter-agent messages are wrapped:

```
<from id="X">
<body>

To respond, end your turn with: `roster notify X "<reply>" --from <self>`. Plain text alone does NOT reach X.
</from>
```

- The dispatcher's UI **hides** these envelopes.
- Orchestrator and worker UIs **render them with a `from <id>` caption**.
- Legacy `[from X]\n\n<body>` is still recognized by parsers and prompts.
- The dispatcher emits `<suggestions>…</suggestions>` blocks at the end of
  each reply; the UI strips them and renders bubbles above the input.

## Auth mirroring (load-bearing)

claude-code reads keychain via Cocoa Keychain Services API, NOT through
the `security` CLI. Roster's PATH-prepended shim that rewrites
`Claude Code-credentials-<hex>` → canonical only catches subprocess calls.

So roster ALSO mirrors the canonical entry into the per-orch suffixed
entry on every spawn/resume:

```
service: Claude Code-credentials-<sha256[:8] of CLAUDE_CONFIG_DIR>
```

Lives in `roster/claudedir.go` `mirrorClaudeCredsToOrch`. If you change
how claude-code looks up credentials, this is where to look.

## Plugin credentials → tmux env

Plugin authors don't deal with keychain:
1. User saves cred via UI form → `service=fleetview-<agentID>` keychain entry
2. roster's `injectPluginCreds` reads on spawn/resume, `tmux setenv`s each
   declared key
3. Plugin's skill/script reads `$KEY` from env

If you add a new plugin source layout or a new kind of plugin metadata,
update `fleetview/credentials.go` (`readPluginConfig`) AND
`roster/claudedir.go` (`readDeclaredCredKeys`).

## On-disk layout (running app)

```
~/.local/share/roster/
  agents/<id>.json                         per-agent record
  claude/<id>/                             per-orch CLAUDE_CONFIG_DIR
    .claude.json, settings.json
    skills/                                hidden auto-load skills
    plugins/installed_plugins.json
    plugins/marketplaces/<name>
    projects/<cwd-encoded>/<uuid>.jsonl    per-session conversation log
    scheduled_tasks.json                   cron tasks claude reads natively
  bin/security                             PATH-prepended shim
~/.config/roster/prompts/<kind>.md         on-disk prompt overrides (override embedded)
```

Workers share their orch's `<id>/` dir. Each agent has its OWN
`session_uuid` and JSONL inside that shared dir. **`findJSONLPath` MUST
trust the registered uuid.** Picking "newest in dir" makes orch and
worker show the same chat.

## Build order matters

`fleetview/Makefile`: `build: ui backend`. The Go binary `go:embed`s
`web/dist/` so the UI MUST rebuild first. Forgetting this means the
binary ships yesterday's UI.

`Flow.app/Contents/MacOS/` is FIRST on PATH inside Flow. So
`~/.local/bin/roster` does NOT shadow the bundled `roster` — copy
new binaries into BOTH locations during dev iteration, then
`codesign --force --deep --sign -` and relaunch.

## Prompt template overrides

`roster/prompts/<kind>.md` is embedded into the binary, but
`~/.config/roster/prompts/<kind>.md` (if present) takes precedence.
After editing the template, copy it to disk for local dev. Tagged
release builds re-embed automatically.

## Common task → which repo

| Task | Repo |
|---|---|
| Sidebar/topnav/chat UX | `gkkirsch/fleet` (`web/src/App.tsx`) |
| Plugin detail / setup metadata | `gkkirsch/fleet` (`plugin_setup.go`, `App.tsx`) |
| Agent lifecycle / spawn / claude config | `gkkirsch/roster` |
| Inter-agent message format | `gkkirsch/roster` (`commands.go::cmdNotify`) + UI parser |
| State detection / camux interrupt | `gkkirsch/camux` (`claude.go`) |
| tmux primitives | `gkkirsch/amux` |
| Wails wrapper / drag region / setup screen | `gkkirsch/fleet-app` |
| Default plugins (advanced-memory etc) | `gkkirsch/gkkirsch-claude-plugins/plugins/<name>/` |

## Before you commit

- Did you change inter-agent message format? Update prompts AND parsers
  in both roster and fleetview.
- Did you add a new agent kind? Add to `claudeDirFor` AND
  `effectiveClaudeDir`.
- Did you change a plugin's metadata? Don't trust supercharge's `sync`;
  use the file-upload re-publish flow (delete + draft + files + submit).
- Did you change a prompt template? Copy to `~/.config/roster/prompts/`
  for local effect.
- Did you change something in fleetview that the .app bundles? Copy the
  new binary into `Flow.app/Contents/MacOS/` AND codesign before testing.

## Read also (in repo: docs/)

- `architecture.md` — fuller context on the tier model
- `auth.md` — keychain entries and the shim
- `communication.md` — relay envelope and suggestion bubbles
- `gotchas.md` — bugs we've already paid for
- `conventions.md` — visual + code style rules

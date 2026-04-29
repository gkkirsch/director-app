# Plugins

A plugin is a directory under `~/dev/gkkirsch-claude-plugins/plugins/<name>/`.
It ships skills, agents, hooks, and (optionally) a Flow-specific setup recipe.

## Layout

```
<plugin-name>/
  .claude-plugin/
    plugin.json        ← claude-code manifest (name, version, skills[], agents[], hooks)
    config.json        ← Flow-specific setup metadata (creds, schedules, scripts)
    hooks.json         ← claude-code hooks
    skills/<name>/SKILL.md
    agents/<name>.md
  hooks/<name>.sh       ← script files referenced from hooks.json
```

Note the `.claude-plugin/` nesting: skills and agents live INSIDE that dir,
and `plugin.json` references them as `./skills/<dir>` and `./agents/<file>.md`
relative to that dir. claude-code's validator rejects bare-string globs
(`"skills": "./skills/"`) — must be **arrays of explicit paths**.

## `plugin.json` (claude-code's schema)

```json
{
  "name": "advanced-memory",
  "displayName": "Advanced Memory",
  "description": "...",
  "version": "1.0.0",
  "author": { "name": "flow", "email": "flow@superchargeclaudecode.com" },
  "keywords": ["memory", "persistence"],
  "skills": ["./skills/memory"],
  "agents": ["./agents/memory-consolidator.md"],
  "hooks": "./hooks.json"
}
```

## `config.json` (Flow's schema)

Flow-specific. claude-code ignores this file.

```json
{
  "credentials": [
    { "key": "GEMINI_API_KEY", "label": "Gemini API Key", "description": "...", "required": true }
  ],
  "schedules": [
    {
      "id": "advanced-memory-nightly",
      "label": "Nightly memory consolidation",
      "description": "...",
      "cron": "0 3 * * *",
      "prompt": "Run memory consolidation: spawn the memory-consolidator agent ...",
      "recurring": true
    }
  ],
  "setup_scripts": [
    {
      "id": "init-memory-dirs",
      "label": "Initialize memory directories",
      "description": "...",
      "command": "mkdir -p memory/topics && [ -f memory/MEMORY.md ] || printf ... > memory/MEMORY.md",
      "run_once": true
    }
  ]
}
```

Backward compat: if `config.json` is absent, fleetview falls back to the
legacy `.claude-plugin/credentials.json` (just the credentials array).

## Credential lifecycle

```
user types value in UI form
  └→ POST /api/agents/:id/credentials
      └→ keychainSet(agentID, plugin, marketplace, key, value)
          → security add-generic-password
              -s fleetview-<agentID>
              -a <plugin>@<marketplace>/<key>
              -w <value> -U

orch spawn / resume
  └→ prepareClaudeIsolation
      └→ injectPluginCreds(claudeDir, agentID, session)
          → walk installed_plugins.json
              → for each plugin, read its config.json (or credentials.json)
                  → for each declared key, keychainGet
                      → tmux set-environment -t <session> KEY VALUE

plugin's skill or script
  └→ reads $KEY directly
```

The plugin author **never deals with keychain**. They just declare keys in
config.json and read `$KEY` at runtime. fleetview + roster handle storage.

## Schedule lifecycle

```
UI: "Suggested schedules" section on plugin detail page
  └→ "Add" button
      └→ POST /api/agents/:id/plugins/apply-schedule
          → server reads the suggestion from the plugin's config.json
              → appends to <orch_dir>/scheduled_tasks.json
                  → claude-code reads scheduled_tasks.json natively
                      → fires the prompt at cron time
```

`Applied: true` is server-derived: scan-time check whether
`scheduled_tasks.json` already has a task with the matching `id`.
Idempotent — clicking "Add" twice is a no-op.

## Setup script lifecycle

```
UI: "Setup scripts" section
  └→ "Run" button
      └→ POST /api/agents/:id/plugins/run-script
          → server reads the script from the plugin's config.json
              → execs `bash -c <command>` with cwd = orch's recorded cwd
                  → returns stdout (and stderr on failure)
```

The cwd matters: `mkdir -p memory/topics` lands in the orch's project
directory, not roster's. `agentCwd(agentID)` reads it from the agent record.

## flow-core marketplace

The default marketplace bundled with Flow:

- Hosted: `https://superchargeclaudecode.com/api/marketplaces/flow-core/marketplace.json`
- Contents: `advanced-memory` + `advanced-knowledge`
- Auto-installed on every fresh **orchestrator** (not dispatcher, not worker —
  workers inherit from the orch).
- Lives in roster's `seedFlowCore` (in `claudeauth.go`).

To update flow-core:
1. Edit the plugin in `~/dev/gkkirsch-claude-plugins/plugins/<name>/`.
2. Push to GitHub.
3. **Don't trust supercharge's `sync` or `import-url` for re-publishing.** The
   served `.git` endpoint caches the import-time snapshot. Use the
   file-upload flow: delete + create draft + POST each file + submit.
   See `auth.md` and `gotchas.md` for details.
4. Re-add to the `flow-core` marketplace via
   `POST /marketplaces/<id>/plugins`.

## Publishing checklist

- `plugin.json` arrays not bare strings (`"skills": ["./skills/x"]`).
- `config.json` has the three top-level arrays even if empty (`[]`).
- `author.name = "flow"` for first-party plugins.
- `repository` matches the GitHub source.
- After upload to supercharge, do a fresh `git clone <slug>.git` and
  inspect the served plugin.json — sync isn't always reliable, so verify.

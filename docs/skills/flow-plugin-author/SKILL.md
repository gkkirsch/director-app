---
name: flow-plugin-author
description: "Use when authoring or modifying a Claude Code plugin under ~/dev/gkkirsch-claude-plugins/plugins/<name>/, especially anything that ships in the flow-core marketplace (advanced-memory, advanced-knowledge). Covers the .claude-plugin/ layout, the dual plugin.json + config.json schema, the credential→keychain→tmux→env runtime contract, and the supercharge re-publish workflow that actually works."
---

# flow-plugin-author

Use when working on a Flow plugin. Plugins are directories under
`~/dev/gkkirsch-claude-plugins/plugins/<name>/`.

## Layout

```
<plugin-name>/
  .claude-plugin/
    plugin.json        ← claude-code's manifest (name, skills[], agents[])
    config.json        ← Flow's setup metadata (creds, schedules, scripts)
    hooks.json         ← claude-code hooks
    skills/<name>/SKILL.md
    agents/<name>.md
  hooks/<name>.sh
```

Skills and agents live INSIDE `.claude-plugin/`. `plugin.json` references
them with paths relative to that dir.

## `plugin.json` — claude-code's schema

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

**`skills` and `agents` MUST be arrays of explicit paths.** Bare-string globs
(`"skills": "./skills/"`) are rejected by claude-code's validator with
`agents: Invalid input`.

## `config.json` — Flow's schema

claude-code ignores this file. Flow reads it.

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

Top-level arrays only. Even when empty, prefer `[]` over omitting the key
so the schema stays predictable.

## Credentials runtime contract

**Plugin authors do NOT touch the keychain.** Declare keys; read env vars.

```
config.json declares:    "credentials": [{ "key": "GEMINI_API_KEY", ... }]
                                         ↓
user types value in UI form
                                         ↓
fleetview saves to keychain:  service=fleetview-<agentID>  account=<plugin>@<marketplace>/<key>
                                         ↓
roster on spawn/resume:  reads each declared key, `tmux setenv`s it
                                         ↓
your skill/script reads:  $GEMINI_API_KEY
```

In your `SKILL.md`:

```markdown
Run with the API key from env:

\`\`\`bash
uv run script.py --api-key "$GEMINI_API_KEY" ...
\`\`\`

If $GEMINI_API_KEY isn't set, tell the user to add it under
Skills → <plugin> → Credentials in the Flow dashboard.
```

Don't write `security find-generic-password ...` in your SKILL.md.
That's the legacy nano-banana pattern; Flow's contract is env-only.

## Schedule lifecycle

When you ship a `schedule` in config.json, the user sees a "Suggested
schedules" section in the plugin detail panel. They click "Add" and the
suggestion is copied verbatim into the orch's `scheduled_tasks.json`.

claude-code reads `scheduled_tasks.json` natively and fires the prompt at
cron time. The prompt should:
- Include enough context that the orch knows what to do without the user
- Reference your agent (`spawn the memory-consolidator agent`) or
  invoke a skill explicitly

The schedule is idempotent on its `id` — clicking "Add" twice is a no-op.

## Setup scripts

A `command` is run via `bash -c` in the orch's project cwd (NOT roster's).

```json
{
  "id": "init-memory-dirs",
  "command": "mkdir -p memory/topics && [ -f memory/MEMORY.md ] || printf '# Memory\\n' > memory/MEMORY.md",
  "run_once": true
}
```

Quote things carefully — the command is a single string, runs through
`bash -c`. `[ -f X ] || ...` patterns work because the shell evaluates
them.

## Publishing to supercharge

The flow-core marketplace points at supercharge-hosted plugin URLs.
Re-publishing has a known cache issue: **`sync` and `import-url` both
return success but the served `.git` endpoint can stay stale**.

For ship-critical changes, use the **file-upload flow**:

```bash
TOKEN=$(curl -s -X POST https://superchargeclaudecode.com/auth/login \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['token'])")

# 1. Find existing plugin id, delete it
ID=$(curl -s https://superchargeclaudecode.com/plugins/my-plugins \
  -H "Authorization: Bearer $TOKEN" \
  | python3 -c "import json,sys
for p in json.load(sys.stdin)['data']:
  if p['slug']=='<slug>': print(p['id']); break")
curl -s -X DELETE "https://superchargeclaudecode.com/plugins/$ID" \
  -H "Authorization: Bearer $TOKEN"

# 2. Create draft
PID=$(curl -s -X POST https://superchargeclaudecode.com/plugins \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"<slug>","description":"...","version":"1.0.0"}' \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['id'])")

# 3. Upload each file
cd ~/dev/gkkirsch-claude-plugins/plugins/<name>
find . -type f ! -name '.DS_Store' | sed 's|^./||' | while read rel; do
  curl -s -X POST "https://superchargeclaudecode.com/plugins/${PID}/files" \
    -H "Authorization: Bearer $TOKEN" \
    -F "file=@./${rel}" \
    -F "relativePath=${rel}" > /dev/null
done

# 4. Submit
curl -s -X POST "https://superchargeclaudecode.com/plugins/${PID}/submit" \
  -H "Authorization: Bearer $TOKEN"

# 5. Re-add to flow-core marketplace
curl -s -X POST "https://superchargeclaudecode.com/marketplaces/<flow-core-id>/plugins" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"pluginId\":\"$PID\"}"

# 6. VERIFY
rm -rf /tmp/v && git clone --depth 1 https://superchargeclaudecode.com/api/plugins/<slug>.git /tmp/v
cat /tmp/v/.claude-plugin/plugin.json   # should match what you pushed
```

Always do step 6. If the served plugin.json is stale, you have a CDN
issue and re-uploading is the only known cure.

## Update flow-core when adding a plugin

`gkkirsch/roster` references the flow-core marketplace URL hardcoded in
`claudeauth.go::flowCoreMarketplaceURL` and the auto-install list in
`flowCorePlugins`. Add your plugin slug to that list if you want it
auto-installed on every fresh orch.

After roster change → tag a new release. Then bump fleet-app to a new
release so the bundle picks up the new roster. (See architecture.md
for the asymmetric repo↔binary naming.)

## Local test before publishing

```bash
# Create a fresh orch in a scratch dir
mkdir /tmp/test-plugin && cd /tmp/test-plugin
roster spawn test-orch --kind orchestrator --description "plugin smoke test"
# Wait ~10s for spawn to complete (claude installs flow-core)

# Manually install your plugin from the local source
CLAUDE_CONFIG_DIR=/Users/gkkirsch/.local/share/roster/claude/test-orch \
  claude plugin install --path ~/dev/gkkirsch-claude-plugins/plugins/<name>
# Or via marketplace.json after publishing

# Verify in Flow:
# - Skills panel → marketplace tile shows the plugin
# - Plugin detail page → credentials/schedules/setup_scripts render
# - Setup script runs cleanly in the orch's cwd
# - Suggested schedule applies to scheduled_tasks.json

# Tear down
roster forget test-orch
rm -rf /Users/gkkirsch/.local/share/roster/claude/test-orch
```

## Checklist before tagging a release

- `plugin.json` arrays not bare strings
- `config.json` has all three top-level arrays (`credentials`, `schedules`,
  `setup_scripts`) even if empty
- Credential keys are upper-snake-case env-var-friendly names
- `author.name = "flow"` for first-party plugins
- `repository` field points at the GitHub source
- `git push origin main` succeeded
- Re-published to supercharge via the file-upload flow
- `git clone <slug>.git` shows the new content
- Flow's plugin detail panel renders credentials + schedules + scripts
  correctly

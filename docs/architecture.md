# Architecture

## Three-tier agent model

```
dispatcher ── routes ──→ orchestrator ── delegates ──→ worker
   "director"             one per domain                one per task
```

Each tier has a different scope and a different default config.

### Dispatcher (one)
- The user-facing surface. On-disk id `director`.
- **Pure router.** Reads `roster list`, picks the best orchestrator by description,
  delegates via `roster notify`. Does not do domain work itself.
- Pinned to `claude-sonnet-4-6` via `--model` on spawn.
- Own `CLAUDE_CONFIG_DIR` at `<roster_data>/claude/director`. **No plugins, no skills**
  — the user's global `~/.claude` plugin inventory does NOT bleed in.
- Skills tab is hidden in the UI for this kind.
- Emits a `<suggestions>` block at the end of every reply (see communication.md).

### Orchestrator (per domain)
- Owns a domain (`hostreply`, `linkedin`, etc.).
- Spawns workers, integrates their results, reports up.
- Own `CLAUDE_CONFIG_DIR`. **flow-core** marketplace is auto-installed on first spawn:
  `advanced-memory` + `advanced-knowledge`.
- Dedicated headed Chrome on a deterministic CDP port. Theme color and profile name
  match the orch's id.
- Auto-restart any plugin/skill the user installs via the UI.

### Worker (per task)
- Leaf executor. One delegated task, then notifies parent.
- **Inherits its orchestrator's** `CLAUDE_CONFIG_DIR` — same skills, same plugins,
  same Chrome (same CDP port).
- Writes to its own session JSONL inside the shared dir; we trust the registered
  `session_uuid`, never "newest in dir."

## Repos

| Repo | Lang | Role |
|------|------|------|
| `gkkirsch/director-app` | Go (Wails) | macOS .app wrapper. Spawns `director-server` as a sidecar; reverse-proxies its HTTP + injects the drag region. |
| `gkkirsch/director` | Go + React | `director-server` backend (HTTP at :47821) + React SPA. The dashboard. |
| `gkkirsch/roster` | Go | Agent registry. `spawn` / `resume` / `notify` / `forget` / `list`. Owns CLAUDE_CONFIG_DIR isolation, tmux session env, prompt templates. |
| `gkkirsch/camux` | Go | Claude Code TUI primitives. State detection (regex on tmux pane) + `interrupt` + `spawn` + `ask`. |
| `gkkirsch/amux` | Go | Thin tmux wrapper. `new` / `kill` / `paste` / `capture`. |
| `gkkirsch/gkkirsch-claude-plugins` | misc | Plugin source. Each plugin is a directory under `plugins/`. |

`fleet-app` is the only repo that knows about all the others — its `build.sh`
collects the latest release of each satellite into `Flow.app/Contents/MacOS/`.

## Filesystem layout (running app)

```
~/.local/share/roster/
  agents/<id>.json                         per-agent record (kind, parent, session_uuid, target, cwd, ...)
  claude/<id>/                             per-orch CLAUDE_CONFIG_DIR
    .claude.json                           seeded onboarding state
    settings.json                          seeded permission/bypass state
    skills/                                hidden auto-load skills (agent-browser, artifact)
    plugins/installed_plugins.json         claude-managed
    plugins/marketplaces/                  claude-managed
    plugins/cache/<marketplace>/<plugin>/  installed plugin source
    projects/<cwd-encoded>/<uuid>.jsonl    per-session conversation log
    scheduled_tasks.json                   cron tasks claude reads natively
    artifacts/<aid>/                       Vite + React 19 starter scaffolds
  bin/security                             PATH-prepended shim
  browser-profiles/<id>/                   per-orch headed Chrome profile

~/.config/roster/prompts/                  on-disk prompt overrides (override embedded)
~/.claude/                                 user's global config (used by orchs that opted in / for shared things)
```

## Process tree (typical)

```
Flow (Wails app)                                ← user-launched
└── fleetview (HTTP server on :47821)            ← spawned by Flow as sidecar
    └── tmux server                             ← already running on user's machine
        ├── session "director"
        │   └── window "cc" → claude            ← the dispatcher
        ├── session "orch-foo"
        │   └── window "cc" → claude            ← an orchestrator
        ├── session "worker-bar"
        │   └── window "cc" → claude            ← a worker (own session, same CLAUDE_CONFIG_DIR as parent orch)
        └── headed Chrome (per orch)            ← spawned via launchChrome
```

Each `claude` process is real Claude Code, isolated by `CLAUDE_CONFIG_DIR`.
Roster talks to them by pasting into their tmux window.

## Why this hierarchy, not a flat agent graph

- **Context is the bottleneck.** A flat "router that does everything" runs out
  of context fast. Three-tier means each agent's prompt + history is small.
- **Scope discipline.** A dispatcher that never edits files, an orchestrator
  that delegates, a worker that does one thing. Easier to reason about.
- **Isolation.** A bad plugin install on a worker can't hurt the dispatcher.
  Auth scopes, browser sessions, and skill installs are all per-orch.

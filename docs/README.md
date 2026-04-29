# Flow developer notes

Living notes for working on the fleet stack. Audience: us, future-Claude, anyone joining.
Each file is a peer-to-peer brief, not formal docs — read before you start changing
something so you don't re-discover what we already learned.

## Files

| File | When to read it |
|------|-----------------|
| [architecture.md](./architecture.md) | First time working on any of the repos. Three-tier agent model + which binary owns what. |
| [communication.md](./communication.md) | Touching anything related to inter-agent messaging, the dispatcher's reply protocol, or suggestion bubbles. |
| [plugins.md](./plugins.md) | Authoring or modifying a plugin in `gkkirsch-claude-plugins` or the flow-core marketplace. |
| [auth.md](./auth.md) | Touching keychain entries, the `security` shim, or seeing "Not logged in" in an orch. |
| [build-and-release.md](./build-and-release.md) | Cutting a release, debugging the GitHub Actions pipelines, or asymmetric repo↔binary names. |
| [gotchas.md](./gotchas.md) | Most useful before you change anything tricky. Bugs we hit and how we fixed them. |
| [conventions.md](./conventions.md) | Touching UI styling, copy, color, icons, comments. |
| [skills/](./skills) | Drop-in Claude Code skills extracted from this work. Install by copying into `~/.claude/skills/`. |

## Repo map

```
gkkirsch/fleet-app  →  Wails .app wrapper (this repo)
gkkirsch/fleet      →  fleetview backend + React SPA  (binary name: fleetview)
gkkirsch/roster     →  agent registry / lifecycle (Go)
gkkirsch/camux      →  Claude Code TUI orchestration
gkkirsch/amux       →  thin tmux wrapper
gkkirsch/gkkirsch-claude-plugins  →  plugin source
```

The `fleet-app` repo bundles the latest release of the four satellites
into `Flow.app/Contents/MacOS/`. Update one of the satellites and you
need a new tag on the satellite *first*, then on `fleet-app`.

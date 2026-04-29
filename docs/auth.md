# Auth + keychain

This is the trickiest area in the project. Read this before touching anything
keychain-related.

## Three keychain entry types

All under macOS keychain (`security` CLI, or Cocoa Keychain Services API).

### 1. `Claude Code-credentials` (canonical)
- Set by `claude` CLI on login.
- The user's "real" auth — what `~/.claude` reads.
- Account: `$USER`.
- Value: JSON `{"claudeAiOauth":{"accessToken":...,"refreshToken":...}}`.

### 2. `Claude Code-credentials-<sha256[:8] of CLAUDE_CONFIG_DIR>` (per-agent)
- What claude-code reads when `CLAUDE_CONFIG_DIR` is set.
- Each orchestrator and dispatcher has its own (because each has its own dir).
- Workers use their orch's dir, so they share their orch's entry.
- **Roster mirrors canonical → per-agent on every spawn AND resume.**

### 3. `fleetview-<agentID>` / `<plugin>@<marketplace>/<key>` (plugin creds)
- What fleetview's UI saves to when the user types a plugin credential.
- Per-agent isolated.
- Roster reads these on spawn/resume and `tmux setenv`s them as env vars.

## The shim and why it's not enough

`/Users/gkkirsch/.local/share/roster/bin/security` is a wrapper installed
on every orch's tmux PATH. It rewrites:

```
Claude Code-credentials-<hex>  →  Claude Code-credentials
```

…for `find-generic-password` and `add-generic-password` calls.

**But:** claude-code reads the keychain directly via the **macOS Keychain
Services API (Cocoa)**, not by shelling out. The shim doesn't intercept
that. So a fresh orch's claude-code looks for
`Claude Code-credentials-<hash>`, finds nothing, and shows
"Not logged in · Run /login" inline on every reply.

The fix: roster's `mirrorClaudeCredsToOrch` reads the canonical entry
and writes it to the per-agent suffixed entry on every `prepareClaudeIsolation`
(spawn AND resume). Lives in `roster/claudedir.go`.

```go
service := "Claude Code-credentials-" + sha256[:8](claudeDir)
security add-generic-password -s <service> -a $USER -w <canonical> -U
```

Refresh-token rotation can drift mid-session, but the user's normal
"stop / resume" flow re-syncs.

## Plugin credential injection

```go
// roster/claudedir.go
injectPluginCreds(claudeDir, agentID, session) {
    for plugin in installed_plugins.json {
        for key in plugin's config.json `credentials` {
            value := keychainGet("fleetview-" + agentID, plugin@market/key)
            if value != "": tmux set-environment -t session KEY value
        }
    }
}
```

Plugin scripts then read `$KEY` directly. Zero keychain knowledge required
in the plugin.

The plugin can declare a credential without anyone having saved it yet —
`keychainGet` misses, env stays unset, plugin's own fallback ("env var not
set, ask user") kicks in.

## Login state guard

`prepareClaudeIsolation` errors loudly if the user hasn't completed Claude
login at all (`userKeychainHasClaudeCreds()` checks the canonical entry).
Better than booting an orch that crashes on its first API call.

## Why we don't put creds in env at process spawn time instead of tmux

We could `os.Setenv` before exec'ing claude. Two reasons we use `tmux setenv`
instead:

1. **Worker inheritance.** Workers are new windows in the orch's existing
   tmux session. They inherit the session env, not roster's process env.
2. **Resume.** A `roster resume` re-fires `prepareClaudeIsolation` which
   updates the session env in place — workers spawned *after* a credential
   change pick it up automatically.

## Debugging "Not logged in" in an orch

```bash
# Check canonical entry exists
security find-generic-password -s "Claude Code-credentials" -a "$USER" 2>&1 | tail -3

# Compute orch's expected hash
DIR=/Users/gkkirsch/.local/share/roster/claude/<orch-id>
echo -n "$DIR" | shasum -a 256 | head -c 8

# Check per-orch entry has the value
security find-generic-password -s "Claude Code-credentials-<hash>" -a "$USER" -w | head -c 30

# If empty, force a re-mirror by resuming
roster stop <orch-id> && sleep 1 && roster resume <orch-id>
```

## Which entry should I use as a developer?

| You want to... | Use entry |
|----------------|-----------|
| Read the user's real Claude auth | `Claude Code-credentials` (canonical) |
| Write a plugin that needs creds | declare in plugin's `config.json`, read `$KEY` from env |
| Make an orch use a specific token | not really supported — we always mirror canonical |
| Test plugin cred injection without UI | `security add-generic-password -s fleetview-<id> -a 'plugin@market/KEY' -w 'value' -U` then resume the orch |

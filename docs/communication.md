# Communication

How agents talk to each other and to the user.

## Inter-agent messages: `<from id="X">` envelope

When an agent sends a message via `roster notify --from X`, roster wraps the
body before pasting it into the recipient's tmux pane:

```
<from id="X">
<body>

To respond, end your turn with: `roster notify X "<your reply>" --from <your-agent-id>`. Plain text alone does NOT reach X.
</from>
```

Why a tag instead of a `[from X]` prefix:
- **Unambiguous parse boundary.** The dashboard can detect "this is a relay"
  by tag, not by guessing where the prefix ends.
- **Hide-able.** The director chat hides relays entirely; orch/worker panes
  render them with a small "from X" caption above the body.

The reply-protocol footer lives **inside** the wrapper. Agents are taught
(via prompt) to extract and act on the body, then run `roster notify`.

### UI rendering rules

| Pane | Relay treatment |
|------|-----------------|
| Director (dispatcher) | **Hidden entirely.** User only sees their own messages and the director's plain-text replies. |
| Orchestrator | **Shown** as a right-side bubble with `from <id>` caption. The relay IS the task — it'd be weird to hide it. |
| Worker | Same as orchestrator. |

Plain user input (no `--from`) renders normally on every pane.

### Legacy compat

The old `[from X]\n\n<body>` prefix format is still recognized by both the
prompts and the UI parser (`isAgentRelay` / `agentRelaySender` / `relayBody`).
Agents that haven't been respawned with the new prompt template still work.
Don't remove the legacy regex.

## Director's reply protocol

The dispatcher is the user's voice surface. It speaks plain text; the user
sees it directly. It does **not** notify the user — that would just paste
text back into its own pane.

When an orchestrator notifies the dispatcher, the dispatcher reads the relay
and produces a plain-text summary. No further notify needed.

## Suggestion bubbles

After every reply, the dispatcher appends:

```
<suggestions>
yeah, run a quick test
no, just ship it
what could go wrong?
</suggestions>
```

The UI:
1. Strips the block from the rendered chat (it's never visible).
2. Parses the lines into bubble buttons.
3. Renders bubbles above the input — but only when (a) we have any AND
   (b) the user hasn't started typing.
4. Click → pre-fills the textarea + focuses it (does NOT auto-send).

### Voice rules (in the prompt)

- **First-person, as the user.** "yeah, draft the email" — not "Spawn the
  email orchestrator."
- **Tied to the last reply.** If the dispatcher just asked a yes/no, two
  bubbles should be reasonable yes/no answers and the third a sideways
  follow-up.
- **Lowercase first word OK.** Reads more like real chat.
- **Max ~7 words.**

The default empty-state bubbles are coded in App.tsx as
`DEFAULT_DIRECTOR_SUGGESTIONS`. Once the dispatcher has emitted suggestions,
those override the defaults.

## Agent display names

| On-disk id | UI name | Why |
|------------|---------|-----|
| `dispatch` | `director` | Reads more naturally for the user. |

The aliasing is one-way: other agents address it as `dispatch`. Only the UI
substitutes "director" for display.

## Suggestion / relay regex reference

```js
SUGGESTIONS_RE  = /<suggestions>([\s\S]*?)<\/suggestions>\s*$/i
FROM_TAG_RE     = /^<from\s+id="([^"]+)">([\s\S]*?)<\/from>\s*$/m
FROM_PREFIX_RE  = /^\[from ([^\]]+)\]\n\n([\s\S]*)$/   ← legacy
```

All three live in `fleetview/web/src/App.tsx`. If you change the wire format
in roster, change them here too.

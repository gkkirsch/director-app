# Conventions

## Visual / UX

### Color palette ("japandi")
Defined in `fleetview/web/src/styles.css`:

- `--linen` — warm off-white background (#F5EFE6)
- `--clay` / `--clay-soft` — warm tan accent. Used for the dispatcher icon,
  destructive armed states, "needs attention" notification dot.
- `--matcha` / `--matcha-soft` — sage green. Used for orchestrator icon,
  "installed/enabled" pills, primary affordances.
- `--ochre` / `--ochre-soft` — mustard. Used for streaming state.
- `--stone` — neutral, scrollbar thumb.
- `--font-heading` — Fraunces (serif). Used for the wordmark, page titles,
  panel headers.

Don't introduce new accent colors casually. Pick from this set.

### Icons
- **lucide-react only** — no emojis in UI text, code comments, or chat output.
- Sidebar agent rows use `KindGlyph` (parameterized by `size`):
  - dispatcher: `Navigation` filled, clay, stroke 2.4
  - orchestrator: `Layers`, matcha primary, stroke 1.7
  - worker: `CornerDownRight`, muted-foreground, stroke 1.7
- The director's icon is the only filled one — distinguishes the router
  visually. Stroke is bumped to 2.4 for presence.
- For nav buttons (Browser / Artifacts / Schedules / Skills): label + tiny
  3.5×3.5 icon, `tracking-[0.22em] uppercase`, label hidden via
  container query `@lg:inline` so the bar collapses gracefully.

### Sidebar row
- Glyph + truncating label, no second status line.
- Streaming → label uses `.shimmer-text` (gradient sweep, same as
  message-view "thinking" verb).
- permission-dialog / trust-dialog → small clay pulsing dot at the right.
- stopped → no indicator (auto-resume on send hides it from the user).
- Selected row gets `pr-9` so the inline trash button has room without
  overlapping the truncated label.

### Two-step destructive actions
- Trash / uninstall / remove-marketplace: first click ARMS (icon + label
  flip to clay), second click commits. Auto-disarms after 4s.
- Same pattern across sidebar inline delete, plugin uninstall, marketplace
  remove. Don't invent new confirm dialogs for this.

### Suggestion bubbles
- Above the input, shown only when (a) we have suggestions AND (b) the
  user hasn't typed anything.
- Voice rules in `roster/prompts/dispatcher.md`. Lowercase first word OK.
- Click pre-fills the textarea + focuses, doesn't auto-send.

### Top bar
- Absolutely positioned over the chat (NOT a flex sibling).
- `bg-background/35 backdrop-blur-md` — frosted glass.
- The chat scrolls underneath it (compensate via `pt-20` on the scroller).
- Drag region (28px, `--wails-draggable: drag`) sits on top of everything
  via z-index 2147483647.

### Auto-grow textarea
- Min 112px, grows to 280px (~12 lines), then scrolls silently
  (`.scrollbar-none` utility). Default scrollbars are too chunky.

## Code

### Comments
- Explain WHY, not WHAT. Identifier naming covers the what.
- Include hidden constraints, subtle invariants, workarounds.
- DON'T reference task/PR/caller ("for the LinkedIn flow", "added in #42")
   — those rot.
- One-line max for most cases. Multi-paragraph docstrings only when the
  function has a non-obvious contract (e.g., findJSONLPath's lookup
  order, mirrorClaudeCredsToOrch's reason for existence).

### Errors
- Surface user-actionable messages inline. The `send-error` path,
  marketplace add error, plugin install error all bubble back into the
  UI as-is.
- Don't swallow with generic "something went wrong."
- Don't validate against internal callers — only validate at system
  boundaries (HTTP request bodies, user input).

### Goroutines
- Don't use them in CLI commands. They die with the parent process.
  If a side effect MUST complete, run synchronously even if it adds
  latency. Annotate the choice.

### Backend handlers (fleetview)
- Pattern: `handleX(w, r, id)` style; routing in `main.go`'s `router()`.
- For "set X on agent Y" handlers: load agents, find by id, resolve
  their effective claude dir (`effectiveClaudeDir`), do the thing.
- The `effectiveClaudeDir` switch defines per-kind isolation. If you
  add a new kind, update both this switch AND roster's `claudeDirFor`.

### Frontend (fleetview/web/src/App.tsx)
- One file, ~3000 lines. Components inline, helpers near their use.
- Don't pre-extract components for "modularity" — extract when a
  component needs to render in multiple places, or has its own state
  worth isolating.
- State that survives agent switch lives in `App`. State per-agent
  lives in the relevant component with `useEffect([agentId])` reset.

### Plugin metadata
- `plugin.json` is claude-code's domain. Keep it minimal + standards-
  compliant. Don't put Flow-specific fields here.
- `config.json` is Flow's. Three top-level arrays only:
  `credentials`, `schedules`, `setup_scripts`.
- Plugins MUST author their skill/script to read `$KEY` from env, not
  query the keychain directly.

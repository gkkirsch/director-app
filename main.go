package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

// Wails wants an embedded asset FS even though we never use it — the
// webview content comes from our reverse-proxied director-server. A
// placeholder file keeps the embed pattern valid.
//

const (
	backendAddr = "127.0.0.1:8080"
	backendURL  = "http://" + backendAddr
)

func main() {
	bootstrapPATH()

	// `--reset` wipes Director state so first-run can be replayed end
	// to end. Useful for testing the setup flow without fully nuking
	// ~/.local/share/roster (which would also drop other agents we
	// care about between sessions). Wipe is intentionally narrow:
	// dispatcher claude dir, Director data home, agent-browser
	// daemons. Existing orchs the user has spawned via the app are
	// preserved so resetting doesn't discard real work.
	if len(os.Args) > 1 && os.Args[1] == "--reset" {
		if err := resetDirectorState(); err != nil {
			fmt.Fprintln(os.Stderr, "fleet-app: reset:", err)
			os.Exit(1)
		}
		fmt.Println("✓ Director state reset. Launch the app to replay first-run setup.")
		os.Exit(0)
	}

	backend, _ := url.Parse(backendURL)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.ModifyResponse = injectDragRegion

	app := &appState{proxy: proxy}
	defer app.shutdown()

	// If prereqs already pass at launch, spawn director-server eagerly and
	// kick off dispatcher init in a goroutine. The dispatch handler
	// gates `/` on init status until the dispatcher exists, so users
	// see the setup page (with init progress) instead of an empty UI.
	if len(checkPrereqs()) == 0 {
		if err := app.startDirectorServer(); err != nil {
			fmt.Fprintln(os.Stderr, "fleet-app:", err)
			os.Exit(1)
		}
		go app.initDirector()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__open", openURLHandler)
	mux.HandleFunc("/__setup/recheck", app.recheckHandler)
	mux.HandleFunc("/__setup/run", runInTerminalHandler)
	mux.HandleFunc("/__setup/auth-token", authTokenHandler)
	mux.HandleFunc("/__setup/sniff-token", sniffTokenHandler)
	mux.HandleFunc("/", app.dispatch)

	err := wails.Run(&options.App{
		Title:     "Director",
		Width:     1280,
		Height:    820,
		MinWidth:  720,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			// Handler is the source of truth — everything proxies to
			// director-server, including /. Skip Assets entirely so Wails
			// doesn't intercept "/" → embedded index.html.
			Handler: mux,
		},
		BackgroundColour: &options.RGBA{R: 245, G: 239, B: 230, A: 1}, // linen
		Mac: &mac.Options{
			// FullSizeContent extends the webview under the title
			// bar so traffic lights overlay our linen background.
			// We then inject a 28px drag strip at the top via the
			// proxy (see injectDragRegion). The sidebar uses pt-10
			// and the top nav uses pt-8, so the strip lands on
			// empty space and doesn't swallow any button clicks.
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                  true,
				HideTitleBar:               false,
				FullSizeContent:            true,
				UseToolbar:                 false,
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wails:", err)
		os.Exit(1)
	}
}

// appState wires together the lazily-spawned director-server backend, the
// reverse proxy in front of it, and a per-request dispatcher that
// picks between "show setup page" and "proxy to director-server" based on
// whether prereqs are satisfied yet.
//
// initStatus tracks the dispatcher-spawn state machine. Friends
// downloading the .app for the first time were landing on an empty
// fleet because `roster spawn director` could fail silently — now
// the spawn captures stderr, writes a setup.log, and the setup page
// stays visible (with retry) until the dispatcher actually exists.
type appState struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	ready bool
	proxy *httputil.ReverseProxy

	initMu     sync.RWMutex
	initStatus string // "", "running", "ok", "failed"
	initError  string
}

const (
	initRunning = "running"
	initOK      = "ok"
	initFailed  = "failed"
)

func (a *appState) setInit(status, errMsg string) {
	a.initMu.Lock()
	a.initStatus = status
	a.initError = errMsg
	a.initMu.Unlock()
}

func (a *appState) getInit() (status, errMsg string) {
	a.initMu.RLock()
	defer a.initMu.RUnlock()
	return a.initStatus, a.initError
}

// dispatch routes incoming requests. Setup page when prereqs aren't
// met OR director-server isn't up OR the dispatcher hasn't been spawned yet;
// otherwise reverse-proxy to director-server.
//
// We gate on dispatcher existence (initStatus != ok) because without
// a dispatcher the UI loads against an empty fleet and looks broken.
// Better to keep the setup page visible with a "Setting up Director…"
// card that surfaces real errors when the spawn fails.
func (a *appState) dispatch(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	ready := a.ready
	a.mu.Unlock()
	status, _ := a.getInit()
	if !ready || status != initOK {
		setupHandler(a, w, r)
		return
	}
	a.proxy.ServeHTTP(w, r)
}

// startDirectorServer locates the bundled binary, spawns it, and waits up
// to 5s for it to bind backendAddr. Idempotent — calling twice is a
// no-op once director-server is up.
func (a *appState) startDirectorServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready {
		return nil
	}
	if portAlive(backendAddr) {
		a.ready = true
		return nil
	}
	bin, err := findDirectorServer()
	if err != nil {
		return fmt.Errorf("director-server not found and nothing is listening on %s", backendAddr)
	}
	cmd := exec.CommandContext(context.Background(), bin)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Force a sensible cwd. When the .app is launched by Finder/Dock,
	// director-server otherwise inherits "/" — and every subsequent
	// `roster spawn` (called by the dispatcher / orchestrators) inherits
	// that too, which means agents try to mkdir under "/" and hit
	// "Read-only file system." Anchor everything at the data dir so
	// artifacts land somewhere writable and discoverable.
	cmd.Dir = directorDataDir()
	if err := os.MkdirAll(cmd.Dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "fleet-app: mkdir %s: %v\n", cmd.Dir, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start director-server: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if portAlive(backendAddr) {
			a.cmd = cmd
			a.ready = true
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("director-server did not bind %s within 5s", backendAddr)
}

func (a *appState) shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Signal(os.Interrupt)
		_, _ = a.cmd.Process.Wait()
	}
}

// directorDataDir is where Director's spawned agents live and write
// things. Defaults to ~/Library/Application Support/Director (proper
// macOS convention — not visible in Finder's home view, survives the
// app being moved). Honors $DIRECTOR_HOME for users who want to move
// it.
func directorDataDir() string {
	if d := os.Getenv("DIRECTOR_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Director")
}

// findDirectorServer prefers the sibling binary inside the .app bundle
// (build.sh drops it there), then falls back to PATH for dev workflow.
func findDirectorServer() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			sibling := filepath.Join(filepath.Dir(real), "director-server")
			if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return sibling, nil
			}
		}
	}
	return exec.LookPath("director-server")
}

// dragSnippet is a tiny HTML overlay we splice into every HTML response
// from fleetview. Wails interprets `--wails-draggable: drag` as a window
// drag handle, so this 28px strip across the top makes the window draggable
// without us having to ship custom CSS in director-server itself. Buttons start
// at pt-8/pt-10 so nothing clickable lives in this band.
//
// Plus a global click handler that catches target="_blank" links and
// pipes them through /__open, since WKWebView ignores _blank otherwise.
const dragSnippet = `<style>
.__wails_drag_region{position:fixed;top:0;left:0;right:0;height:28px;z-index:2147483647;--wails-draggable:drag;-webkit-app-region:drag;cursor:default;user-select:none;-webkit-user-select:none;-webkit-touch-callout:none;}
.__wails_drag_region *{--wails-draggable:no-drag;-webkit-app-region:no-drag;}
</style><div class="__wails_drag_region" onmousedown="event.preventDefault();window.getSelection&&window.getSelection().removeAllRanges();"></div>
<script>
(function(){
  document.addEventListener('click', function(e) {
    var a = e.target && e.target.closest && e.target.closest('a[target="_blank"]');
    if (!a || !a.href) return;
    e.preventDefault();
    fetch('/__open?url=' + encodeURIComponent(a.href)).catch(function(){});
  }, true);
})();
</script>`

func injectDragRegion(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		return nil
	}
	body, err := readBody(resp)
	if err != nil {
		return err
	}
	idx := bytes.LastIndex(body, []byte("</body>"))
	if idx < 0 {
		idx = len(body)
	}
	out := make([]byte, 0, len(body)+len(dragSnippet))
	out = append(out, body[:idx]...)
	out = append(out, dragSnippet...)
	out = append(out, body[idx:]...)

	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
	resp.Header.Del("Content-Encoding") // we decoded gzip if present
	return nil
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return io.ReadAll(gz)
	}
	return io.ReadAll(resp.Body)
}

// openURLHandler shells out to `open <url>` so links escape the
// WKWebView into the user's default browser. Restricted to http(s)
// so a malicious page can't trick us into running arbitrary URI schemes.
func openURLHandler(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if err := exec.Command("open", u.String()).Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func portAlive(addr string) bool {
	resp, err := (&http.Client{Timeout: 300 * time.Millisecond}).Get("http://" + addr + "/api/fleet")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// bootstrapPATH makes shell-installed binaries (brew, npm globals,
// claude, tmux) reachable when the .app is launched from Finder/Dock.
// Apps launched outside a terminal inherit a tiny PATH (/usr/bin:/bin
// only) — we both prepend our bundle dir AND ask the user's login
// shell what their real PATH looks like, then merge them.
func bootstrapPATH() {
	var parts []string
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			parts = append(parts, filepath.Dir(real))
		}
	}
	if shell := os.Getenv("SHELL"); shell != "" {
		out, err := exec.Command(shell, "-l", "-c", "printf %s \"$PATH\"").Output()
		if err == nil && len(out) > 0 {
			parts = append(parts, string(out))
		}
	}
	parts = append(parts, os.Getenv("PATH"))

	seen := map[string]bool{}
	var deduped []string
	for _, chunk := range parts {
		for _, p := range strings.Split(chunk, ":") {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			deduped = append(deduped, p)
		}
	}
	os.Setenv("PATH", strings.Join(deduped, ":"))
}

// prereq describes a tool the user needs. The setup page renders one
// card per missing prereq with the install command and a button that
// opens Terminal pre-loaded with that command.
type prereq struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Why     string `json:"why"`
	Fix     string `json:"fix"`              // shell command to run
	DocsURL string `json:"docs_url,omitempty"`
	Note    string `json:"note,omitempty"`

	// Optional: render an inline text input + submit button. Used by
	// the Director auth step where the user pastes a setup-token after
	// running `claude setup-token` in Terminal.
	AcceptsInput     bool   `json:"accepts_input,omitempty"`
	InputPlaceholder string `json:"input_placeholder,omitempty"`
	InputSubmitURL   string `json:"input_submit_url,omitempty"`
}

func checkPrereqs() []prereq {
	var missing []prereq
	if _, err := exec.LookPath("tmux"); err != nil {
		missing = append(missing, prereq{
			ID:      "tmux",
			Label:   "tmux",
			Why:     "Director runs each agent in its own tmux session.",
			Fix:     "brew install tmux",
			DocsURL: "https://github.com/tmux/tmux/wiki/Installing",
		})
	}
	// Note: node is intentionally NOT a hard prereq anymore. The
	// official Claude Code installer is self-contained (drops a native
	// binary at ~/.local/bin/claude), and Director's other parts don't
	// need node directly. agent-browser is a separate npm install
	// triggered later only if the user opts into it.
	claudePath := findClaude()
	if claudePath == "" {
		missing = append(missing, prereq{
			ID:    "claude",
			Label: "Claude Code CLI",
			Why:   "The CLI Director drives. The official installer drops a native binary into ~/.local/bin and works without Node.",
			Fix:   "curl -fsSL https://claude.ai/install.sh | bash",
			Note:  "After install, run `claude` once and complete the login flow before clicking Recheck.",
			DocsURL: "https://docs.claude.com/en/docs/claude-code/installation",
		})
	} else if !directorAuthConfigured() {
		missing = append(missing, prereq{
			ID:    "director-auth-token",
			Label: "Director auth token",
			Why:   "Director runs many orchs in parallel. To avoid token-rotation conflicts, every orch shares one long-lived setup-token instead of using your interactive Claude session.",
			Fix:   "claude setup-token",
			Note:  "After the browser flow finishes, copy the token from your terminal and paste it below.",
			AcceptsInput:    true,
			InputPlaceholder: "sk-ant-oat01-…",
			InputSubmitURL:   "/__setup/auth-token",
		})
	}
	return missing
}

// findClaude searches PATH first, then common install locations the
// official installer / npm-via-various-node-managers drop the binary
// into. Returns the absolute path on success, "" on miss. As a side
// effect, when found via fallback, prepends the binary's dir to PATH
// so subprocess spawns also see it without the user having to edit
// their shell rc.
func findClaude() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local/bin/claude"),         // Anthropic native installer
		filepath.Join(home, ".npm-global/bin/claude"),    // npm with custom prefix
		filepath.Join(home, ".asdf/shims/claude"),        // asdf
		filepath.Join(home, ".volta/bin/claude"),         // volta
		filepath.Join(home, ".fnm/aliases/default/bin/claude"), // fnm
		"/opt/homebrew/bin/claude",                        // Homebrew (Apple Silicon)
		"/usr/local/bin/claude",                           // Homebrew (Intel) / legacy
	}
	for _, p := range candidates {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.Mode()&0o111 == 0 {
			continue // not executable
		}
		// Prepend the dir to PATH so future subprocesses see it too.
		dir := filepath.Dir(p)
		os.Setenv("PATH", dir+":"+os.Getenv("PATH"))
		return p
	}
	return ""
}

// directorAuthConfigured returns true when the user has run
// `claude setup-token` and pasted the resulting token into Director.
// The token is the per-machine OAuth credential every spawned orch
// uses (see roster's directorOAuthToken in claudedir.go).
func directorAuthConfigured() bool {
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", "Director-OAuth-Token", "-a", os.Getenv("USER"))
	return cmd.Run() == nil
}

// sniffTokenHandler reads the contents of every open Terminal.app
// tab via AppleScript, looks for an `sk-ant-oat01-…` setup-token
// pattern, and (if found) stores it the same way the manual paste
// form does. The setup page polls this endpoint every ~2s after the
// user clicks "Open Terminal" so they don't actually have to copy
// anything — Director scrapes the token off-screen the moment it
// appears.
//
// Why scrape vs. capture stdout: claude-code's setup-token command
// drives an OAuth browser flow, which we can't take over. The simplest
// robust capture point is the user's own terminal where the command
// already prints the token they would otherwise paste manually.
func sniffTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	// Pull contents of every Terminal tab in every window. AppleScript
	// joins them with a delimiter we won't see in real output so we can
	// scan the whole blob with one regex.
	// Use `history of t` (the scrollback string) rather than
	// `contents of t` — the latter returns a class that AppleScript
	// can't always coerce to Unicode text and errors out (-1700).
	const sep = "\u0001---DIR-TAB-SEP---\u0001"
	script := fmt.Sprintf(`set out to ""
tell application "Terminal"
  if (count of windows) is 0 then return ""
  repeat with w in windows
    repeat with t in tabs of w
      try
        set out to out & (history of t) & "%s"
      end try
    end repeat
  end repeat
end tell
return out`, sep)
	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.Output()
	if err != nil {
		// Terminal not running, no Apple-Events permission yet, etc —
		// just return "no token", the UI will keep polling.
		writeTokenSniff(w, "", "")
		return
	}
	match := extractSetupToken(string(out))
	if match == "" {
		writeTokenSniff(w, "", "")
		return
	}
	// Persist via roster, same as the manual paste path.
	rosterPath, err := exec.LookPath("roster")
	if err != nil {
		writeTokenSniff(w, "", "roster CLI not on PATH")
		return
	}
	setCmd := exec.Command(rosterPath, "auth", "set", match)
	if outBytes, err := setCmd.CombinedOutput(); err != nil {
		writeTokenSniff(w, "", strings.TrimSpace(string(outBytes)))
		return
	}
	writeTokenSniff(w, match, "")
}

// extractSetupToken pulls a complete sk-ant-oat01-… token from a chunk
// of arbitrary terminal scrollback.
//
// `claude setup-token` prints the token on its own line, sandwiched
// between blank lines:
//
//   Your OAuth token (valid for 1 year):
//
//   sk-ant-oat01-AAA…AAA
//
//   Store this token securely.
//
// Terminal hard-wraps long lines at the column width — so if the
// token is longer than the terminal is wide, a literal `\n` lands
// mid-token. We have to skip THAT newline (a "soft wrap") but stop
// at a paragraph break (a blank line, or a line with only whitespace).
//
// Strategy: anchor on `sk-ant-oat01-` (only appears in real tokens —
// the "Use this token by setting: export CLAUDE_CODE_OAUTH_TOKEN=…"
// line uses a `<token>` placeholder). Walk forward; valid token chars
// append; whitespace runs are checked — a single `\n` is a soft wrap
// (skip), but `\n` followed by a blank line (or any non-token-char
// after whitespace) ends the token. Use LastIndex so re-runs of
// setup-token always pick the most recent.
func extractSetupToken(s string) string {
	const tokenPrefix = "sk-ant-oat01-"
	const minLen = 50
	const maxLen = 200

	idx := strings.LastIndex(s, tokenPrefix)
	if idx < 0 {
		return ""
	}
	tail := s[idx+len(tokenPrefix):]

	var buf strings.Builder
	buf.WriteString(tokenPrefix)

	isTokenChar := func(r rune) bool {
		return (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
	}

walk:
	for i := 0; i < len(tail) && buf.Len() < maxLen; i++ {
		r := rune(tail[i])
		if isTokenChar(r) {
			buf.WriteRune(r)
			continue
		}
		// Whitespace: peek ahead. A soft wrap is a single `\n` (or
		// `\r\n`) immediately followed by another token char on the
		// next line. Anything else (blank line, two newlines, line
		// with only spaces, etc) terminates the token.
		if r == ' ' || r == '\t' || r == '\r' {
			continue
		}
		if r == '\n' {
			j := i + 1
			for j < len(tail) {
				nx := tail[j]
				if nx == ' ' || nx == '\t' || nx == '\r' {
					j++
					continue
				}
				if nx == '\n' {
					// Blank line → paragraph break → end of token.
					break walk
				}
				if isTokenChar(rune(nx)) {
					// Soft wrap: jump i forward past the whitespace
					// run; the outer loop's i++ then lands on the
					// next token char.
					i = j - 1
					continue walk
				}
				// Non-token, non-whitespace next char → end.
				break walk
			}
			break walk
		}
		// Any other char → end.
		break walk
	}

	body := buf.String()
	if !strings.HasPrefix(body, tokenPrefix) || len(body) <= len(tokenPrefix) {
		return ""
	}
	if len(body) < minLen {
		return ""
	}
	return body
}

func writeTokenSniff(w http.ResponseWriter, token, errMsg string) {
	type resp struct {
		Token string `json:"token,omitempty"`
		Error string `json:"error,omitempty"`
		Found bool   `json:"found"`
	}
	w.Header().Set("Content-Type", "application/json")
	r := resp{Token: token, Error: errMsg, Found: token != ""}
	_ = json.NewEncoder(w).Encode(r)
}

// authTokenHandler accepts a JSON {token: "sk-ant-oat01-…"} body and
// stores it via `roster auth set` (which writes the keychain entry).
// Used by the setup page's inline form on the Director-auth step.
func authTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}
	rosterPath, err := exec.LookPath("roster")
	if err != nil {
		http.Error(w, "roster CLI not on PATH; restart Director after install", http.StatusInternalServerError)
		return
	}
	cmd := exec.Command(rosterPath, "auth", "set", body.Token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Surface roster's error message back to the UI so the user
		// sees "doesn't look like a setup-token" / similar.
		http.Error(w, strings.TrimSpace(string(out)), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"configured":true}`))
}

func (a *appState) recheckHandler(w http.ResponseWriter, r *http.Request) {
	// Re-bootstrap PATH every time. The user might have just finished
	// `brew install node` or the claude installer (which appends to
	// ~/.zshrc), and the dirs they added won't be in the PATH we
	// captured at startup. Sourcing the login shell again picks up
	// any rc-file edits.
	bootstrapPATH()
	missing := checkPrereqs()
	initStatus, initErr := a.getInit()

	resp := struct {
		Missing    []prereq `json:"missing"`
		Ready      bool     `json:"ready"`
		InitStatus string   `json:"init_status"`
		InitError  string   `json:"init_error,omitempty"`
	}{Missing: missing, InitStatus: initStatus, InitError: initErr}

	if len(missing) == 0 {
		if err := a.startDirectorServer(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Kick off init if we haven't yet, OR retry if the previous
		// attempt failed. Don't restart a "running" init — that would
		// race two `roster spawn`s against the same id.
		if initStatus == "" || initStatus == initFailed {
			go a.initDirector()
			resp.InitStatus = initRunning
		}
	}
	// Ready means user can leave the setup page: prereqs satisfied
	// AND dispatcher exists.
	resp.Ready = len(missing) == 0 && resp.InitStatus == initOK

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// runInTerminalHandler asks Terminal.app to open a new window and run
// the supplied command. The command is whitelisted to the install
// strings we render on the setup page so a hostile fetch can't run
// arbitrary shell.
//
// Race we have to handle: many users have oh-my-zsh's "would you like
// to update?" prompt fire at shell startup. AppleScript's `do script`
// types the command in immediately — if the prompt is still pending,
// the first character of our command gets eaten as the answer to the
// prompt. So we open Terminal first, give the shell a moment to fully
// initialize and dismiss any pending prompts (Enter, then sleep),
// THEN type the command.
func runInTerminalHandler(w http.ResponseWriter, r *http.Request) {
	cmd := r.URL.Query().Get("cmd")
	allowed := false
	for _, p := range checkPrereqs() {
		if p.Fix == cmd {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "command not on the current setup list", http.StatusBadRequest)
		return
	}
	// AppleScript escaping: backslash + double quote.
	esc := strings.ReplaceAll(cmd, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	// Two-stage: first activate Terminal with an empty `do script`
	// (which opens a new window and runs the user's normal shell init,
	// including any oh-my-zsh "update?" prompt). Then wait 1.2s for
	// the shell to settle and any pending prompt to render, send
	// Return to dismiss it, wait again, and finally type the command.
	// Result: the user's command lands at a clean prompt regardless of
	// what oh-my-zsh / starship / their own shell hooks were doing.
	script := fmt.Sprintf(`tell application "Terminal"
  activate
  set newTab to do script ""
  delay 1.2
  do script "" in newTab
  delay 0.4
  do script "%s" in newTab
end tell`, esc)
	if err := exec.Command("osascript", "-e", script).Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// initDirector ensures the "director" dispatcher exists. Updates
// a.initStatus (running → ok|failed) so the setup page can show
// progress and surface real errors. Captures stderr from `roster
// spawn` and appends to setup.log for post-mortem.
//
// Safe to call multiple times: dispatcherExists short-circuits when
// the agent is already in roster's registry.
func (a *appState) initDirector() {
	a.setInit(initRunning, "")
	logSetup("init: starting")

	// Wait up to 5s for director-server to bind its port before probing
	// /api/fleet. startDirectorServer already polls but a slow box could
	// race the goroutine that called us.
	for i := 0; i < 50 && !portAlive(backendAddr); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !portAlive(backendAddr) {
		msg := "fleetview backend never came up on " + backendAddr
		logSetup("init failed: " + msg)
		a.setInit(initFailed, msg)
		return
	}

	if dispatcherExists() {
		logSetup("init: dispatcher already exists")
		a.setInit(initOK, "")
		return
	}

	cmd := exec.Command("roster", "spawn", "director",
		"--kind", "dispatcher",
		"--display-name", "Director",
		"--description", "routes user requests")
	// Anchor at the data dir so the dispatcher's recorded cwd isn't '/'
	// (Finder/Dock launches inherit '/'). Every agent the dispatcher
	// later spawns inherits this in turn via the parent's cwd at
	// roster-spawn time.
	dir := directorDataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		msg := fmt.Sprintf("mkdir %s: %v", dir, err)
		logSetup("init failed: " + msg)
		a.setInit(initFailed, msg)
		return
	}
	cmd.Dir = dir
	var combined bytes.Buffer
	cmd.Stdout = io.MultiWriter(&combined, os.Stderr)
	cmd.Stderr = io.MultiWriter(&combined, os.Stderr)
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(combined.String())
		msg := out
		if msg == "" {
			msg = err.Error()
		}
		logSetup("init failed: roster spawn exited: " + msg)
		a.setInit(initFailed, msg)
		return
	}
	logSetup("init: dispatcher spawned ok")
	a.setInit(initOK, "")
}

// dispatcherExists asks the running fleetview whether any registered
// agent is a dispatcher. Cheap; we use it both as the idempotency
// check before spawning and as the heal signal on subsequent launches.
func dispatcherExists() bool {
	resp, err := http.Get(backendURL + "/api/fleet")
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return bytes.Contains(body, []byte(`"kind":"dispatcher"`))
}

// logSetup appends a timestamped line to setup.log in the Director
// data dir. Best-effort: errors are swallowed since this is purely
// for post-mortem when a friend reports "doesn't work."
func logSetup(line string) {
	dir := directorDataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "setup.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
}

// resetDirectorState wipes the per-machine bits that gate first-run
// so `--reset` can replay the setup flow without touching the user's
// actual orchs / artifacts. Targets:
//
//   - <data>/director (roster's per-orch claude config dir for
//     the dispatcher specifically — preserves other agents)
//   - <roster-data>/agents/director.json (registry entry)
//   - directorDataDir() / setup.log (so the new run starts a fresh log)
//   - ~/.agent-browser/director.* daemon files
//
// Also cleans up the legacy "dispatch" id from before the rename so
// users on v0.5.2 or earlier don't end up with two dispatchers in
// their registry after upgrading.
//
// We deliberately do NOT remove other orchs (hacker-news, etc.) or
// global plugin caches — those are user data, not setup state.
func resetDirectorState() error {
	rosterData := filepath.Join(os.Getenv("HOME"), ".local", "share", "roster")
	var candidates []string
	for _, id := range []string{"director", "dispatch"} {
		candidates = append(candidates,
			filepath.Join(rosterData, "claude", id),
			filepath.Join(rosterData, "agents", id+".json"),
			filepath.Join(os.Getenv("HOME"), ".agent-browser", id+".pid"),
			filepath.Join(os.Getenv("HOME"), ".agent-browser", id+".sock"),
			filepath.Join(os.Getenv("HOME"), ".agent-browser", id+".stream"),
			filepath.Join(os.Getenv("HOME"), ".agent-browser", id+".version"),
		)
	}
	candidates = append(candidates, filepath.Join(directorDataDir(), "setup.log"))
	for _, p := range candidates {
		_ = os.RemoveAll(p)
	}
	// Kill any tmux sessions named after the dispatcher, otherwise
	// `roster spawn director` would complain the target is taken.
	for _, id := range []string{"director", "dispatch"} {
		_ = exec.Command("amux", "kill", id).Run()
	}
	return nil
}

// setupHandler renders the setup page. We seed it with the current
// state (missing prereqs + init status) so the first paint is never
// ambiguous — no "loading…" flicker before the JS poll catches up.
func setupHandler(a *appState, w http.ResponseWriter, r *http.Request) {
	missing := checkPrereqs()
	missingJSON, _ := json.Marshal(missing)
	status, errMsg := "", ""
	if a != nil {
		status, errMsg = a.getInit()
	}
	stateJSON, _ := json.Marshal(map[string]any{
		"missing":     missing,
		"init_status": status,
		"init_error":  errMsg,
	})
	page := strings.ReplaceAll(setupHTML, "%MISSING%", string(missingJSON))
	page = strings.ReplaceAll(page, "%STATE%", string(stateJSON))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

const setupHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Director — Setup</title>
<style>
:root{
  --linen:#F5EFE6;
  --ink:#2A2620;
  --muted:#7A7268;
  --rule:#E0D7C9;
  --card:#FBF7F0;
  --accent:#7C8B5E;
}
*{box-sizing:border-box}
html,body{height:100%;margin:0;background:var(--linen);color:var(--ink);
  font-family:-apple-system,BlinkMacSystemFont,"SF Pro Text",sans-serif;
  -webkit-font-smoothing:antialiased;}
.__wails_drag_region{position:fixed;top:0;left:0;right:0;height:28px;z-index:99;
  --wails-draggable:drag;-webkit-app-region:drag;cursor:default;
  user-select:none;-webkit-user-select:none;}
.wrap{max-width:520px;margin:0 auto;padding:56px 36px 28px}
h1{font-family:Georgia,"New York",serif;font-weight:400;font-size:32px;line-height:1.05;margin:0}
.sub{margin-top:10px;color:var(--muted);font-size:13.5px;line-height:1.5}
.list{margin-top:22px;display:flex;flex-direction:column;gap:10px}
.card{background:var(--card);border:1px solid var(--rule);border-radius:12px;padding:16px}
.row{display:flex;align-items:flex-start;justify-content:space-between;gap:14px}
.label{font-weight:600;font-size:14px}
.why{margin-top:4px;color:var(--muted);font-size:12.5px;line-height:1.5}
.fix{margin-top:12px;display:flex;align-items:center;gap:8px}
code{background:var(--linen);border:1px solid var(--rule);border-radius:6px;
  padding:6px 10px;font:12px ui-monospace,SFMono-Regular,Menlo,monospace;
  flex:1;min-width:0;overflow:hidden;white-space:nowrap;text-overflow:ellipsis}
button{background:var(--ink);color:var(--linen);border:0;border-radius:7px;
  padding:7px 12px;font:600 11px/1 -apple-system,sans-serif;letter-spacing:.06em;
  text-transform:uppercase;cursor:pointer;white-space:nowrap}
button.ghost{background:transparent;color:var(--ink);border:1px solid var(--rule)}
button:hover{opacity:.92}
.note{margin-top:8px;color:var(--muted);font-size:11.5px;line-height:1.5}
.footer{margin-top:18px;display:flex;justify-content:space-between;align-items:center;gap:12px}
.status{color:var(--muted);font-size:12.5px}
.allgood{margin-top:24px;text-align:center;color:var(--accent);font-size:15px}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}
.spinner{width:14px;height:14px;border-radius:50%;border:2px solid var(--rule);
  border-top-color:var(--accent);animation:spin .8s linear infinite;flex-shrink:0;margin-top:2px}
@keyframes spin{to{transform:rotate(360deg)}}
.errbox{margin-top:12px;background:var(--linen);border:1px solid var(--rule);
  border-radius:6px;padding:10px 12px;font:11.5px ui-monospace,SFMono-Regular,Menlo,monospace;
  white-space:pre-wrap;word-break:break-word;max-height:140px;overflow:auto;color:#8a3c2c}
input.tokeninput{flex:1;min-width:0;padding:8px 10px;border:1px solid var(--rule);
  border-radius:6px;font:12px ui-monospace,SFMono-Regular,Menlo,monospace;
  background:var(--linen);color:var(--ink);outline:none}
input.tokeninput:focus{border-color:var(--accent)}
.suberr{margin-top:8px;color:#8a3c2c;font-size:11.5px;line-height:1.4;word-break:break-word}
/* Slim scrollbars — the macOS default tracks looked massive against
   the linen card. Same treatment for any nested scrollers (errbox). */
::-webkit-scrollbar{width:6px;height:6px}
::-webkit-scrollbar-thumb{background:var(--rule);border-radius:3px}
::-webkit-scrollbar-track{background:transparent}
</style>
</head>
<body>
<div class="__wails_drag_region"></div>
<div class="wrap">
  <h1>Almost there.</h1>
  <p class="sub">Director drives Claude Code on your machine and needs a few tools installed
  before it can spawn agents. We'll point you at each one — click <em>Install</em> to open
  Terminal with the command pre-loaded.</p>
  <div id="list" class="list"></div>
  <div class="footer">
    <span class="status" id="status"></span>
    <button id="recheck">Recheck</button>
  </div>
</div>
<script>
const initialState = %STATE%;
let busy = false;
let polling = false;

function render(state){
  const list = document.getElementById('list');
  list.innerHTML = '';
  const missing = state.missing || [];
  const initStatus = state.init_status || '';
  const initError = state.init_error || '';

  if (missing.length === 0 && initStatus === 'ok') {
    list.innerHTML = '<div class="allgood">All set. Loading Director…</div>';
    setTimeout(() => location.reload(), 600);
    return;
  }

  // Stage 1: prereq cards. Two flavors:
  //   - shell-fix card: opens Terminal with a command (default).
  //   - input card: shell-fix PLUS an inline text input that POSTs the
  //     pasted value to a server endpoint (Director auth-token step).
  for (const p of missing) {
    const card = document.createElement('div');
    card.className = 'card';
    const inputBlock = p.accepts_input ? ` + "`" + `
      <div class="fix" style="margin-top:10px">
        <input type="password" class="tokeninput" placeholder="${(p.input_placeholder || '').replace(/"/g,'&quot;')}" autocomplete="off" spellcheck="false" />
        <button data-submit="${p.input_submit_url}">Save</button>
      </div>
      <div class="suberr" style="display:none"></div>
    ` + "`" + ` : '';
    card.innerHTML = ` + "`" + `
      <div class="row">
        <div>
          <div class="label">${p.label}</div>
          <div class="why">${p.why}</div>
        </div>
      </div>
      <div class="fix">
        <code>${p.fix}</code>
        <button data-cmd="${p.fix.replace(/"/g,'&quot;')}">Open Terminal</button>
        <button class="ghost" data-copy="${p.fix.replace(/"/g,'&quot;')}">Copy</button>
      </div>
      ${inputBlock}
      ${p.note ? '<div class="note">'+p.note+'</div>' : ''}
    ` + "`" + `;
    list.appendChild(card);
  }
  for (const b of list.querySelectorAll('button[data-cmd]')) {
    b.addEventListener('click', async () => {
      if (busy) return; busy = true;
      const cmd = b.getAttribute('data-cmd');
      await fetch('/__setup/run?cmd=' + encodeURIComponent(cmd));
      // If this card supports inline-paste (i.e. setup-token), the
      // command prints the token right back into the terminal. We
      // scrape the user's open Terminal tabs every ~2s for an
      // sk-ant-oat01-… match so they never have to copy + paste.
      const card = b.closest('.card');
      const acceptsInput = !!card.querySelector('input.tokeninput');
      if (acceptsInput) {
        setStatus('Opened Terminal. Complete the browser flow — Director will pick the token up automatically.');
        startTokenSniff();
      } else {
        setStatus('Opened Terminal — finish the install and click Recheck.');
      }
      busy = false;
    });
  }
  for (const b of list.querySelectorAll('button[data-copy]')) {
    b.addEventListener('click', async () => {
      const cmd = b.getAttribute('data-copy');
      await navigator.clipboard.writeText(cmd);
      const t = b.textContent; b.textContent = 'Copied'; setTimeout(()=>b.textContent=t, 1100);
    });
  }
  for (const b of list.querySelectorAll('button[data-submit]')) {
    b.addEventListener('click', async () => {
      if (busy) return;
      const card = b.closest('.card');
      const inp = card.querySelector('input.tokeninput');
      const err = card.querySelector('.suberr');
      const url = b.getAttribute('data-submit');
      const value = (inp.value || '').trim();
      if (!value) { inp.focus(); return; }
      busy = true;
      err.style.display = 'none';
      err.textContent = '';
      try {
        const r = await fetch(url, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ token: value }),
        });
        if (!r.ok) {
          err.textContent = (await r.text()).trim() || ('HTTP ' + r.status);
          err.style.display = 'block';
        } else {
          inp.value = '';
          recheck();
        }
      } finally { busy = false; }
    });
  }

  // Stage 2: dispatcher init card. Only renders when prereqs are
  // all satisfied so the user moves through one stage at a time.
  if (missing.length === 0) {
    const card = document.createElement('div');
    card.className = 'card';
    if (initStatus === 'failed') {
      card.innerHTML = ` + "`" + `
        <div class="row"><div>
          <div class="label">Setup failed</div>
          <div class="why">Director couldn't spawn its dispatcher. Most often this is a Claude Code login issue or a network blip during plugin install.</div>
        </div></div>
        <pre class="errbox">${escapeHTML(initError || 'unknown error')}</pre>
        <div class="fix"><button id="retry">Retry setup</button></div>
        <div class="note">Full log at <code>~/Library/Application Support/Director/setup.log</code></div>
      ` + "`" + `;
    } else {
      card.innerHTML = ` + "`" + `
        <div class="row"><div>
          <div class="label">Setting up Director…</div>
          <div class="why">Spawning the dispatcher and installing default plugins. This takes ~30s on first launch.</div>
        </div><div class="spinner"></div></div>
      ` + "`" + `;
    }
    list.appendChild(card);
    const retry = document.getElementById('retry');
    if (retry) retry.addEventListener('click', () => recheck());
    if (initStatus !== 'failed') startPolling();
  }
}

function escapeHTML(s){
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

function setStatus(s){ document.getElementById('status').textContent = s; }

async function recheck(){
  if (busy) return; busy = true;
  setStatus('Checking…');
  try {
    const r = await fetch('/__setup/recheck');
    const d = await r.json();
    render(d);
    if (d.missing.length === 0) {
      setStatus(d.init_status === 'ok' ? '' : '');
    } else {
      setStatus('Still missing ' + d.missing.length + ' — finish the install above.');
    }
  } finally { busy = false; }
}

// Auto-poll while the dispatcher is initializing so the user sees
// the transition without clicking Recheck. Stops as soon as init
// reaches a terminal state (ok or failed).
function startPolling(){
  if (polling) return;
  polling = true;
  (async function loop(){
    while (polling) {
      await new Promise(r => setTimeout(r, 1500));
      try {
        const r = await fetch('/__setup/recheck');
        const d = await r.json();
        render(d);
        if (d.init_status === 'ok' || d.init_status === 'failed' || d.missing.length > 0) {
          polling = false;
          return;
        }
      } catch (e) { /* keep polling */ }
    }
  })();
}

// Token sniffer — polls the AppleScript-backed Terminal scrape until
// it finds an sk-ant-oat01-… token. Idempotent (called once per
// "Open Terminal" click), self-cancelling on success or 60s timeout.
let sniffing = false;
function startTokenSniff(){
  if (sniffing) return;
  sniffing = true;
  const startedAt = Date.now();
  (async function loop(){
    while (sniffing) {
      await new Promise(r => setTimeout(r, 1800));
      if (Date.now() - startedAt > 90_000) { sniffing = false; return; }
      try {
        const r = await fetch('/__setup/sniff-token');
        const d = await r.json();
        if (d.found) {
          sniffing = false;
          setStatus('Token captured — finishing setup…');
          recheck();
          return;
        }
      } catch (e) { /* keep polling */ }
    }
  })();
}

document.getElementById('recheck').addEventListener('click', recheck);
render(initialState);
</script>
</body>
</html>`

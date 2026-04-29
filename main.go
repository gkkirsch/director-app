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
// webview content comes from our reverse-proxied fleetview server. A
// placeholder file keeps the embed pattern valid.
//

const (
	backendAddr = "127.0.0.1:8080"
	backendURL  = "http://" + backendAddr
)

func main() {
	bootstrapPATH()
	// Chdir to ~/Flow at startup. Finder/Dock-launched .apps inherit
	// cwd=/ from launchd, and any subprocess we fork (fleetview,
	// `roster spawn` from ensureDispatcher) inherits it too. That
	// turns every \$PWD-relative write inside an agent into a
	// "/file: read-only" error. Anchor the whole process tree at
	// the writable Flow data dir.
	dataDir := flowDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err == nil {
		_ = os.Chdir(dataDir)
	}

	backend, _ := url.Parse(backendURL)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.ModifyResponse = injectDragRegion

	app := &appState{proxy: proxy}
	defer app.shutdown()

	// If prereqs already pass at launch, spawn fleetview eagerly.
	// Otherwise the setup page handles the wait → recheck → spawn flow.
	if len(checkPrereqs()) == 0 {
		if err := app.startFleetview(); err != nil {
			fmt.Fprintln(os.Stderr, "fleet-app:", err)
			os.Exit(1)
		}
		go ensureDispatcher()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__open", openURLHandler)
	mux.HandleFunc("/__setup/recheck", app.recheckHandler)
	mux.HandleFunc("/__setup/run", runInTerminalHandler)
	mux.HandleFunc("/", app.dispatch)

	err := wails.Run(&options.App{
		Title:     "Flow",
		Width:     1280,
		Height:    820,
		MinWidth:  720,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			// Handler is the source of truth — everything proxies to
			// fleetview, including /. Skip Assets entirely so Wails
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

// appState wires together the lazily-spawned fleetview backend, the
// reverse proxy in front of it, and a per-request dispatcher that
// picks between "show setup page" and "proxy to fleetview" based on
// whether prereqs are satisfied yet.
type appState struct {
	mu    sync.Mutex
	cmd   *exec.Cmd
	ready bool
	proxy *httputil.ReverseProxy
}

// dispatch routes "/" — the setup page when fleetview hasn't started
// yet, otherwise the proxy to fleetview.
func (a *appState) dispatch(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	ready := a.ready
	a.mu.Unlock()
	if !ready {
		setupHandler(w, r)
		return
	}
	a.proxy.ServeHTTP(w, r)
}

// startFleetview locates the bundled binary, spawns it, and waits up
// to 5s for it to bind backendAddr. Idempotent — calling twice is a
// no-op once fleetview is up.
func (a *appState) startFleetview() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready {
		return nil
	}
	if portAlive(backendAddr) {
		a.ready = true
		return nil
	}
	bin, err := findFleetview()
	if err != nil {
		return fmt.Errorf("fleetview not found and nothing is listening on %s", backendAddr)
	}
	cmd := exec.CommandContext(context.Background(), bin)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Force a sensible cwd. When the .app is launched by Finder/Dock,
	// fleetview otherwise inherits "/" — and every subsequent
	// `roster spawn` (called by the dispatcher / orchestrators) inherits
	// that too, which means agents try to mkdir under "/" and hit
	// "Read-only file system." Anchor everything at ~/Flow so artifacts
	// land somewhere writable and discoverable.
	cmd.Dir = flowDataDir()
	if err := os.MkdirAll(cmd.Dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "fleet-app: mkdir %s: %v\n", cmd.Dir, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start fleetview: %w", err)
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
	return fmt.Errorf("fleetview did not bind %s within 5s", backendAddr)
}

func (a *appState) shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Signal(os.Interrupt)
		_, _ = a.cmd.Process.Wait()
	}
}

// flowDataDir is where Flow's spawned agents live and write things.
// Defaults to ~/Flow. Honors $FLOW_HOME if set so users can move the
// data dir elsewhere without recompiling.
func flowDataDir() string {
	if d := os.Getenv("FLOW_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Flow")
}

// findFleetview prefers the sibling binary inside the .app bundle
// (build.sh drops it there), then falls back to PATH for dev workflow.
func findFleetview() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			sibling := filepath.Join(filepath.Dir(real), "fleetview")
			if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return sibling, nil
			}
		}
	}
	return exec.LookPath("fleetview")
}

// dragSnippet is a tiny HTML overlay we splice into every HTML response
// from fleetview. Wails interprets `--wails-draggable: drag` as a window
// drag handle, so this 28px strip across the top makes the window draggable
// without us having to ship custom CSS in fleetview itself. Buttons start
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
}

func checkPrereqs() []prereq {
	var missing []prereq
	if _, err := exec.LookPath("tmux"); err != nil {
		missing = append(missing, prereq{
			ID:      "tmux",
			Label:   "tmux",
			Why:     "Flow runs each agent in its own tmux session.",
			Fix:     "brew install tmux",
			DocsURL: "https://github.com/tmux/tmux/wiki/Installing",
		})
	}
	// Note: node is intentionally NOT a hard prereq anymore. The
	// official Claude Code installer is self-contained (drops a native
	// binary at ~/.local/bin/claude), and Flow's other parts don't
	// need node directly. agent-browser is a separate npm install
	// triggered later only if the user opts into it.
	claudePath := findClaude()
	if claudePath == "" {
		missing = append(missing, prereq{
			ID:    "claude",
			Label: "Claude Code CLI",
			Why:   "The CLI Flow drives. The official installer drops a native binary into ~/.local/bin and works without Node.",
			Fix:   "curl -fsSL https://claude.ai/install.sh | bash",
			Note:  "After install, run `claude` once and complete the login flow before clicking Recheck.",
			DocsURL: "https://docs.claude.com/en/docs/claude-code/installation",
		})
	} else if !claudeAuthenticated() {
		missing = append(missing, prereq{
			ID:    "claude-login",
			Label: "Claude Code login",
			Why:   "Flow uses your Claude Code session. Run the CLI once and finish the login.",
			Fix:   "claude",
			Note:  "Quit it (Ctrl-C) once you see the prompt come back, then click Recheck.",
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

// claudeAuthenticated returns true if the user has a Claude Code
// keychain entry. The CLI writes "Claude Code-credentials" after a
// successful login.
func claudeAuthenticated() bool {
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", "Claude Code-credentials", "-a", os.Getenv("USER"))
	return cmd.Run() == nil
}

func (a *appState) recheckHandler(w http.ResponseWriter, r *http.Request) {
	// Re-bootstrap PATH every time. The user might have just finished
	// `brew install node` or the claude installer (which appends to
	// ~/.zshrc), and the dirs they added won't be in the PATH we
	// captured at startup. Sourcing the login shell again picks up
	// any rc-file edits.
	bootstrapPATH()
	missing := checkPrereqs()
	resp := struct {
		Missing []prereq `json:"missing"`
		Ready   bool     `json:"ready"`
	}{Missing: missing}
	if len(missing) == 0 {
		if err := a.startFleetview(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go ensureDispatcher()
		resp.Ready = true
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// runInTerminalHandler asks Terminal.app to open a new window and run
// the supplied command. The command is whitelisted to the install
// strings we render on the setup page so a hostile fetch can't run
// arbitrary shell.
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
	script := fmt.Sprintf(`tell application "Terminal"
  activate
  do script "%s"
end tell`, esc)
	if err := exec.Command("osascript", "-e", script).Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ensureDispatcher creates the default "dispatch" agent on first run
// so the user lands on a working surface instead of an empty fleet.
// roster's spawn command is idempotent on duplicate ids — the error
// ("already exists") is the expected path on every run after the first.
func ensureDispatcher() {
	// Wait for fleetview to be answering before trusting LookPath +
	// the API to be stable.
	for i := 0; i < 30; i++ {
		if portAlive(backendAddr) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	resp, err := http.Get(backendURL + "/api/fleet")
	if err == nil && resp != nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if bytes.Contains(body, []byte(`"kind":"dispatcher"`)) {
			return
		}
	}
	cmd := exec.Command("roster", "spawn", "dispatch",
		"--kind", "dispatcher",
		"--description", "routes user requests")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func setupHandler(w http.ResponseWriter, r *http.Request) {
	missing := checkPrereqs()
	data, _ := json.Marshal(missing)
	page := strings.ReplaceAll(setupHTML, "%MISSING%", string(data))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

const setupHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Flow — Setup</title>
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
.wrap{max-width:560px;margin:0 auto;padding:80px 40px 40px}
h1{font-family:Georgia,"New York",serif;font-weight:400;font-size:38px;line-height:1;margin:0}
.sub{margin-top:14px;color:var(--muted);font-size:14.5px;line-height:1.5}
.list{margin-top:32px;display:flex;flex-direction:column;gap:14px}
.card{background:var(--card);border:1px solid var(--rule);border-radius:14px;padding:20px}
.row{display:flex;align-items:baseline;justify-content:space-between;gap:16px}
.label{font-weight:600;font-size:15px}
.why{margin-top:6px;color:var(--muted);font-size:13px;line-height:1.5}
.fix{margin-top:14px;display:flex;align-items:center;gap:8px}
code{background:var(--linen);border:1px solid var(--rule);border-radius:6px;
  padding:6px 10px;font:13px ui-monospace,SFMono-Regular,Menlo,monospace;
  flex:1;min-width:0;overflow:auto;white-space:nowrap}
button{background:var(--ink);color:var(--linen);border:0;border-radius:8px;
  padding:8px 14px;font:600 12px/1 -apple-system,sans-serif;letter-spacing:.06em;
  text-transform:uppercase;cursor:pointer;white-space:nowrap}
button.ghost{background:transparent;color:var(--ink);border:1px solid var(--rule)}
button:hover{opacity:.92}
.note{margin-top:10px;color:var(--muted);font-size:12.5px;line-height:1.5}
.footer{margin-top:28px;display:flex;justify-content:space-between;align-items:center;gap:12px}
.status{color:var(--muted);font-size:13px}
.allgood{margin-top:32px;text-align:center;color:var(--accent);font-size:16px}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="__wails_drag_region"></div>
<div class="wrap">
  <h1>Almost there.</h1>
  <p class="sub">Flow drives Claude Code on your machine and needs a few tools installed
  before it can spawn agents. We'll point you at each one — click <em>Install</em> to open
  Terminal with the command pre-loaded.</p>
  <div id="list" class="list"></div>
  <div class="footer">
    <span class="status" id="status"></span>
    <button id="recheck">Recheck</button>
  </div>
</div>
<script>
const initial = %MISSING%;
let busy = false;

function render(items){
  const list = document.getElementById('list');
  list.innerHTML = '';
  if (items.length === 0) {
    list.innerHTML = '<div class="allgood">All set. Loading Flow…</div>';
    setTimeout(() => location.reload(), 800);
    return;
  }
  for (const p of items) {
    const card = document.createElement('div');
    card.className = 'card';
    card.innerHTML = ` + "`" + `
      <div class="row">
        <div>
          <div class="label">${p.label}</div>
          <div class="why">${p.why}</div>
        </div>
      </div>
      <div class="fix">
        <code>${p.fix}</code>
        <button data-cmd="${p.fix.replace(/"/g,'&quot;')}">Install</button>
        <button class="ghost" data-copy="${p.fix.replace(/"/g,'&quot;')}">Copy</button>
      </div>
      ${p.note ? '<div class="note">'+p.note+'</div>' : ''}
    ` + "`" + `;
    list.appendChild(card);
  }
  for (const b of list.querySelectorAll('button[data-cmd]')) {
    b.addEventListener('click', async () => {
      if (busy) return;
      busy = true;
      const cmd = b.getAttribute('data-cmd');
      await fetch('/__setup/run?cmd=' + encodeURIComponent(cmd));
      setStatus('Opened Terminal — finish the install and click Recheck.');
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
}

function setStatus(s){ document.getElementById('status').textContent = s; }

document.getElementById('recheck').addEventListener('click', async () => {
  if (busy) return; busy = true;
  setStatus('Checking…');
  try {
    const r = await fetch('/__setup/recheck');
    const d = await r.json();
    if (d.ready) { render([]); return; }
    render(d.missing);
    setStatus('Still missing ' + d.missing.length + ' — finish the install above.');
  } finally { busy = false; }
});

render(initial);
</script>
</body>
</html>`

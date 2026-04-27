package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd, err := ensureFleetview(backendAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-app:", err)
		os.Exit(1)
	}
	if cmd != nil {
		defer func() {
			_ = cmd.Process.Signal(os.Interrupt)
			_, _ = cmd.Process.Wait()
		}()
	}

	backend, _ := url.Parse(backendURL)
	proxy := httputil.NewSingleHostReverseProxy(backend)

	err = wails.Run(&options.App{
		Title:  "Fleet",
		Width:  1280,
		Height: 820,
		AssetServer: &assetserver.Options{
			// Handler is the source of truth — everything proxies to
			// fleetview, including /. Skip Assets entirely so Wails
			// doesn't intercept "/" → embedded index.html.
			Handler: proxy,
		},
		BackgroundColour: &options.RGBA{R: 245, G: 239, B: 230, A: 1}, // linen
		Mac: &mac.Options{
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                  false,
				HideTitleBar:               false,
				FullSizeContent:            false,
				UseToolbar:                 false,
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wails:", err)
		os.Exit(1)
	}
}

// ensureFleetview returns a running fleetview process if we had to
// spawn one, or (nil, nil) if something else was already listening on
// the address. Errors only when neither path works.
//
// Resolution order for the binary:
//  1. <app>.app/Contents/MacOS/fleetview — sibling of this exe.
//     The build script copies it there so the bundle is self-contained.
//  2. fleetview on $PATH — dev workflow + power users.
func ensureFleetview(addr string) (*exec.Cmd, error) {
	if portAlive(addr) {
		return nil, nil
	}
	bin, err := findFleetview()
	if err != nil {
		return nil, fmt.Errorf("fleetview not found and nothing is listening on %s; reinstall the app or run\n  curl -fsSL https://raw.githubusercontent.com/gkkirsch/fleet/main/install.sh | bash", addr)
	}
	cmd := exec.CommandContext(context.Background(), bin)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start fleetview: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if portAlive(addr) {
			return cmd, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("fleetview did not bind %s within 5s", addr)
}

// findFleetview walks the resolution order described on ensureFleetview.
func findFleetview() (string, error) {
	if exe, err := os.Executable(); err == nil {
		// Resolve symlinks so we get the real .app/Contents/MacOS path.
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			sibling := filepath.Join(filepath.Dir(real), "fleetview")
			if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return sibling, nil
			}
		}
	}
	return exec.LookPath("fleetview")
}

func portAlive(addr string) bool {
	resp, err := (&http.Client{Timeout: 300 * time.Millisecond}).Get("http://" + addr + "/api/fleet")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

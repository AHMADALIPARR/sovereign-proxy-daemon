// Package launcher starts a Chromium binary with the spec's flags and a
// --remote-debugging-pipe CDP transport (see cdp.Client for the framing).
package launcher

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"browser-engine/internal/cdp"
)

// Config for one browser launch.
type Config struct {
	Binary      string
	Proxy       string // e.g. socks5://127.0.0.1:1080
	SessionID   string // carried as the SOCKS5 username (daemon pins it)
	UserDataDir string
	Headless    bool
	ExtraFlags  []string
}

// Browser is a running Chromium with a CDP client attached.
type Browser struct {
	cmd    *exec.Cmd
	client *cdp.Client
	pipeW  *os.File
	pipeR  *os.File
}

// Flags builds the Chromium argument list (spec §2, verbatim flags first).
func Flags(cfg Config) []string {
	args := []string{}
	if cfg.Headless {
		args = append(args, "--headless=new")
	}
	// VM necessities on this box (documented, not part of the profile).
	args = append(args, "--no-sandbox", "--enable-unsafe-swiftshader")
	proxy := cfg.Proxy
	if cfg.SessionID != "" && proxy != "" {
		if u, err := url.Parse(proxy); err == nil && u.User == nil {
			u.User = url.User(cfg.SessionID)
			proxy = u.String()
		}
	}
	if proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}
	args = append(args, cfg.ExtraFlags...)
	args = append(args, "--remote-debugging-pipe")
	if cfg.UserDataDir != "" {
		args = append(args, "--user-data-dir="+cfg.UserDataDir)
	}
	args = append(args, "about:blank")
	return args
}

// Launch starts Chromium and returns a Browser with the CDP client ready.
func Launch(cfg Config) (*Browser, error) {
	if cfg.Binary == "" {
		return nil, fmt.Errorf("launcher: no chromium binary configured")
	}
	if cfg.UserDataDir == "" {
		d, err := os.MkdirTemp("", "sov-chromium-*")
		if err != nil {
			return nil, err
		}
		cfg.UserDataDir = d
	}
	// Pipe 1: parent writes -> child fd 3. Pipe 2: child fd 4 -> parent reads.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	resR, resW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.Binary, Flags(cfg)...)
	cmd.ExtraFiles = []*os.File{cmdR, resW}
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launcher: start %s: %w", cfg.Binary, err)
	}
	// The child owns the read end of pipe 1 and the write end of pipe 2 now.
	cmdR.Close()
	resW.Close()
	return &Browser{cmd: cmd, client: cdp.New(cmdW, resR), pipeW: cmdW, pipeR: resR}, nil
}

// CDP returns the protocol client.
func (b *Browser) CDP() *cdp.Client { return b.client }

// Close kills Chromium and closes the pipes.
func (b *Browser) Close() {
	if b.cmd != nil && b.cmd.Process != nil {
		b.cmd.Process.Kill()
		b.cmd.Wait()
	}
	b.client.Close()
	if b.pipeW != nil {
		b.pipeW.Close()
	}
	if b.pipeR != nil {
		b.pipeR.Close()
	}
}

// DefaultBinary locates the Playwright Chromium on this machine, or "".
func DefaultBinary() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".cache", "ms-playwright", "chromium-1254", "chrome-linux64", "chrome")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

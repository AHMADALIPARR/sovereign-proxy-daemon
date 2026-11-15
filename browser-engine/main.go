// Command browser-engine: launch Chromium through the proxy daemon with a
// mobile device profile applied over CDP, navigate, and verify the
// fingerprint the page actually sees.
//
// Usage:
//
//	browser-engine --config ../config.yaml --profile iphone17 \
//	    --session tab-7 --url https://browserleaks.com --verify
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"browser-engine/internal/cdp"
	"browser-engine/internal/launcher"

	"gopkg.in/yaml.v3"
)

type fileConfig struct {
	Browser struct {
		Binary     string   `yaml:"binary"`
		Proxy      string   `yaml:"proxy_server"`
		ExtraFlags []string `yaml:"extra_flags"`
	} `yaml:"browser"`
	Devices map[string]cdp.DeviceProfile `yaml:"devices"`
}

type check struct {
	Name     string      `json:"name"`
	Expected interface{} `json:"expected"`
	Actual   interface{} `json:"actual"`
	Pass     bool        `json:"pass"`
}

type report struct {
	Profile  string  `json:"profile"`
	Session  string  `json:"session"`
	URL      string  `json:"url"`
	Version  string  `json:"chromium_version"`
	Checks   []check `json:"checks"`
	AllPass  bool    `json:"all_pass"`
	PageText string  `json:"page_text_snippet,omitempty"`
}

func main() {
	cfgPath := flag.String("config", "../config.yaml", "path to config.yaml")
	profileName := flag.String("profile", "iphone17", "device profile name")
	sessionID := flag.String("session", "", "browser session id (SOCKS5 username)")
	urlFlag := flag.String("url", "about:blank", "URL to navigate to")
	verify := flag.Bool("verify", true, "evaluate the fingerprint and print a report")
	flag.Parse()

	data, err := os.ReadFile(*cfgPath)
	if err != nil {
		fatalf("read config: %v", err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		fatalf("parse config: %v", err)
	}
	prof, ok := fc.Devices[*profileName]
	if !ok {
		fatalf("no device profile %q in %s", *profileName, *cfgPath)
	}
	binary := fc.Browser.Binary
	if binary == "" {
		binary = launcher.DefaultBinary()
	}
	if binary == "" {
		fatalf("no chromium binary: set browser.binary in config or install the Playwright chromium")
	}

	br, err := launcher.Launch(launcher.Config{
		Binary:     binary,
		Proxy:      fc.Browser.Proxy,
		SessionID:  *sessionID,
		Headless:   true,
		ExtraFlags: fc.Browser.ExtraFlags,
	})
	if err != nil {
		fatalf("launch: %v", err)
	}
	defer br.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cli := br.CDP()

	verRaw, err := cli.Call(ctx, "", "Browser.getVersion", nil)
	if err != nil {
		fatalf("Browser.getVersion: %v", err)
	}
	var ver struct {
		Product string `json:"product"`
	}
	_ = json.Unmarshal(verRaw, &ver)

	tgtRaw, err := cli.Call(ctx, "", "Target.createTarget", map[string]interface{}{"url": "about:blank"})
	if err != nil {
		fatalf("Target.createTarget: %v", err)
	}
	var tgt struct {
		TargetID string `json:"targetId"`
	}
	_ = json.Unmarshal(tgtRaw, &tgt)

	attRaw, err := cli.Call(ctx, "", "Target.attachToTarget", map[string]interface{}{
		"targetId": tgt.TargetID, "flatten": true,
	})
	if err != nil {
		fatalf("Target.attachToTarget: %v", err)
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(attRaw, &att)

	if err := cdp.ApplyMobileProfile(ctx, cli, att.SessionID, prof); err != nil {
		fatalf("apply profile: %v", err)
	}

	if _, err := cli.Call(ctx, att.SessionID, "Page.navigate", map[string]interface{}{"url": *urlFlag}); err != nil {
		fatalf("Page.navigate: %v", err)
	}
	// Wait for the load event (best effort: the page may keep loading
	// subresources; Runtime.evaluate works either way after a short wait).
	loadCtx, loadCancel := context.WithTimeout(ctx, 30*time.Second)
	_, _ = cli.WaitForEvent(loadCtx, att.SessionID, "Page.loadEventFired")
	loadCancel()
	time.Sleep(2 * time.Second)

	rep := report{Profile: *profileName, Session: *sessionID, URL: *urlFlag, Version: ver.Product, AllPass: true}
	if *verify {
		rep.Checks = runChecks(ctx, cli, att.SessionID, prof)
		for _, c := range rep.Checks {
			if !c.Pass {
				rep.AllPass = false
			}
		}
		// Grab a snippet of the visible page text (lets the runner confirm
		// what an external audit page itself displayed).
		txtRaw, err := eval(ctx, cli, att.SessionID, `document.body ? document.body.innerText.slice(0, 600) : ""`)
		if err == nil {
			var v struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(txtRaw, &v) == nil {
				rep.PageText = v.Result.Value
			}
		}
	}

	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if *verify && !rep.AllPass {
		os.Exit(1)
	}
}

func runChecks(ctx context.Context, cli *cdp.Client, sessionID string, p cdp.DeviceProfile) []check {
	// One evaluate returning everything, so the numbers are from a single
	// page state, not spread across reloads.
	expr := `(function() {
  let webgl = {};
  try {
    const c = document.createElement('canvas');
    const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    if (gl) {
      const ext = gl.getExtension('WEBGL_debug_renderer_info');
      webgl.vendor = ext ? gl.getParameter(ext.UNMASKED_VENDOR_WEBGL) : gl.getParameter(37445);
      webgl.renderer = ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(37446);
    }
  } catch (e) {}
  return {
    userAgent: navigator.userAgent,
    platform: navigator.platform,
    hardwareConcurrency: navigator.hardwareConcurrency,
    maxTouchPoints: navigator.maxTouchPoints,
    webdriver: ('webdriver' in navigator) ? navigator.webdriver : 'absent',
    touch: ('ontouchstart' in window) || navigator.maxTouchPoints > 0,
    innerWidth: window.innerWidth,
    innerHeight: window.innerHeight,
    devicePixelRatio: window.devicePixelRatio,
    webglVendor: webgl.vendor || null,
    webglRenderer: webgl.renderer || null
  };
})()`
	raw, err := eval(ctx, cli, sessionID, expr)
	if err != nil {
		return []check{{Name: "evaluate", Expected: "ok", Actual: err.Error(), Pass: false}}
	}
	var v struct {
		Result struct {
			Value struct {
				UserAgent           string      `json:"userAgent"`
				Platform            string      `json:"platform"`
				HardwareConcurrency int         `json:"hardwareConcurrency"`
				MaxTouchPoints      int         `json:"maxTouchPoints"`
				Webdriver           interface{} `json:"webdriver"`
				Touch               bool        `json:"touch"`
				InnerWidth          int         `json:"innerWidth"`
				InnerHeight         int         `json:"innerHeight"`
				DevicePixelRatio    float64     `json:"devicePixelRatio"`
				WebGLVendor         interface{} `json:"webglVendor"`
				WebGLRenderer       interface{} `json:"webglRenderer"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return []check{{Name: "evaluate-parse", Expected: "ok", Actual: err.Error(), Pass: false}}
	}
	g := v.Result.Value
	mk := func(name string, expected, actual interface{}) check {
		return check{Name: name, Expected: expected, Actual: actual, Pass: fmt.Sprint(expected) == fmt.Sprint(actual)}
	}
	return []check{
		mk("userAgent", p.UserAgent, g.UserAgent),
		mk("platform", p.Platform, g.Platform),
		mk("hardwareConcurrency", p.HardwareConcurrency, g.HardwareConcurrency),
		mk("maxTouchPoints", p.MaxTouchPoints, g.MaxTouchPoints),
		mk("webdriver-absent-or-undefined", "absent", webdriverShown(g.Webdriver)),
		mk("touch", true, g.Touch),
		mk("innerWidth", p.Width, g.InnerWidth),
		mk("innerHeight", p.Height, g.InnerHeight),
		mk("devicePixelRatio", p.DeviceScaleFactor, g.DevicePixelRatio),
		mk("webglVendor", p.WebGLVendor, g.WebGLVendor),
		mk("webglRenderer", p.WebGLRenderer, g.WebGLRenderer),
	}
}

func webdriverShown(v interface{}) string {
	if v == nil {
		return "absent"
	}
	if b, ok := v.(bool); ok && !b {
		return "absent"
	}
	return fmt.Sprint(v)
}

func eval(ctx context.Context, cli *cdp.Client, sessionID, expr string) (json.RawMessage, error) {
	return cli.Call(ctx, sessionID, "Runtime.evaluate", map[string]interface{}{
		"expression":    expr,
		"returnByValue": true,
	})
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "browser-engine: "+format+"\n", args...)
	os.Exit(2)
}

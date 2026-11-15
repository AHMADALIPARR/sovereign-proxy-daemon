// Package emulation: the mobile device profile, applied to every new CDP
// target the engine creates (spec §3).
//
// NOTE ON LANGUAGE: the engine itself is Go. The spoof itself is a
// JavaScript snippet by necessity — CDP's
// Page.addScriptToEvaluateOnNewDocument injects code into the page's JS
// context; there is no other channel by which a browser page can be made to
// report different navigator/WebGL values. The snippet is data the engine
// sends, not part of the engine's stack.
package cdp

import (
	"context"
	"fmt"
)

// DeviceProfile is one entry of the config.yaml `devices:` map.
type DeviceProfile struct {
	UserAgent           string  `yaml:"user_agent"`
	Platform            string  `yaml:"platform"`
	Width               int     `yaml:"width"`
	Height              int     `yaml:"height"`
	DeviceScaleFactor   float64 `yaml:"device_scale_factor"`
	Mobile              bool    `yaml:"mobile"`
	Touch               bool    `yaml:"touch"`
	MaxTouchPoints      int     `yaml:"max_touch_points"`
	HardwareConcurrency int     `yaml:"hardware_concurrency"`
	WebGLVendor         string  `yaml:"webgl_vendor"`
	WebGLRenderer       string  `yaml:"webgl_renderer"`
}

// SpoofScript returns the JS injected before any page script runs. It pins
// the navigator/hardware/WebGL values to the profile.
func SpoofScript(p DeviceProfile) string {
	return fmt.Sprintf(`(() => {
  const d = (obj, prop, value) => {
    try { Object.defineProperty(obj, prop, { get: () => value, configurable: true }); } catch (e) {}
  };
  d(navigator, 'hardwareConcurrency', %d);
  d(navigator, 'maxTouchPoints', %d);
  d(navigator, 'platform', %q);
  d(navigator, 'vendor', 'Apple Computer, Inc.');
  d(Navigator.prototype, 'webdriver', undefined);
  const vendor = %q, renderer = %q;
  const patch = (proto) => {
    if (!proto) return;
    const orig = proto.getParameter;
    proto.getParameter = function(p) {
      if (p === 37445) return vendor;    // UNMASKED_VENDOR_WEBGL
      if (p === 37446) return renderer;  // UNMASKED_RENDERER_WEBGL
      return orig.call(this, p);
    };
  };
  patch(window.WebGLRenderingContext && window.WebGLRenderingContext.prototype);
  patch(window.WebGL2RenderingContext && window.WebGL2RenderingContext.prototype);
})();`,
		p.HardwareConcurrency, p.MaxTouchPoints, p.Platform, p.WebGLVendor, p.WebGLRenderer)
}

// ApplyMobileProfile sends the CDP emulation commands for a session.
// Order matters: the injection must be registered before navigation.
func ApplyMobileProfile(ctx context.Context, c *Client, sessionID string, p DeviceProfile) error {
	if _, err := c.Call(ctx, sessionID, "Page.enable", nil); err != nil {
		return fmt.Errorf("Page.enable: %w", err)
	}
	if _, err := c.Call(ctx, sessionID, "Runtime.enable", nil); err != nil {
		return fmt.Errorf("Runtime.enable: %w", err)
	}
	if _, err := c.Call(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument", map[string]interface{}{
		"source": SpoofScript(p),
	}); err != nil {
		return fmt.Errorf("addScriptToEvaluateOnNewDocument: %w", err)
	}
	uaMeta := map[string]interface{}{
		"brands": []map[string]interface{}{
			{"brand": "Apple Safari", "version": "17"},
			{"brand": "Safari", "version": "17"},
			{"brand": "Not-A.Brand", "version": "99"},
		},
		"fullVersionList": []map[string]interface{}{
			{"brand": "Apple Safari", "version": "17.0"},
			{"brand": "Safari", "version": "17.0"},
			{"brand": "Not-A.Brand", "version": "99.0.0.0"},
		},
		"platform":        p.Platform,
		"platformVersion": "17.0",
		"architecture":    "arm",
		"model":           "iPhone",
		"mobile":          true,
	}
	if _, err := c.Call(ctx, sessionID, "Emulation.setUserAgentOverride", map[string]interface{}{
		"userAgent":         p.UserAgent,
		"acceptLanguage":    "en-US,en;q=0.9",
		"platform":          p.Platform,
		"userAgentMetadata": uaMeta,
	}); err != nil {
		return fmt.Errorf("setUserAgentOverride: %w", err)
	}
	if _, err := c.Call(ctx, sessionID, "Emulation.setDeviceMetricsOverride", map[string]interface{}{
		"width":             p.Width,
		"height":            p.Height,
		"deviceScaleFactor": p.DeviceScaleFactor,
		"mobile":            p.Mobile,
	}); err != nil {
		return fmt.Errorf("setDeviceMetricsOverride: %w", err)
	}
	if p.Touch {
		if _, err := c.Call(ctx, sessionID, "Emulation.setTouchEmulationEnabled", map[string]interface{}{
			"enabled":        true,
			"maxTouchPoints": p.MaxTouchPoints,
		}); err != nil {
			return fmt.Errorf("setTouchEmulationEnabled: %w", err)
		}
	}
	return nil
}

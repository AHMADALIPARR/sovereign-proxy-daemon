package launcher

import (
	"strings"
	"testing"
)

func TestFlagsSpecOrder(t *testing.T) {
	args := Flags(Config{
		Binary:     "/bin/chrome",
		Proxy:      "socks5://127.0.0.1:1080",
		SessionID:  "tab-7",
		Headless:   true,
		ExtraFlags: []string{"--disable-setuid-sandbox", "--disable-web-security"},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--headless=new",
		"--proxy-server=socks5://tab-7@127.0.0.1:1080",
		"--disable-setuid-sandbox",
		"--disable-web-security",
		"--remote-debugging-pipe",
		"about:blank",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("flags %q missing %q", joined, want)
		}
	}
}

func TestFlagsNoSessionNoUser(t *testing.T) {
	args := Flags(Config{Proxy: "socks5://127.0.0.1:1080"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--proxy-server=socks5://127.0.0.1:1080") {
		t.Errorf("flags %q: proxy without session must stay bare", joined)
	}
	if strings.Contains(joined, "@127.0.0.1") {
		t.Errorf("flags %q: no session id, no username expected", joined)
	}
}

func TestFlagsNoProxy(t *testing.T) {
	args := Flags(Config{})
	for _, a := range args {
		if strings.HasPrefix(a, "--proxy-server=") {
			t.Errorf("flags %v: proxy flag present with no proxy configured", args)
		}
	}
}

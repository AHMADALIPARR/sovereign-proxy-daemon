package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeChromium answers one NUL-terminated request with a canned response,
// then echoes subsequent requests with their id.
func fakeChromium(t *testing.T, r *os.File, w *os.File) {
	t.Helper()
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 512)
		for {
			n, err := r.Read(tmp)
			if err != nil {
				return
			}
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.IndexByte(buf, 0x00)
				if i < 0 {
					break
				}
				raw := buf[:i]
				buf = buf[i+1:]
				var m map[string]interface{}
				if err := json.Unmarshal(raw, &m); err != nil {
					continue
				}
				id := int(m["id"].(float64))
				resp, _ := json.Marshal(map[string]interface{}{
					"id":     id,
					"result": map[string]interface{}{"ok": true, "method": m["method"]},
				})
				w.Write(append(resp, 0x00))
			}
		}
	}()
}

func TestCallRoundTrip(t *testing.T) {
	// client writes -> fake reads; fake writes -> client reads.
	c2fR, c2fW, _ := os.Pipe()
	f2cR, f2cW, _ := os.Pipe()
	fakeChromium(t, c2fR, f2cW)
	cli := New(c2fW, f2cR)
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cli.Call(ctx, "sess-1", "Runtime.evaluate", map[string]interface{}{"expression": "1+1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var v map[string]interface{}
	if err := json.Unmarshal(res, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v["ok"] != true || v["method"] != "Runtime.evaluate" {
		t.Fatalf("unexpected result: %v", v)
	}
}

func TestEventRouting(t *testing.T) {
	c2fR, c2fW, _ := os.Pipe()
	f2cR, f2cW, _ := os.Pipe()
	go func() {
		// consume whatever the client sends
		io.Copy(io.Discard, c2fR)
	}()
	cli := New(c2fW, f2cR)
	defer cli.Close()

	f2cW.Write([]byte(`{"method":"Page.loadEventFired","params":{"timestamp":1},"sessionId":"s1"}` + "\x00"))
	select {
	case ev := <-cli.Events():
		if ev.Method != "Page.loadEventFired" || ev.SessionID != "s1" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
	}
}

func TestSpoofScriptContainsProfile(t *testing.T) {
	p := DeviceProfile{
		Platform: "iPhone", HardwareConcurrency: 4, MaxTouchPoints: 5,
		WebGLVendor: "Apple Inc.", WebGLRenderer: "Apple GPU",
	}
	s := SpoofScript(p)
	for _, want := range []string{
		"hardwareConcurrency", "4",
		"maxTouchPoints", "5",
		"iPhone",
		"Apple Inc.", "Apple GPU",
		"37445", "37446",
		"webdriver",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("spoof script missing %q", want)
		}
	}
}

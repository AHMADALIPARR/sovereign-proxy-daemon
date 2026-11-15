package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func healthyPool(urls ...string) *ProxyPool {
	return NewProxyPool(urls)
}

func TestPoolNextSkipsUnhealthy(t *testing.T) {
	p := healthyPool("direct://a", "direct://b")
	p.nodes[0].IsHealthy.Store(false)
	for i := 0; i < 5; i++ {
		if got := p.Next(); got == nil || got.URL != "direct://b" {
			t.Fatalf("Next() = %v, want direct://b", got)
		}
	}
	p.nodes[1].IsHealthy.Store(false)
	if got := p.Next(); got != nil {
		t.Fatalf("Next() with all unhealthy = %v, want nil", got)
	}
}

func TestManagerPerSessionPin(t *testing.T) {
	p := healthyPool("direct://a", "direct://b", "direct://c")
	m := NewManager(p, "per_session", time.Second)

	first := m.Pick("tab-1")
	if first == nil {
		t.Fatal("Pick(tab-1) = nil")
	}
	for i := 0; i < 10; i++ {
		if got := m.Pick("tab-1"); got != first {
			t.Fatalf("session tab-1 moved %v -> %v, want pinned", first.URL, got.URL)
		}
	}
	// a session pinned to a node that dies must re-pick to a healthy one
	first.IsHealthy.Store(false)
	repicked := m.Pick("tab-1")
	if repicked == nil || repicked == first || !repicked.IsHealthy.Load() {
		t.Fatalf("dead pin repick = %v, want a different healthy node", repicked)
	}
}

func TestManagerIntervalRotation(t *testing.T) {
	p := healthyPool("direct://a", "direct://b")
	m := NewManager(p, "interval", 50*time.Millisecond)
	a := m.Pick("")
	if a == nil {
		t.Fatal("Pick() = nil")
	}
	// within the interval the shared node is stable
	if got := m.Pick(""); got != a {
		t.Fatalf("interval node changed early: %v -> %v", a.URL, got.URL)
	}
	time.Sleep(70 * time.Millisecond)
	b := m.Pick("")
	if b == nil || b == a {
		t.Fatalf("interval node did not rotate after expiry: %v", b)
	}
}

func TestManagerUnknownModeDefaultsPerSession(t *testing.T) {
	p := healthyPool("direct://a")
	m := NewManager(p, "bogus", 0)
	if m.Mode() != "per_session" {
		t.Fatalf("Mode() = %q, want per_session", m.Mode())
	}
}

func TestCheckNodeHTTPUpstreamCheckedThroughProxy(t *testing.T) {
	// Local 200 target.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer ln.Close()
	target := "http://" + ln.Addr().String()

	// Local HTTP-proxy stub: CONNECT -> dial target -> relay.
	stub, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stub.Close()
	go func() {
		for {
			c, err := stub.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				parts := strings.Fields(line)
				if len(parts) < 2 || parts[0] != "CONNECT" {
					return
				}
				for {
					l, err := br.ReadString('\n')
					if err != nil || l == "\r\n" {
						break
					}
				}
				up, err := net.DialTimeout("tcp", parts[1], 5*time.Second)
				if err != nil {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
					return
				}
				defer up.Close()
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { io.Copy(c, up); done <- struct{}{} }()
				go func() { io.Copy(up, c); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()

	good := &ProxyNode{URL: "http://" + stub.Addr().String()}
	checkNode(good, target, 10*time.Second)
	if !good.IsHealthy.Load() {
		t.Fatal("http upstream via working CONNECT proxy marked unhealthy, want healthy")
	}
	if good.LatencyMs.Load() < 0 {
		t.Fatal("http upstream latency not recorded")
	}

	dead := &ProxyNode{URL: "http://127.0.0.1:19998"}
	checkNode(dead, target, 5*time.Second)
	if dead.IsHealthy.Load() {
		t.Fatal("dead http upstream marked healthy, want unhealthy")
	}
}

func TestCheckNodeDirectHealthyDeadUnhealthy(t *testing.T) {
	// NOTE: on this VM a direct dial to the public internet does not work —
	// the sandbox only lets traffic out through its own HTTP proxy (a bare
	// TLS ClientHello to api.ipify.org:443 gets a non-TLS answer). So the
	// healthy-path check uses a local HTTP target (the checker logic is
	// identical: 200 -> healthy, latency recorded), and the dead-node check
	// uses the real target (it fails at TCP connect, no egress needed).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer ln.Close()

	direct := &ProxyNode{URL: "direct://"}
	checkNode(direct, "http://"+ln.Addr().String(), 10*time.Second)
	if !direct.IsHealthy.Load() {
		t.Fatal("direct:// node marked unhealthy against a local 200 target, want healthy")
	}
	if direct.LatencyMs.Load() < 0 {
		t.Fatal("direct:// latency not recorded")
	}
	dead := &ProxyNode{URL: "socks5://127.0.0.1:19999"}
	checkNode(dead, "https://api.ipify.org", 5*time.Second)
	if dead.IsHealthy.Load() {
		t.Fatal("dead socks5 node marked healthy, want unhealthy")
	}
}

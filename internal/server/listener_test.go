package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"sovereign-proxy-daemon/internal/proxy"

	xproxy "golang.org/x/net/proxy"
)

// echoServer serves one-line echo on a random local port.
func echoServer(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// stubSOCKS5 is a minimal SOCKS5 upstream that dials the target directly and
// relays. It proves the daemon can route through a real SOCKS5 upstream.
func stubSOCKS5(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(br, hdr); err != nil {
					return
				}
				m := make([]byte, int(hdr[1]))
				io.ReadFull(br, m)
				c.Write([]byte{0x05, 0x00})
				req := make([]byte, 4)
				if _, err := io.ReadFull(br, req); err != nil {
					return
				}
				// ATYP is req[3]; the address bytes follow it.
				var host string
				switch req[3] {
				case 0x01:
					b := make([]byte, 4)
					io.ReadFull(br, b)
					host = net.IP(b).String()
				case 0x03:
					l := make([]byte, 1)
					io.ReadFull(br, l)
					b := make([]byte, int(l[0]))
					io.ReadFull(br, b)
					host = string(b)
				default:
					return
				}
				port := make([]byte, 2)
				io.ReadFull(br, port)
				target := net.JoinHostPort(host, fmt.Sprintf("%d", int(port[0])<<8|int(port[1])))
				up, err := net.DialTimeout("tcp", target, 5*time.Second)
				if err != nil {
					c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				done := make(chan struct{}, 2)
				go func() { io.Copy(c, up); done <- struct{}{} }()
				go func() { io.Copy(up, c); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func startDaemon(t *testing.T, mode string, urls []string) string {
	t.Helper()
	pool := proxy.NewProxyPool(urls)
	mgr := proxy.NewManager(pool, mode, 30*time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	srv := New(addr, mgr)
	go srv.ListenAndServe()
	time.Sleep(100 * time.Millisecond)
	return addr
}

// socks5DialThrough opens a SOCKS5 CONNECT via daemonAddr to target and
// returns the tunnelled conn.
func socks5DialThrough(t *testing.T, daemonAddr, target string, user string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", daemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		c.Write([]byte{0x05, 0x01, 0x02})
		resp := make([]byte, 2)
		io.ReadFull(c, resp)
		if !bytes.Equal(resp, []byte{0x05, 0x02}) {
			t.Fatalf("auth method not accepted: %v", resp)
		}
		c.Write([]byte{0x01, byte(len(user))})
		c.Write([]byte(user))
		c.Write([]byte{0x00})
		io.ReadFull(c, resp)
		if !bytes.Equal(resp, []byte{0x01, 0x00}) {
			t.Fatalf("auth failed: %v", resp)
		}
	} else {
		c.Write([]byte{0x05, 0x01, 0x00})
		resp := make([]byte, 2)
		io.ReadFull(c, resp)
		if !bytes.Equal(resp, []byte{0x05, 0x00}) {
			t.Fatalf("no-auth not accepted: %v", resp)
		}
	}
	host, port, _ := net.SplitHostPort(target)
	u, _ := url.Parse("//" + host)
	_ = u
	var req []byte
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		req = []byte{0x05, 0x01, 0x00, 0x01}
		req = append(req, ip.To4()...)
	} else {
		req = []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
		req = append(req, host...)
	}
	p := 0
	fmt.Sscanf(port, "%d", &p)
	req = append(req, byte(p>>8), byte(p))
	c.Write(req)
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("SOCKS5 reply read: %v", err)
	}
	if rep[1] != 0x00 {
		t.Fatalf("SOCKS5 CONNECT failed, rep=%d", rep[1])
	}
	return c
}

func TestSOCKS5ThroughDirectUpstream(t *testing.T) {
	echo, closeEcho := echoServer(t)
	defer closeEcho()
	daemon := startDaemon(t, "per_session", []string{"direct://"})

	c := socks5DialThrough(t, daemon, echo, "tab-1")
	defer c.Close()
	c.Write([]byte("ping-through-daemon\n"))
	buf := make([]byte, 64)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "ping-through-daemon\n" {
		t.Fatalf("echo through daemon = %q, err %v", string(buf[:n]), err)
	}
}

func TestSOCKS5ThroughStubUpstream(t *testing.T) {
	stub, closeStub := stubSOCKS5(t)
	defer closeStub()
	echo, closeEcho := echoServer(t)
	defer closeEcho()
	daemon := startDaemon(t, "per_session", []string{"socks5://" + stub})

	c := socks5DialThrough(t, daemon, echo, "")
	defer c.Close()
	c.Write([]byte("ping-via-stub\n"))
	buf := make([]byte, 64)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "ping-via-stub\n" {
		t.Fatalf("echo via stub upstream = %q, err %v", string(buf[:n]), err)
	}
}

func TestHTTPConnectThroughDaemon(t *testing.T) {
	// NOTE: a GET to the public internet cannot work here — this VM's
	// sandbox only lets traffic out through its own HTTP proxy, and the
	// daemon's direct:// upstream dials straight out (a bare TLS
	// ClientHello gets a non-TLS answer from the sandbox's interception).
	// So the test runs a local self-signed TLS server and fetches it
	// THROUGH the daemon: this proves the full SOCKS5 CONNECT -> upstream
	// dial -> TLS-in-tunnel -> HTTP 200 path end to end.
	tlsSrv := startLocalTLS(t)
	defer tlsSrv.Close()

	daemon := startDaemon(t, "per_session", []string{"direct://"})
	dialer, err := xproxy.SOCKS5("tcp", daemon, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		Dial:            dialer.Dial,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only local server
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Get(tlsSrv.URL)
	if err != nil {
		t.Fatalf("GET via daemon: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "tls-ok" {
		t.Fatalf("GET via daemon: status %d body %q", resp.StatusCode, body)
	}
}

// startLocalTLS runs a self-signed HTTPS server on 127.0.0.1 answering "tls-ok".
func startLocalTLS(t *testing.T) *httptest.Server {
	t.Helper()
	certPEM, keyPEM := selfSignedCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tls-ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	return srv
}

// selfSignedCert returns PEM cert/key for 127.0.0.1, generated in-test.
func selfSignedCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

func TestNoHealthyUpstreamRefused(t *testing.T) {
	// Pool with a single node, forced unhealthy: the daemon must fail
	// closed (SOCKS5 error reply) instead of dialing direct.
	pool := proxy.NewProxyPool([]string{"socks5://127.0.0.1:19999"})
	for _, n := range pool.GetAll() {
		n.IsHealthy.Store(false)
	}
	mgr := proxy.NewManager(pool, "per_session", 30*time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	srv := New(addr, mgr)
	go srv.ListenAndServe()
	time.Sleep(100 * time.Millisecond)

	echo, closeEcho := echoServer(t)
	defer closeEcho()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	io.ReadFull(c, resp)
	host, port, _ := net.SplitHostPort(echo)
	p := 0
	fmt.Sscanf(port, "%d", &p)
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(p>>8), byte(p))
	c.Write(req)
	rep := make([]byte, 10)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("SOCKS5 reply read: %v", err)
	}
	if rep[1] == 0x00 {
		t.Fatal("CONNECT succeeded with no healthy upstream, want refusal")
	}
}

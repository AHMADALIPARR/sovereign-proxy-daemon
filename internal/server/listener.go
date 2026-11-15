// listener.go — the local SOCKS5 / HTTP proxy listener. NOT in the supplied
// paste; the pasted structure lists internal/server/listener.go but provides
// no code for it, and the pasted main.go starts no listener at all. Without
// this file the daemon listens on nothing and routes no traffic.
//
// Protocol dispatch: first byte 0x05 -> SOCKS5 (RFC 1928). Anything else is
// treated as an HTTP proxy request (CONNECT, or absolute-form GET/POST).
// SOCKS5 username/password auth carries the session id (see manager.go).
package server

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	"sovereign-proxy-daemon/internal/proxy"

	xproxy "golang.org/x/net/proxy"
)

type Server struct {
	addr string
	mgr  *proxy.Manager
}

func New(addr string, mgr *proxy.Manager) *Server {
	return &Server{addr: addr, mgr: mgr}
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	log.Printf("[Server] listening on %s (mode=%s)", s.addr, s.mgr.Mode())
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("[Server] accept: %v", err)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	b, err := br.Peek(1)
	if err != nil {
		return
	}
	if b[0] == 0x05 {
		s.handleSOCKS5(c, br)
		return
	}
	s.handleHTTP(c, br)
}

// ---------------------------------------------------------------------------
// Upstream dialing: direct://, socks5://, http(s):// (via CONNECT).
// ---------------------------------------------------------------------------

// DialUpstream opens a connection to target through node. A nil node means
// no healthy upstream is available.
func DialUpstream(node *proxy.ProxyNode, target string) (net.Conn, error) {
	if node == nil {
		return nil, fmt.Errorf("no healthy upstream")
	}
	u, err := url.Parse(node.URL)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(u.Scheme) {
	case "direct":
		d := net.Dialer{Timeout: 10 * time.Second}
		return d.Dial("tcp", target)
	case "socks5", "socks4":
		var auth *xproxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
		if err != nil {
			return nil, err
		}
		return d.Dial("tcp", target)
	case "http", "https":
		d := net.Dialer{Timeout: 10 * time.Second}
		up, err := d.Dial("tcp", u.Host)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
		if u.User != nil {
			pw, _ := u.User.Password()
			cred := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pw))
			fmt.Fprintf(&sb, "Proxy-Authorization: Basic %s\r\n", cred)
		}
		sb.WriteString("\r\n")
		if _, err := up.Write([]byte(sb.String())); err != nil {
			up.Close()
			return nil, err
		}
		br := bufio.NewReader(up)
		line, err := br.ReadString('\n')
		if err != nil || !strings.Contains(line, " 200") {
			up.Close()
			return nil, fmt.Errorf("upstream CONNECT failed: %s", strings.TrimSpace(line))
		}
		// drain remaining headers
		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}
		// NOTE: br may have buffered bytes beyond the headers. For CONNECT
		// the body is empty, so the buffer holds nothing. Safe to use up.
		return up, nil
	default:
		return nil, fmt.Errorf("unsupported upstream scheme %q", u.Scheme)
	}
}

// relay copies bytes in both directions until one side closes.
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// ---------------------------------------------------------------------------
// SOCKS5 (RFC 1928): no-auth and username/password (session id in username).
// ---------------------------------------------------------------------------

func (s *Server) handleSOCKS5(c net.Conn, br *bufio.Reader) {
	// Greeting: VER(0x05) NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 0x05 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	wantUserPass := false
	hasNoAuth := false
	for _, m := range methods {
		switch m {
		case 0x00:
			hasNoAuth = true
		case 0x02:
			wantUserPass = true
		}
	}
	sessionID := ""
	if wantUserPass {
		c.Write([]byte{0x05, 0x02})
		auth, err := readSOCKS5Auth(br)
		if err != nil {
			return
		}
		sessionID = auth
		c.Write([]byte{0x01, 0x00}) // auth success
	} else if hasNoAuth {
		c.Write([]byte{0x05, 0x00})
	} else {
		c.Write([]byte{0x05, 0xff})
		return
	}

	// Request: VER CMD RSV ATYP DST.ADDR DST.PORT — note ATYP is req[3],
	// already read; readSOCKS5Addr takes it as a parameter.
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 0x05 {
		return
	}
	if req[1] != 0x01 { // CONNECT only
		c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	host, err := readSOCKS5Addr(br, req[3])
	if err != nil {
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(br, port); err != nil {
		return
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", int(port[0])<<8|int(port[1])))

	node := s.mgr.Pick(sessionID)
	up, err := DialUpstream(node, target)
	if err != nil {
		log.Printf("[Server] upstream dial %s: %v", target, err)
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	// success reply: VER REP RSV ATYP(IPv4) BND.ADDR BND.PORT
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	relay(c, up)
}

func readSOCKS5Auth(br *bufio.Reader) (string, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 0x01 {
		return "", fmt.Errorf("bad auth header")
	}
	ulen := int(hdr[1])
	uname := make([]byte, ulen)
	if _, err := io.ReadFull(br, uname); err != nil {
		return "", err
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(br, plen); err != nil {
		return "", err
	}
	passwd := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(br, passwd); err != nil {
		return "", err
	}
	return string(uname), nil
}

func readSOCKS5Addr(br *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	default:
		return "", fmt.Errorf("unsupported ATYP %d", atyp)
	}
}

// ---------------------------------------------------------------------------
// HTTP proxy: CONNECT host:port, and absolute-form requests.
// ---------------------------------------------------------------------------

func (s *Server) handleHTTP(c net.Conn, br *bufio.Reader) {
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 2 {
		return
	}
	method, target := parts[0], parts[1]

	// drain headers (we only need Host for absolute-form)
	host := ""
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(l), "host:") {
			host = strings.TrimSpace(l[5:])
		}
	}

	if strings.ToUpper(method) == "CONNECT" {
		node := s.mgr.Pick("")
		up, err := DialUpstream(node, target)
		if err != nil {
			fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		defer up.Close()
		fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		relay(c, up)
		return
	}

	// absolute-form: http://host:port/path
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err != nil {
			return
		}
		dst := u.Host
		if !strings.Contains(dst, ":") {
			dst += ":80"
		}
		node := s.mgr.Pick("")
		up, err := DialUpstream(node, dst)
		if err != nil {
			fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		defer up.Close()
		// forward the request in origin-form
		fmt.Fprintf(up, "%s %s HTTP/1.1\r\nHost: %s\r\n\r\n",
			method, u.RequestURI(), host)
		relay(c, up)
		return
	}
	fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
}

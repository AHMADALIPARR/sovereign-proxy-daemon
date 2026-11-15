package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

func StartHealthChecker(ctx context.Context, pool *ProxyPool, targetURL string, timeout time.Duration, interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				runHealthCheckBatch(pool, targetURL, timeout)
			}
		}
	}()
}

func runHealthCheckBatch(pool *ProxyPool, targetURL string, timeout time.Duration) {
	var wg sync.WaitGroup
	nodes := pool.GetAll()

	for _, node := range nodes {
		wg.Add(1)
		go func(n *ProxyNode) {
			defer wg.Done()
			checkNode(n, targetURL, timeout)
		}(node)
	}
	wg.Wait()
}

func checkNode(node *ProxyNode, targetURL string, timeout time.Duration) {
	start := time.Now()
	parsedURL, err := url.Parse(node.URL)
	if err != nil {
		node.IsHealthy.Store(false)
		return
	}

	var dialer proxy.Dialer = proxy.Direct
	switch parsedURL.Scheme {
	case "socks5", "socks4":
		var auth *proxy.Auth
		if parsedURL.User != nil {
			password, _ := parsedURL.User.Password()
			auth = &proxy.Auth{
				User:     parsedURL.User.Username(),
				Password: password,
			}
		}
		d, err := proxy.SOCKS5("tcp", parsedURL.Host, auth, proxy.Direct)
		if err != nil {
			node.IsHealthy.Store(false)
			return
		}
		dialer = d
	case "http", "https":
		// OWNER-APPROVED FIX (2026-11-14): the supplied code fell through
		// to proxy.Direct for http(s) upstreams, so it health-checked the
		// target WITHOUT the proxy and marked working HTTP upstreams DOWN.
		// An HTTP upstream must be checked through a CONNECT tunnel.
		dialer = &httpConnectDialer{upstream: parsedURL}
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
		Timeout: timeout,
	}

	resp, err := client.Get(targetURL)
	if err != nil || resp.StatusCode != 200 {
		node.IsHealthy.Store(false)
		log.Printf("[Proxy-Checker] DOWN: %s (Err: %v)", node.URL, err)
		return
	}
	defer resp.Body.Close()

	latency := time.Since(start).Milliseconds()
	node.LatencyMs.Store(latency)
	node.IsHealthy.Store(true)
}

// httpConnectDialer is a proxy.Dialer that reaches addr through an HTTP(S)
// proxy by issuing CONNECT and returning the tunnelled connection.
type httpConnectDialer struct {
	upstream *url.URL
}

func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	up, err := dialer.Dial("tcp", d.upstream.Host)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if d.upstream.User != nil {
		pw, _ := d.upstream.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(d.upstream.User.Username() + ":" + pw))
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
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	return up, nil
}

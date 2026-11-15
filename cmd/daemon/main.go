package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sovereign-proxy-daemon/internal/config"
	"sovereign-proxy-daemon/internal/proxy"
	"sovereign-proxy-daemon/internal/server"
)

func main() {
	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	var upstreamURLs []string
	for _, up := range cfg.UpstreamProxies {
		upstreamURLs = append(upstreamURLs, up.URL)
	}

	pool := proxy.NewProxyPool(upstreamURLs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Launch background concurrent health checking
	proxy.StartHealthChecker(
		ctx,
		pool,
		cfg.HealthCheck.TargetURL,
		time.Duration(cfg.HealthCheck.TimeoutSeconds)*time.Second,
		time.Duration(cfg.HealthCheck.IntervalSeconds)*time.Second,
	)

	log.Printf("[Daemon] Sovereign Proxy Multiplexer running on %s", cfg.Server.ListenAddr)
	log.Printf("[Daemon] Managing %d upstream proxy nodes...", len(upstreamURLs))

	// NOTE (assistant wiring — not in the supplied paste): the pasted main.go
	// starts only the health checker and never listens on anything. Starting
	// the local SOCKS5/HTTP listener here; without it the daemon routes no
	// traffic. Rotation modes live in proxy.Manager (manager.go).
	mgr := proxy.NewManager(pool, cfg.Server.RotationMode,
		time.Duration(cfg.Server.RotationIntervalSeconds)*time.Second)
	srv := server.New(cfg.Server.ListenAddr, mgr)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("[Server] %v", err)
		}
	}()

	// Graceful shutdown handler
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("[Daemon] Shutting down proxy daemon...")
	cancel()
}

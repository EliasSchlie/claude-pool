package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
	"github.com/EliasSchlie/claude-pool/internal/pool"
)

func main() {
	poolDir := flag.String("pool-dir", "", "Pool directory (required)")
	flag.Parse()

	if *poolDir == "" {
		fmt.Fprintln(os.Stderr, "error: --pool-dir is required")
		os.Exit(1)
	}

	p := paths.New(*poolDir)
	if err := p.EnsureDirs(); err != nil {
		log.Fatalf("failed to create pool directories: %v", err)
	}

	cfgMgr := pool.NewConfigManager(p.ConfigJSON())
	mgr := pool.NewManager(p, cfgMgr)

	srv := api.NewServer(p.Socket(), mgr.Handle)
	if err := srv.Start(); err != nil {
		log.Fatalf("failed to start API server: %v", err)
	}

	log.Printf("claude-pool daemon started (pool-dir=%s)", *poolDir)

	// Wait for shutdown signal or destroy
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("received %v, shutting down", sig)
	case <-mgr.Done():
		log.Printf("pool destroyed, shutting down")
	}

	srv.Stop()
	mgr.Shutdown()
}

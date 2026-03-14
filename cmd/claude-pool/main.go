package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
	"github.com/EliasSchlie/claude-pool/internal/pool"
)

func main() {
	// Handle install/uninstall subcommands before flag parsing —
	// they don't need --pool-dir and operate on ~/.claude/ instead.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			if err := cmdInstall(); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			return
		case "uninstall":
			if err := cmdUninstall(); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

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

	// Set up file logging per design principle #3: each pool has its own logs.
	// Writes to both stderr (for attached terminals) and daemon.log (for debugging).
	logFile, err := os.OpenFile(p.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("failed to open log file %s: %v", p.DaemonLog(), err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))

	cfgMgr := pool.NewConfigManager(p.ConfigJSON())
	mgr := pool.NewManager(p, cfgMgr)

	srv := api.NewServer(p.Socket(), mgr.Handle)
	srv.OnDisconnect(mgr.HandleDisconnect)
	mgr.SetConnAcceptedAt(srv.ConnAcceptedAt)
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
	log.Printf("daemon stopped")
}

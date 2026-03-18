package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EliasSchlie/claude-pool/internal/api"
	"github.com/EliasSchlie/claude-pool/internal/paths"
	"github.com/EliasSchlie/claude-pool/internal/pool"
)

// trimLogFile removes log entries older than maxAge from the log file.
// Go's log package prefixes each line with "2006/01/02 15:04:05", which
// we parse to determine age. Lines that fail to parse are kept.
func trimLogFile(path string, maxAge time.Duration) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // no log file yet
	}

	cutoff := time.Now().Add(-maxAge)
	lines := strings.Split(string(data), "\n")
	start := 0
	for i, line := range lines {
		if len(line) < 19 {
			continue
		}
		// Parse Go log timestamp prefix: "2006/01/02 15:04:05"
		t, err := time.Parse("2006/01/02 15:04:05", line[:19])
		if err != nil {
			continue
		}
		if t.After(cutoff) {
			start = i
			break
		}
	}
	if start > 0 {
		trimmed := strings.Join(lines[start:], "\n")
		os.WriteFile(path, []byte(trimmed), 0644)
	}
}

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

	defaultPoolDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		defaultPoolDir = filepath.Join(home, ".claude-pool", "default")
	}
	poolDir := flag.String("pool-dir", defaultPoolDir, "Pool directory (default: ~/.claude-pool/default/)")
	flag.Parse()

	if *poolDir == "" {
		fmt.Fprintln(os.Stderr, "error: --pool-dir is required (could not determine home directory)")
		os.Exit(1)
	}

	p := paths.New(*poolDir)
	if err := p.EnsureDirs(); err != nil {
		log.Fatalf("failed to create pool directories: %v", err)
	}

	// SPEC: "Entries older than 30 days are discarded."
	trimLogFile(p.DaemonLog(), 30*24*time.Hour)

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

	// Test watchdog: if started by a test process, self-destruct when it dies.
	// Prevents orphaned daemons when `go test` is killed or times out.
	ownerDied := make(chan struct{})
	if pidStr := os.Getenv("CLAUDE_POOL_TEST_OWNER_PID"); pidStr != "" {
		ownerPID, err := strconv.Atoi(pidStr)
		if err == nil && ownerPID > 0 {
			go func() {
				for {
					if err := syscall.Kill(ownerPID, 0); err != nil {
						log.Printf("test owner (pid=%d) died, self-destructing", ownerPID)
						close(ownerDied)
						return
					}
					time.Sleep(2 * time.Second)
				}
			}()
		}
	}

	// Wait for shutdown signal or destroy
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("received %v, shutting down", sig)
	case <-mgr.Done():
		log.Printf("pool destroyed, shutting down")
	case <-ownerDied:
		log.Printf("shutting down (test owner gone)")
	}

	srv.Stop()
	mgr.Shutdown()
	log.Printf("daemon stopped")
}

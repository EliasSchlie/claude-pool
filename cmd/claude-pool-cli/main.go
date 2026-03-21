// claude-pool-cli — Thin CLI router for claude-pool daemons.
//
// Pool state lives under CLAUDE_POOL_HOME (default: ~/.claude-pool/).
// Each pool gets its own directory: CLAUDE_POOL_HOME/<pool-name>/.
// The `init` command starts the daemon; all other commands talk to it via socket.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// conn is a persistent socket connection used for the entire CLI invocation.
// Opened once in main(), reused by all commands (including --block which
// sends start then wait on the same connection).
type conn struct {
	c       net.Conn
	scanner *bufio.Scanner
}

func dial(socketPath string) (*conn, error) {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to pool: %w", err)
	}
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4MB max — debug-capture can be large
	return &conn{c: c, scanner: s}, nil
}

func (c *conn) send(msg map[string]any) (map[string]any, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := c.c.Write(data); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}

	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("read failed: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}

	var resp map[string]any
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}

	return resp, nil
}

func (c *conn) close() { c.c.Close() }

// --- Path helpers ---

// poolHome returns the base directory for all pool state.
// Respects CLAUDE_POOL_HOME env var, defaults to ~/.claude-pool/.
func poolHome() (string, error) {
	if v := os.Getenv("CLAUDE_POOL_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home: %w", err)
	}
	return filepath.Join(home, ".claude-pool"), nil
}

// poolDir returns the directory for a named pool.
func poolDir(name string) (string, error) {
	home, err := poolHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, name), nil
}

// socketPath returns the socket path for a named pool.
func socketPath(name string) (string, error) {
	dir, err := poolDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "api.sock"), nil
}

// registryPath returns the path to pools.json.
func registryPath() (string, error) {
	home, err := poolHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "pools.json"), nil
}

// daemonBin returns the daemon binary path.
// Resolution order:
//  1. CLAUDE_POOL_DAEMON env var (explicit override)
//  2. "claude-pool" in PATH
//  3. Sibling of the CLI binary (same directory as claude-pool-cli)
func daemonBin() (string, error) {
	if v := os.Getenv("CLAUDE_POOL_DAEMON"); v != "" {
		return v, nil
	}
	if path, err := exec.LookPath("claude-pool"); err == nil {
		return path, nil
	}
	// Sibling discovery: daemon binary next to this CLI binary.
	// Handles symlinks (make install) and direct bin/ usage.
	if self, err := os.Executable(); err == nil {
		resolved, err := filepath.EvalSymlinks(self)
		if err == nil {
			sibling := filepath.Join(filepath.Dir(resolved), "claude-pool")
			if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
				return sibling, nil
			}
		}
	}
	return "", fmt.Errorf("daemon binary not found (set CLAUDE_POOL_DAEMON or add claude-pool to PATH)")
}

func main() {
	args := os.Args[1:]

	pool := "default"
	jsonMode := false
	var remaining []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pool":
			i++
			if i < len(args) {
				pool = args[i]
			}
		case "--json":
			jsonMode = true
		default:
			remaining = append(remaining, args[i])
		}
	}

	if len(remaining) == 0 {
		fmt.Fprintln(os.Stderr, "error: no command specified")
		os.Exit(1)
	}

	cmd := remaining[0]
	cmdArgs := remaining[1:]

	// pools is filesystem-only — no socket needed
	if cmd == "pools" {
		if err := doPools(jsonMode); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// init is special — starts the daemon, then sends init command
	if cmd == "init" {
		if err := doInit(pool, cmdArgs, jsonMode); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// attach takes over the terminal — handle before normal dispatch
	if cmd == "attach" {
		sock, err := socketPath(pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if err := doAttach(sock, cmdArgs); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	sock, err := socketPath(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	c, err := dial(sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer c.close()

	if err := dispatch(c, cmd, cmdArgs, jsonMode); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(c *conn, cmd string, args []string, jsonMode bool) error {
	switch cmd {
	case "ping":
		return doSimple(c, map[string]any{"type": "ping"}, jsonMode)
	case "health":
		return doSimple(c, map[string]any{"type": "health"}, jsonMode)
	case "start":
		return doStart(c, args, jsonMode)
	case "followup":
		return doFollowup(c, args, jsonMode)
	case "wait":
		return doWait(c, args, jsonMode)
	case "capture":
		return doCapture(c, args, jsonMode)
	case "stop":
		return doSessionCmd(c, "stop", args, jsonMode)
	case "info":
		return doInfo(c, args, jsonMode)
	case "ls":
		return doLs(c, args, jsonMode)
	case "set":
		return doSet(c, args, jsonMode)
	case "archive":
		return doArchive(c, args, jsonMode)
	case "unarchive":
		return doSessionCmd(c, "unarchive", args, jsonMode)
	case "resize":
		return doResize(c, args, jsonMode)
	case "config":
		return doConfig(c, args, jsonMode)
	case "destroy":
		return doDestroy(c, args, jsonMode)
	case "debug":
		return doDebug(c, args, jsonMode)
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

// --- Command implementations ---

// doInit starts the daemon (if needed), writes config, sends init, and registers the pool.
func doInit(poolName string, args []string, jsonMode bool) error {
	dir, err := poolDir(poolName)
	if err != nil {
		return err
	}

	sock := filepath.Join(dir, "api.sock")

	// Try connecting to an existing daemon. If it's already running,
	// skip spawn and just send init (handles manual start or survived restart).
	initSuccess := false
	existingConn, _ := dial(sock)

	// Parse args for config updates and init message
	var size int
	var flags, workDir string
	keepFresh := -1 // -1 = not set
	noRestore := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--size":
			i++
			if i < len(args) {
				size, _ = strconv.Atoi(args[i])
			}
		case "--flags":
			i++
			if i < len(args) {
				flags = args[i]
			}
		case "--dir":
			i++
			if i < len(args) {
				workDir = args[i]
			}
		case "--keep-fresh":
			i++
			if i < len(args) {
				keepFresh, _ = strconv.Atoi(args[i])
			}
		case "--no-restore":
			noRestore = true
		}
	}

	// Create pool directory
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create pool directory: %w", err)
	}

	// Ensure hooks are installed (idempotent — re-registers if missing)
	bin, err := daemonBin()
	if err != nil {
		return err
	}
	installCmd := exec.Command(bin, "install")
	installCmd.Stdout = os.Stderr
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("hook installation failed: %w", err)
	}

	// Load existing config, apply overrides, save
	configPath := filepath.Join(dir, "config.json")
	var cfg map[string]any
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: corrupt config.json, starting fresh: %v\n", err)
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	if size > 0 {
		cfg["size"] = size
	}
	if flags != "" {
		cfg["flags"] = flags
	}
	if workDir != "" {
		cfg["dir"] = workDir
	}
	if keepFresh >= 0 {
		cfg["keepFresh"] = keepFresh
	}
	cfgData, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath, append(cfgData, '\n'), 0644); err != nil {
		return fmt.Errorf("cannot write config: %w", err)
	}

	if existingConn == nil {
		// Start daemon (detached — must not inherit CLI's stdio pipes).
		// Daemon handles its own logging to daemon.log. Don't pipe its
		// stdout/stderr anywhere — the CLI must not hold open file descriptors
		// to the daemon (otherwise cmd.Run() in tests blocks until daemon exits).
		daemon := exec.Command(bin, "--pool-dir", dir)
		daemon.Stdout = nil
		daemon.Stderr = nil
		daemon.Stdin = nil
		if err := daemon.Start(); err != nil {
			return fmt.Errorf("cannot start daemon: %w", err)
		}

		// Kill daemon on any subsequent failure — cleared on success.
		defer func() {
			if !initSuccess {
				daemon.Process.Kill()
			}
		}()

		// Wait for socket to appear
		deadline := time.Now().Add(10 * time.Second)
		var statErr error
		for time.Now().Before(deadline) {
			if _, statErr = os.Stat(sock); statErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if statErr != nil {
			return fmt.Errorf("daemon socket never appeared at %s", sock)
		}

		existingConn, err = dial(sock)
		if err != nil {
			return fmt.Errorf("cannot connect to daemon: %w", err)
		}
	}
	defer existingConn.close()

	initMsg := map[string]any{"type": "init"}
	if size > 0 {
		initMsg["size"] = size
	}
	if noRestore {
		initMsg["noRestore"] = true
	}

	resp, err := existingConn.send(initMsg)
	if err != nil {
		return err
	}
	if err := checkError(resp); err != nil {
		return err
	}

	initSuccess = true // success — don't kill daemon on exit

	// Register pool
	if err := registerPool(poolName, sock); err != nil {
		// Non-fatal — pool is running, just not in registry
		fmt.Fprintf(os.Stderr, "warning: failed to register pool: %v\n", err)
	}

	return printResp(resp, jsonMode)
}

// registerPool adds or updates a pool entry in pools.json.
func registerPool(name, sock string) error {
	regPath, err := registryPath()
	if err != nil {
		return err
	}

	var registry map[string]map[string]any
	if data, err := os.ReadFile(regPath); err == nil {
		json.Unmarshal(data, &registry)
	}
	if registry == nil {
		registry = map[string]map[string]any{}
	}

	registry[name] = map[string]any{"socket": sock}
	data, _ := json.MarshalIndent(registry, "", "  ")
	return os.WriteFile(regPath, append(data, '\n'), 0644)
}

func doStart(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "start"}
	var block bool
	var outputFlags map[string]any

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--prompt":
			i++
			if i < len(args) {
				msg["prompt"] = args[i]
			}
		case "--parent":
			i++
			if i < len(args) {
				if args[i] == "none" {
					msg["parent"] = nil
				} else {
					msg["parent"] = args[i]
				}
			}
		case "--block":
			block = true
		case "--source":
			i++
			if i < len(args) {
				if outputFlags == nil {
					outputFlags = map[string]any{}
				}
				outputFlags["source"] = args[i]
			}
		case "--turns":
			i++
			if i < len(args) {
				if outputFlags == nil {
					outputFlags = map[string]any{}
				}
				if v, err := strconv.Atoi(args[i]); err == nil {
					outputFlags["turns"] = v
				}
			}
		case "--detail":
			i++
			if i < len(args) {
				if outputFlags == nil {
					outputFlags = map[string]any{}
				}
				outputFlags["detail"] = args[i]
			}
		}
	}

	if _, hasParent := msg["parent"]; !hasParent {
		if id := callerSessionID(); id != "" {
			msg["parent"] = id
		}
	}

	// SPEC: --block requires --prompt (nothing to wait for without one)
	if block {
		if _, hasPrompt := msg["prompt"]; !hasPrompt {
			return fmt.Errorf("--block requires --prompt")
		}
	}

	resp, err := c.send(msg)
	if err != nil {
		return err
	}
	if err := checkError(resp); err != nil {
		return err
	}

	if block {
		var err2 error
		resp, err2 = blockWait(c, resp, outputFlags)
		if err2 != nil {
			return err2
		}
	}

	return printResp(resp, jsonMode)
}

func doFollowup(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "followup"}
	var block bool
	var outputFlags map[string]any

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--prompt":
			i++
			if i < len(args) {
				msg["prompt"] = args[i]
			}
		case "--block":
			block = true
		case "--source":
			i++
			if i < len(args) {
				if outputFlags == nil {
					outputFlags = map[string]any{}
				}
				outputFlags["source"] = args[i]
			}
		case "--turns":
			i++
			if i < len(args) {
				if outputFlags == nil {
					outputFlags = map[string]any{}
				}
				if v, err := strconv.Atoi(args[i]); err == nil {
					outputFlags["turns"] = v
				}
			}
		case "--detail":
			i++
			if i < len(args) {
				if outputFlags == nil {
					outputFlags = map[string]any{}
				}
				outputFlags["detail"] = args[i]
			}
		}
	}

	resp, err := c.send(msg)
	if err != nil {
		return err
	}
	if err := checkError(resp); err != nil {
		return err
	}

	if block {
		var err2 error
		resp, err2 = blockWait(c, resp, outputFlags)
		if err2 != nil {
			return err2
		}
	}

	return printResp(resp, jsonMode)
}

// blockWait prints the session ID to stderr immediately, then sends a wait
// command and blocks until the session completes. Used by --block in both
// doStart and doFollowup.
func blockWait(c *conn, startResp map[string]any, outputFlags map[string]any) (map[string]any, error) {
	sessionID, _ := startResp["sessionId"].(string)
	fmt.Fprintf(os.Stderr, "Session %s (waiting for completion...)\n", sessionID)
	waitMsg := map[string]any{
		"type":      "wait",
		"sessionId": sessionID,
		"timeout":   300000,
	}
	for k, v := range outputFlags {
		waitMsg[k] = v
	}
	resp, err := c.send(waitMsg)
	if err != nil {
		return nil, err
	}
	if err := checkError(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func doWait(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "wait"}

	hasSession := false
	hasParent := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
				hasSession = true
			}
		case "--parent":
			i++
			if i < len(args) {
				hasParent = true
				if args[i] == "none" {
					msg["parent"] = nil
				} else {
					msg["parent"] = args[i]
				}
			}
		case "--timeout":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["timeout"] = v
				}
			}
		case "--source":
			i++
			if i < len(args) {
				msg["source"] = args[i]
			}
		case "--turns":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["turns"] = v
				}
			}
		case "--detail":
			i++
			if i < len(args) {
				msg["detail"] = args[i]
			}
		}
	}

	if !hasSession && !hasParent {
		if id := callerSessionID(); id != "" {
			msg["parent"] = id
		}
	}

	return doSimple(c, msg, jsonMode)
}

func doCapture(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "capture"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--source":
			i++
			if i < len(args) {
				msg["source"] = args[i]
			}
		case "--turns":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["turns"] = v
				}
			}
		case "--detail":
			i++
			if i < len(args) {
				msg["detail"] = args[i]
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doInfo(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "info"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--verbosity":
			i++
			if i < len(args) {
				msg["verbosity"] = args[i]
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doLs(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "ls"}

	if id := callerSessionID(); id != "" {
		msg["callerId"] = id
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--parent":
			i++
			if i < len(args) {
				if args[i] == "none" {
					delete(msg, "callerId")
					msg["all"] = true
				} else {
					msg["callerId"] = args[i]
				}
			}
		case "--status":
			i++
			if i < len(args) {
				msg["statuses"] = strings.Split(args[i], ",")
			}
		case "--archived":
			msg["archived"] = true
		case "--verbosity":
			i++
			if i < len(args) {
				v := args[i]
				msg["verbosity"] = v
				if v == "nested" || v == "full" {
					msg["tree"] = true
				}
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doSet(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "set"}
	var meta []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--priority":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["priority"] = v
				}
			}
		case "--pinned":
			i++
			if i < len(args) {
				if args[i] == "false" {
					msg["pinned"] = false
				} else if v, err := strconv.Atoi(args[i]); err == nil {
					msg["pinned"] = v
				}
			}
		case "--meta":
			i++
			if i < len(args) {
				meta = append(meta, args[i])
			}
		}
	}

	if len(meta) > 0 {
		metadata := map[string]any{}
		for _, m := range meta {
			parts := strings.SplitN(m, "=", 2)
			if len(parts) == 2 {
				metadata[parts[0]] = parts[1]
			}
		}
		msg["metadata"] = metadata
	}

	return doSimple(c, msg, jsonMode)
}

func doArchive(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "archive"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--recursive":
			msg["recursive"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doResize(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "resize"}
	for i := 0; i < len(args); i++ {
		if args[i] == "--size" {
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["size"] = v
				}
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doConfig(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "config"}
	for i := 0; i < len(args); i++ {
		if args[i] == "--set" {
			i++
			if i < len(args) {
				parts := strings.SplitN(args[i], "=", 2)
				if len(parts) == 2 {
					if msg["set"] == nil {
						msg["set"] = map[string]any{}
					}
					set := msg["set"].(map[string]any)
					if v, err := strconv.Atoi(parts[1]); err == nil {
						set[parts[0]] = v
					} else {
						set[parts[0]] = parts[1]
					}
				}
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doDestroy(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "destroy"}
	for _, a := range args {
		if a == "--confirm" {
			msg["confirm"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doDebug(c *conn, args []string, jsonMode bool) error {
	if len(args) == 0 {
		return fmt.Errorf("debug requires a subcommand: input, capture, slots, logs")
	}
	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "input":
		return doDebugInput(c, subArgs, jsonMode)
	case "capture":
		return doDebugCapture(c, subArgs, jsonMode)
	case "slots":
		return doSimple(c, map[string]any{"type": "debug-slots"}, jsonMode)
	case "logs":
		return doDebugLogs(c, subArgs, jsonMode)
	default:
		return fmt.Errorf("unknown debug command: %s", sub)
	}
}

func doDebugInput(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "input"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--data":
			i++
			if i < len(args) {
				msg["data"] = args[i]
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doDebugCapture(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "debug-capture"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--slot":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["slot"] = v
				}
			}
		case "--raw":
			msg["raw"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doDebugLogs(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "debug-logs"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lines":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["lines"] = v
				}
			}
		case "--follow":
			msg["follow"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
}

// doAttach connects to a session's PTY and pipes stdin/stdout bidirectionally.
// Sets terminal to raw mode, handles SIGWINCH for resize, and restores on exit.
// Disconnect with ~. (tilde-dot on a fresh line, like SSH). ~~ sends a literal ~.
func doAttach(apiSock string, args []string) error {
	var sessionID string
	for i := 0; i < len(args); i++ {
		if args[i] == "--session" {
			i++
			if i < len(args) {
				sessionID = args[i]
			}
		}
	}
	if sessionID == "" {
		return fmt.Errorf("--session is required")
	}

	// Send attach request via API
	c, err := dial(apiSock)
	if err != nil {
		return err
	}
	resp, err := c.send(map[string]any{"type": "attach", "sessionId": sessionID})
	c.close()
	if err != nil {
		return err
	}
	if err := checkError(resp); err != nil {
		return err
	}
	attachSock, _ := resp["socketPath"].(string)
	if attachSock == "" {
		return fmt.Errorf("no socketPath in attach response")
	}

	// Connect to the attach pipe
	pipeConn, err := net.Dial("unix", attachSock)
	if err != nil {
		return fmt.Errorf("connect to attach pipe: %w", err)
	}
	defer pipeConn.Close()

	// Set terminal to raw mode
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("make raw: %w", err)
	}
	restore := func() { term.Restore(fd, oldState) }
	defer restore()

	// Restore terminal on SIGINT/SIGTERM (otherwise raw mode persists)
	termSigCh := make(chan os.Signal, 1)
	signal.Notify(termSigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(termSigCh)
	go func() {
		if sig, ok := <-termSigCh; ok {
			restore()
			// Re-raise to get the default behavior (exit with signal status)
			signal.Reset(sig)
			syscall.Kill(syscall.Getpid(), sig.(syscall.Signal))
		}
	}()

	// Send resize to match local terminal size.
	// Tracks last sent size to prevent resize loops in nested PTY environments.
	var lastW, lastH int
	sendResize := func() {
		w, h, err := term.GetSize(fd)
		if err != nil {
			return
		}
		if w == lastW && h == lastH {
			return
		}
		lastW, lastH = w, h
		rc, err := dial(apiSock)
		if err != nil {
			return
		}
		rc.send(map[string]any{
			"type":      "pty-resize",
			"sessionId": sessionID,
			"cols":      w,
			"rows":      h,
		})
		rc.close()
	}
	sendResize()

	// Handle SIGWINCH → pty-resize
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			sendResize()
		}
	}()
	defer func() {
		signal.Stop(winchCh)
		close(winchCh)
	}()

	// Bidirectional pipe: stdin → socket, socket → stdout
	done := make(chan struct{}, 2)

	// Socket → stdout
	go func() {
		io.Copy(os.Stdout, pipeConn)
		done <- struct{}{}
	}()

	// Stdin → socket with ~. escape sequence detection (like SSH).
	// ~. on a fresh line (after Enter/start) disconnects cleanly.
	go func() {
		const (
			stateNormal   = 0
			stateAfterNL  = 1 // just saw \r or \n, or at start
			stateAfterEsc = 2 // saw ~ after newline
		)
		state := stateAfterNL // start as if after newline
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			writeStart := 0 // batch contiguous bytes for a single Write

			for i := 0; i < n; i++ {
				b := buf[i]
				switch state {
				case stateNormal:
					if b == '\r' || b == '\n' {
						state = stateAfterNL
					}
				case stateAfterNL:
					if b == '~' {
						// Flush bytes before the held ~
						if i > writeStart {
							if _, err := pipeConn.Write(buf[writeStart:i]); err != nil {
								done <- struct{}{}
								return
							}
						}
						state = stateAfterEsc
						writeStart = i + 1 // skip the ~ for now
						continue
					}
					if b == '\r' || b == '\n' {
						state = stateAfterNL
					} else {
						state = stateNormal
					}
				case stateAfterEsc:
					if b == '.' {
						// ~. escape — disconnect
						done <- struct{}{}
						return
					}
					// Not an escape — flush the held ~
					if _, err := pipeConn.Write([]byte{'~'}); err != nil {
						done <- struct{}{}
						return
					}
					writeStart = i // include this byte in next batch
					if b == '\r' || b == '\n' {
						state = stateAfterNL
					} else if b == '~' {
						// ~~ → sent one ~, hold this new one
						writeStart = i + 1
						continue
					} else {
						state = stateNormal
					}
				}
			}

			// Flush remaining batch
			if writeStart < n && state != stateAfterEsc {
				if _, err := pipeConn.Write(buf[writeStart:n]); err != nil {
					done <- struct{}{}
					return
				}
			}

			if readErr != nil {
				// Flush held ~ on EOF
				if state == stateAfterEsc {
					pipeConn.Write([]byte{'~'})
				}
				done <- struct{}{}
				return
			}
		}
	}()

	// Wait for either direction to finish
	<-done
	fmt.Fprintf(os.Stderr, "\r\nConnection closed.\r\n")
	return nil
}

func doSessionCmd(c *conn, cmdType string, args []string, jsonMode bool) error {
	msg := map[string]any{"type": cmdType}
	for i := 0; i < len(args); i++ {
		if args[i] == "--session" {
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doPools(jsonMode bool) error {
	regPath, err := registryPath()
	if err != nil {
		return err
	}

	var registry map[string]map[string]any
	if data, err := os.ReadFile(regPath); err == nil {
		json.Unmarshal(data, &registry)
	}
	// Missing file → empty registry (not an error)
	if registry == nil {
		registry = map[string]map[string]any{}
	}

	type poolInfo struct {
		Name   string `json:"name"`
		Socket string `json:"socket"`
		Status string `json:"status"`
	}

	var pools []poolInfo
	for name, entry := range registry {
		socket, _ := entry["socket"].(string)
		status := "stopped"
		if socket != "" {
			if conn, err := net.Dial("unix", socket); err == nil {
				conn.Close()
				status = "running"
			}
		}
		pools = append(pools, poolInfo{Name: name, Socket: socket, Status: status})
	}

	if jsonMode {
		resp := map[string]any{"type": "pools", "pools": pools}
		out, _ := json.Marshal(resp)
		fmt.Println(string(out))
	} else {
		if len(pools) == 0 {
			fmt.Println("No pools registered.")
		} else {
			for _, p := range pools {
				fmt.Printf("%s (%s)\n", p.Name, p.Status)
			}
		}
	}
	return nil
}

// --- Helpers ---

// callerSessionID returns the caller's Claude Code UUID for parent auto-detection.
// SPEC: "defaults to that session's Claude Code UUID."
//
// Uses the PID registry: a PreToolUse Bash hook writes each Claude session's
// PID → UUID to ~/.claude-pool/pid-registry/. Walks up the process tree
// looking for any ancestor in the registry, since the depth between
// claude-pool-cli and the Claude process varies (subshells, Open Cockpit,
// wrapper scripts, etc.).
func callerSessionID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	regDir := filepath.Join(home, ".claude-pool", "pid-registry")

	pid := os.Getppid()
	for i := 0; i < 8 && pid > 1; i++ {
		data, err := os.ReadFile(filepath.Join(regDir, strconv.Itoa(pid)))
		if err == nil {
			return strings.TrimSpace(string(data))
		}
		pid, err = parentPID(pid)
		if err != nil {
			return ""
		}
	}
	return ""
}

// parentPID returns the parent PID of the given process.
func parentPID(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// --- Output ---

func doSimple(c *conn, msg map[string]any, jsonMode bool) error {
	resp, err := c.send(msg)
	if err != nil {
		return err
	}
	if err := checkError(resp); err != nil {
		return err
	}
	return printResp(resp, jsonMode)
}

func checkError(resp map[string]any) error {
	if resp["type"] == "error" {
		errMsg, _ := resp["error"].(string)
		return fmt.Errorf("%s", errMsg)
	}
	return nil
}

func printResp(resp map[string]any, jsonMode bool) error {
	if jsonMode {
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	switch resp["type"] {
	case "pong":
		fmt.Println("pong")
	case "health":
		health, _ := resp["health"].(map[string]any)
		data, _ := json.MarshalIndent(health, "", "  ")
		fmt.Println(string(data))
	case "pool":
		state, _ := resp["pool"].(map[string]any)
		data, _ := json.MarshalIndent(state, "", "  ")
		fmt.Println(string(data))
	case "started":
		sessionID, _ := resp["sessionId"].(string)
		status, _ := resp["status"].(string)
		fmt.Printf("Session %s (%s)\n", sessionID, status)
	case "result":
		content, _ := resp["content"].(string)
		fmt.Print(content)
		if content != "" && !strings.HasSuffix(content, "\n") {
			fmt.Println()
		}
	case "session":
		session, _ := resp["session"].(map[string]any)
		data, _ := json.MarshalIndent(session, "", "  ")
		fmt.Println(string(data))
	case "sessions":
		sessions, _ := resp["sessions"].([]any)
		data, _ := json.MarshalIndent(sessions, "", "  ")
		fmt.Println(string(data))
	case "ok":
		fmt.Println("ok")
	case "config":
		config, _ := resp["config"].(map[string]any)
		data, _ := json.MarshalIndent(config, "", "  ")
		fmt.Println(string(data))
	default:
		data, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(data))
	}
	return nil
}

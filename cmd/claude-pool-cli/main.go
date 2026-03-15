// claude-pool-cli — Thin CLI router for claude-pool daemons.
//
// Resolves pool names from the registry (pools.json) to socket connections,
// translates CLI commands to socket API calls, and formats output.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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
	return &conn{c: c, scanner: bufio.NewScanner(c)}, nil
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

	// pools is registry-only — no socket needed
	if cmd == "pools" {
		if err := doPools(pool, jsonMode); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	socketPath, err := resolvePool(pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	c, err := dial(socketPath)
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

func resolvePool(name string) (string, error) {
	registryPath := os.Getenv("CLAUDE_POOL_REGISTRY")
	if registryPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home: %w", err)
		}
		registryPath = home + "/.claude-pool/pools.json"
	}

	data, err := os.ReadFile(registryPath)
	if err != nil {
		return "", fmt.Errorf("cannot read registry %s: %w", registryPath, err)
	}

	var registry map[string]map[string]any
	if err := json.Unmarshal(data, &registry); err != nil {
		return "", fmt.Errorf("cannot parse registry: %w", err)
	}

	entry, ok := registry[name]
	if !ok {
		return "", fmt.Errorf("pool %q not found in registry", name)
	}

	socket, ok := entry["socket"].(string)
	if !ok {
		return "", fmt.Errorf("pool %q has no socket path", name)
	}

	return socket, nil
}

func dispatch(c *conn, cmd string, args []string, jsonMode bool) error {
	switch cmd {
	case "ping":
		return doSimple(c, map[string]any{"type": "ping"}, jsonMode)
	case "health":
		return doSimple(c, map[string]any{"type": "health"}, jsonMode)
	case "init":
		return doInit(c, args, jsonMode)
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

func doInit(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "init"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--size":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["size"] = v
				}
			}
		case "--flags":
			i++
			if i < len(args) {
				msg["flags"] = args[i]
			}
		case "--dir":
			i++
			if i < len(args) {
				msg["dir"] = args[i]
			}
		case "--no-restore":
			msg["noRestore"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
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

	// Auto-detect parent from CLAUDE_POOL_SESSION_ID if not explicitly set
	if _, hasParent := msg["parent"]; !hasParent {
		if envParent := os.Getenv("CLAUDE_POOL_SESSION_ID"); envParent != "" {
			msg["parent"] = envParent
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
		sessionID, _ := resp["sessionId"].(string)
		waitMsg := map[string]any{
			"type":      "wait",
			"sessionId": sessionID,
			"timeout":   300000,
		}
		// Merge output flags into wait message
		for k, v := range outputFlags {
			waitMsg[k] = v
		}
		resp, err = c.send(waitMsg)
		if err != nil {
			return err
		}
		if err := checkError(resp); err != nil {
			return err
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
		sessionID, _ := resp["sessionId"].(string)
		waitMsg := map[string]any{
			"type":      "wait",
			"sessionId": sessionID,
			"timeout":   300000,
		}
		for k, v := range outputFlags {
			waitMsg[k] = v
		}
		resp, err = c.send(waitMsg)
		if err != nil {
			return err
		}
		if err := checkError(resp); err != nil {
			return err
		}
	}

	return printResp(resp, jsonMode)
}

func doWait(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "wait"}

	// Auto-detect parent from CLAUDE_POOL_SESSION_ID
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

	// Auto-detect parent if no --session or --parent given
	if !hasSession && !hasParent {
		if envParent := os.Getenv("CLAUDE_POOL_SESSION_ID"); envParent != "" {
			msg["parent"] = envParent
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

	// Auto-detect caller session
	if envSession := os.Getenv("CLAUDE_POOL_SESSION_ID"); envSession != "" {
		msg["callerId"] = envSession
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--parent":
			i++
			if i < len(args) {
				if args[i] == "none" {
					// Override auto-detection — show all sessions
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
					// Try to parse as number
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

func doPools(currentPool string, jsonMode bool) error {
	registryPath := os.Getenv("CLAUDE_POOL_REGISTRY")
	if registryPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home: %w", err)
		}
		registryPath = home + "/.claude-pool/pools.json"
	}

	data, err := os.ReadFile(registryPath)
	if err != nil {
		return fmt.Errorf("cannot read registry: %w", err)
	}

	var registry map[string]map[string]any
	if err := json.Unmarshal(data, &registry); err != nil {
		return fmt.Errorf("cannot parse registry: %w", err)
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
			// Try connecting to check if running
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
		for _, p := range pools {
			fmt.Printf("%s (%s)\n", p.Name, p.Status)
		}
	}
	return nil
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

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

	var registry map[string]map[string]string
	if err := json.Unmarshal(data, &registry); err != nil {
		return "", fmt.Errorf("cannot parse registry: %w", err)
	}

	entry, ok := registry[name]
	if !ok {
		return "", fmt.Errorf("pool %q not found in registry", name)
	}

	socket, ok := entry["socket"]
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
	case "start":
		return doStart(c, args, jsonMode)
	case "wait":
		return doWait(c, args, jsonMode)
	case "info":
		return doInfo(c, args, jsonMode)
	case "ls":
		return doLs(c, args, jsonMode)
	case "capture":
		return doCapture(c, args, jsonMode)
	case "followup":
		return doFollowup(c, args, jsonMode)
	case "set-priority":
		return doSetPriority(c, args, jsonMode)
	case "pin":
		return doPin(c, args, jsonMode)
	case "unpin":
		return doUnpin(c, args, jsonMode)
	case "offload":
		return doSessionCmd(c, "offload", args, jsonMode)
	case "archive":
		return doArchive(c, args, jsonMode)
	case "destroy":
		return doDestroy(c, args, jsonMode)
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

// --- Command implementations ---

func doStart(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "start"}
	var block bool

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
				msg["parentId"] = args[i]
			}
		case "--block":
			block = true
		}
	}

	if _, hasParent := msg["parentId"]; !hasParent {
		if envParent := os.Getenv("CLAUDE_POOL_SESSION_ID"); envParent != "" {
			msg["parentId"] = envParent
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
			"timeout":   120000,
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
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
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
	return doSimple(c, msg, jsonMode)
}

func doInfo(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "info"}
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

func doLs(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "ls"}

	if envSession := os.Getenv("CLAUDE_POOL_SESSION_ID"); envSession != "" {
		msg["callerId"] = envSession
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--all":
			msg["all"] = true
		case "--tree":
			msg["tree"] = true
		case "--archived":
			msg["archived"] = true
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

func doFollowup(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "followup"}
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
		case "--force":
			msg["force"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doSetPriority(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "set-priority"}
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
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doPin(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "pin"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		case "--duration":
			i++
			if i < len(args) {
				if v, err := strconv.Atoi(args[i]); err == nil {
					msg["duration"] = v
				}
			}
		}
	}
	return doSimple(c, msg, jsonMode)
}

func doUnpin(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "unpin"}
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

func doDestroy(c *conn, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "destroy"}
	for _, a := range args {
		if a == "--confirm" {
			msg["confirm"] = true
		}
	}
	return doSimple(c, msg, jsonMode)
}

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

// --- Output ---

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

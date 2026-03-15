// claude-pool-cli — Thin CLI router for claude-pool daemons.
//
// Resolves pool names from the registry (pools.json) to socket connections,
// translates CLI commands to socket API calls, and formats output.
//
// Stub: commands are not yet implemented. This binary exists so that CLI tests
// can compile and fail meaningfully.
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

func main() {
	args := os.Args[1:]

	// Parse global flags
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

	if err := dispatch(socketPath, cmd, cmdArgs, jsonMode); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// resolvePool reads the registry to find a pool's socket path.
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

// dispatch routes CLI commands to socket API calls.
func dispatch(socketPath, cmd string, args []string, jsonMode bool) error {
	switch cmd {
	case "ping":
		return doSimple(socketPath, map[string]any{"type": "ping"}, jsonMode)

	case "health":
		return doSimple(socketPath, map[string]any{"type": "health"}, jsonMode)

	case "start":
		return doStart(socketPath, args, jsonMode)

	case "wait":
		return doWait(socketPath, args, jsonMode)

	case "info":
		return doInfo(socketPath, args, jsonMode)

	case "ls":
		return doLs(socketPath, args, jsonMode)

	case "capture":
		return doCapture(socketPath, args, jsonMode)

	case "followup":
		return doFollowup(socketPath, args, jsonMode)

	case "set-priority":
		return doSetPriority(socketPath, args, jsonMode)

	case "pin":
		return doPin(socketPath, args, jsonMode)

	case "unpin":
		return doUnpin(socketPath, args, jsonMode)

	case "offload":
		return doSessionCmd(socketPath, "offload", args, jsonMode)

	case "archive":
		return doArchive(socketPath, args, jsonMode)

	case "destroy":
		return doDestroy(socketPath, args, jsonMode)

	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

// --- Command implementations ---

func doStart(socketPath string, args []string, jsonMode bool) error {
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

	// Auto-detect parent from env
	if _, hasParent := msg["parentId"]; !hasParent {
		if envParent := os.Getenv("CLAUDE_POOL_SESSION_ID"); envParent != "" {
			msg["parentId"] = envParent
		}
	}

	resp, err := sendMsg(socketPath, msg)
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
		resp, err = sendMsg(socketPath, waitMsg)
		if err != nil {
			return err
		}
		if err := checkError(resp); err != nil {
			return err
		}
	}

	return printResp(resp, jsonMode)
}

func doWait(socketPath string, args []string, jsonMode bool) error {
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doInfo(socketPath string, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "info"}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		}
	}

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doLs(socketPath string, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "ls"}

	// Auto-detect caller from env for ownership filtering
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doCapture(socketPath string, args []string, jsonMode bool) error {
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doFollowup(socketPath string, args []string, jsonMode bool) error {
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doSetPriority(socketPath string, args []string, jsonMode bool) error {
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doPin(socketPath string, args []string, jsonMode bool) error {
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doUnpin(socketPath string, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "unpin"}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		}
	}

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doSessionCmd(socketPath, cmdType string, args []string, jsonMode bool) error {
	msg := map[string]any{"type": cmdType}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
			if i < len(args) {
				msg["sessionId"] = args[i]
			}
		}
	}

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doArchive(socketPath string, args []string, jsonMode bool) error {
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

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doDestroy(socketPath string, args []string, jsonMode bool) error {
	msg := map[string]any{"type": "destroy"}

	for _, a := range args {
		if a == "--confirm" {
			msg["confirm"] = true
		}
	}

	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

func doSimple(socketPath string, msg map[string]any, jsonMode bool) error {
	resp, err := sendMsg(socketPath, msg)
	if err != nil {
		return err
	}

	if err := checkError(resp); err != nil {
		return err
	}

	return printResp(resp, jsonMode)
}

// --- Socket communication ---

func sendMsg(socketPath string, msg map[string]any) (map[string]any, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to pool: %w", err)
	}
	defer conn.Close()

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read failed: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}

	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}

	return resp, nil
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

	// Human-readable output
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

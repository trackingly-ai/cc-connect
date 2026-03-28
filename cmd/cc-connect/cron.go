package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func runCron(args []string) {
	if len(args) == 0 {
		printCronUsage()
		return
	}

	switch args[0] {
	case "add":
		runCronAdd(args[1:])
	case "list":
		runCronList(args[1:])
	case "info":
		runCronInfo(args[1:])
	case "edit":
		runCronEdit(args[1:])
	case "del", "delete", "rm", "remove":
		runCronDel(args[1:])
	case "--help", "-h", "help":
		printCronUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown cron subcommand: %s\n", args[0])
		printCronUsage()
		os.Exit(1)
	}
}

func runCronAdd(args []string) {
	var project, sessionKey, cronExpr, prompt, execCmd, desc, dataDir, sessionMode string
	var timeoutMins *int

	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 < len(args) {
				i++
				project = args[i]
			}
		case "--session-key", "--session", "-s":
			if i+1 < len(args) {
				i++
				sessionKey = args[i]
			}
		case "--cron", "-c":
			if i+1 < len(args) {
				i++
				cronExpr = args[i]
			}
		case "--prompt":
			if i+1 < len(args) {
				i++
				prompt = args[i]
			}
		case "--exec":
			if i+1 < len(args) {
				i++
				execCmd = args[i]
			}
		case "--desc", "--description":
			if i+1 < len(args) {
				i++
				desc = args[i]
			}
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		case "--session-mode":
			if i+1 < len(args) {
				i++
				sessionMode = args[i]
			}
		case "--timeout-mins":
			if i+1 < len(args) {
				i++
				n, err := strconv.Atoi(args[i])
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: invalid --timeout-mins: %v\n", err)
					os.Exit(1)
				}
				timeoutMins = &n
			}
		case "--help", "-h":
			printCronAddUsage()
			return
		default:
			positional = append(positional, args[i])
		}
	}

	// Fallback to env vars (set by cc-connect when spawning agent)
	if project == "" {
		project = os.Getenv("CC_PROJECT")
	}
	if sessionKey == "" {
		sessionKey = os.Getenv("CC_SESSION_KEY")
	}

	// If cron expr not provided via --cron, try positional: first 5 fields are cron, rest is prompt
	if cronExpr == "" && len(positional) >= 6 {
		cronExpr = strings.Join(positional[:5], " ")
		if prompt == "" && execCmd == "" {
			prompt = strings.Join(positional[5:], " ")
		}
	} else if prompt == "" && execCmd == "" && len(positional) > 0 {
		prompt = strings.Join(positional, " ")
	}

	if cronExpr == "" || (prompt == "" && execCmd == "") {
		fmt.Fprintln(os.Stderr, "Error: cron expression and either --prompt or --exec are required")
		printCronAddUsage()
		os.Exit(1)
	}
	if prompt != "" && execCmd != "" {
		fmt.Fprintln(os.Stderr, "Error: --prompt and --exec are mutually exclusive")
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	body := map[string]any{
		"project":     project,
		"session_key": sessionKey,
		"cron_expr":   cronExpr,
		"prompt":      prompt,
		"exec":        execCmd,
		"description": desc,
	}
	if sessionMode != "" {
		body["session_mode"] = sessionMode
	}
	if timeoutMins != nil {
		body["timeout_mins"] = *timeoutMins
	}
	payload, _ := json.Marshal(body)

	resp, err := apiPost(sockPath, "/cron/add", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(respBody)))
		os.Exit(1)
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid cron add response: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Cron job created: %s\n", result["id"])
	fmt.Printf("Schedule: %s\n", result["cron_expr"])
	if execCmd != "" {
		fmt.Printf("Command: %s\n", result["exec"])
	} else {
		fmt.Printf("Prompt: %s\n", result["prompt"])
	}
}

func runCronList(args []string) {
	var project, dataDir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 < len(args) {
				i++
				project = args[i]
			}
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		}
	}

	if project == "" {
		project = os.Getenv("CC_PROJECT")
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	url := "/cron/list"
	if project != "" {
		url += "?project=" + project
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Get("http://unix" + url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	var jobs []map[string]any
	if err := json.Unmarshal(body, &jobs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid cron list response: %v\n", err)
		os.Exit(1)
	}

	if len(jobs) == 0 {
		fmt.Println("No scheduled tasks.")
		return
	}

	fmt.Printf("Scheduled tasks (%d):\n\n", len(jobs))
	for _, j := range jobs {
		enabled := "✅"
		if e, ok := j["enabled"].(bool); ok && !e {
			enabled = "⏸"
		}
		id, _ := j["id"].(string)
		expr, _ := j["cron_expr"].(string)
		prompt, _ := j["prompt"].(string)
		execCmd, _ := j["exec"].(string)
		desc, _ := j["description"].(string)
		display := desc
		if display == "" {
			if execCmd != "" {
				display = "🖥 " + execCmd
			} else {
				display = prompt
			}
			if len(display) > 60 {
				display = display[:60] + "..."
			}
		}
		fmt.Printf("  %s %s  %s  %s\n", enabled, id, expr, display)
	}
}

func runCronDel(args []string) {
	var dataDir string
	var id string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		default:
			id = args[i]
		}
	}

	if id == "" {
		fmt.Fprintln(os.Stderr, "Error: job ID is required")
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	payload, _ := json.Marshal(map[string]string{"id": id})
	resp, err := apiPost(sockPath, "/cron/del", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Printf("Cron job %s deleted.\n", id)
}

func runCronInfo(args []string) {
	var dataDir, id string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		default:
			id = args[i]
		}
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "Error: job ID is required")
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}}
	values := url.Values{"id": {id}}
	resp, err := client.Get("http://unix/cron/info?" + values.Encode())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	fmt.Println(string(body))
}

func runCronEdit(args []string) {
	var dataDir string
	pos := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		case "--help", "-h":
			printCronEditUsage()
			return
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) < 3 {
		fmt.Fprintln(os.Stderr, "Error: usage: cc-connect cron edit <id> <field> <value>")
		printCronEditUsage()
		os.Exit(1)
	}
	id, field, rawValue := pos[0], pos[1], strings.Join(pos[2:], " ")
	var value any = rawValue
	switch field {
	case "enabled", "mute", "silent":
		b, err := strconv.ParseBool(rawValue)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid bool for %s: %v\n", field, err)
			os.Exit(1)
		}
		value = b
	case "timeout_mins":
		n, err := strconv.Atoi(rawValue)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid int for timeout_mins: %v\n", err)
			os.Exit(1)
		}
		value = n
	}

	sockPath := resolveSocketPath(dataDir)
	payload, _ := json.Marshal(map[string]any{
		"id":    id,
		"field": field,
		"value": value,
	})
	resp, err := apiPost(sockPath, "/cron/edit", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	fmt.Println(string(body))
}

func apiPost(sockPath, path string, payload []byte) (*http.Response, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}
	return client.Post("http://unix"+path, "application/json", bytes.NewReader(payload))
}

func printCronUsage() {
	fmt.Println(`Usage: cc-connect cron <command> [options]

Commands:
  add       Create a new scheduled task
  addexec   Create a scheduled shell task
  list      List all scheduled tasks
  info <id> Show full JSON for one task
  edit      Modify one task field
  del <id>  Delete a scheduled task

Run 'cc-connect cron <command> --help' for details.`)
}

func printCronAddUsage() {
	fmt.Println(`Usage: cc-connect cron add [options] [<min> <hour> <day> <month> <weekday> <prompt>]

Create a new scheduled task.

Options:
  -p, --project <name>       Target project (auto-detected from CC_PROJECT env)
  -s, --session-key <key>    Target session (auto-detected from CC_SESSION_KEY env)
  -c, --cron <expr>          Cron expression, e.g. "0 6 * * *"
      --prompt <text>        Task prompt
      --exec <command>       Shell command to execute
      --desc <text>          Short description
      --session-mode <mode>  reuse | new_per_run
      --timeout-mins <mins>  omit for default (30m), 0=unlimited
      --data-dir <path>      Data directory (default: ~/.cc-connect)
  -h, --help                 Show this help

Examples:
  cc-connect cron add --cron "0 6 * * *" --prompt "Collect GitHub trending data" --desc "Daily Trending"
  cc-connect cron add --cron "0 6 * * *" --exec "git status --short" --desc "Daily Git Check"
  cc-connect cron add 0 6 * * * Collect GitHub trending data and send me a summary`)
}

func printCronEditUsage() {
	fmt.Println(`Usage: cc-connect cron edit <id> <field> <value>

Editable fields:
  project session_key cron_expr prompt exec work_dir description
  enabled silent mute session_mode timeout_mins

Examples:
  cc-connect cron edit abc123 description Nightly sync
  cc-connect cron edit abc123 enabled false
  cc-connect cron edit abc123 session_mode new_per_run
  cc-connect cron edit abc123 timeout_mins 10`)
}

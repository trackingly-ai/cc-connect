package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

func runSendFile(args []string) {
	var project, sessionKey, dataDir, path, caption string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "-p":
			if i+1 < len(args) {
				i++
				project = args[i]
			}
		case "--session", "-s":
			if i+1 < len(args) {
				i++
				sessionKey = args[i]
			}
		case "--path":
			if i+1 < len(args) {
				i++
				path = args[i]
			}
		case "--caption":
			if i+1 < len(args) {
				i++
				caption = args[i]
			}
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		case "--help", "-h":
			printSendFileUsage()
			return
		default:
			if path == "" {
				path = args[i]
			} else {
				caption = strings.TrimSpace(strings.Join([]string{caption, args[i]}, " "))
			}
		}
	}

	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, "Error: path is required")
		printSendFileUsage()
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	payload, _ := json.Marshal(map[string]string{
		"project":     project,
		"session_key": sessionKey,
		"path":        path,
		"caption":     caption,
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Post("http://unix/send-file", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Println("File sent successfully.")
}

func printSendFileUsage() {
	fmt.Println(`Usage: cc-connect send-file [options] --path <absolute-path>
       cc-connect send-file [options] <absolute-path>

Send a local file to an active cc-connect session.

Options:
      --path <path>        Absolute local file path to send
      --caption <text>     Optional short caption sent alongside the file
  -p, --project <name>     Target project (optional if only one project)
  -s, --session <key>      Target session key (optional, picks first active)
      --data-dir <path>    Data directory (default: ~/.cc-connect)
  -h, --help               Show this help

Examples:
  cc-connect send-file --path /tmp/report.pdf
  cc-connect send-file --path /tmp/screenshot.png --caption "Latest screenshot"`)
}

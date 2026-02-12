// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/version"
)

// Default socket path (can be overridden via BUREAU_PROXY_SOCKET env var)
const defaultSocketPath = "/run/bureau/proxy.sock"

// ProxyRequest is the JSON request format.
type ProxyRequest struct {
	Service string   `json:"service"`
	Args    []string `json:"args"`
	Input   string   `json:"input,omitempty"`
	Stream  bool     `json:"stream,omitempty"`
}

// ProxyResponse is the JSON response format for non-streaming calls.
type ProxyResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// StreamChunk is a single chunk in a streaming response.
type StreamChunk struct {
	Type string `json:"type"` // "stdout", "stderr", "exit", or "error"
	Data string `json:"data,omitempty"`
	Code int    `json:"code,omitempty"`
}

func main() {
	code := run()
	os.Exit(code)
}

func run() int {
	// Parse arguments - check for --stream and --version flags
	stream := false
	args := os.Args[1:]

	// Look for --version flag first
	for _, arg := range args {
		if arg == "--version" {
			fmt.Printf("bureau-proxy-call %s\n", version.Info())
			return 0
		}
	}

	// Refuse to run outside a sandbox. The default socket path
	// (/run/bureau/proxy.sock) is a bind-mount destination inside a bwrap
	// namespace — outside a sandbox it could point to another instance's
	// socket or not exist at all.
	if os.Getenv("BUREAU_SANDBOX") != "1" {
		fmt.Fprintf(os.Stderr, "error: bureau-proxy-call must run inside a Bureau sandbox (BUREAU_SANDBOX=1 not set)\n")
		return 1
	}

	// Look for --stream flag
	for i, arg := range args {
		if arg == "--stream" {
			stream = true
			args = append(args[:i], args[i+1:]...)
			break
		}
	}

	// Also enable streaming via environment variable
	if os.Getenv("BUREAU_PROXY_STREAM") == "1" {
		stream = true
	}

	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: bureau-proxy-call [--stream] <service> [args...]\n")
		fmt.Fprintf(os.Stderr, "example: bureau-proxy-call github pr list\n")
		fmt.Fprintf(os.Stderr, "         bureau-proxy-call --stream github pr list\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "options:\n")
		fmt.Fprintf(os.Stderr, "  --stream    Stream output as it arrives (for long-running commands)\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "environment:\n")
		fmt.Fprintf(os.Stderr, "  BUREAU_PROXY_SOCKET  Socket path (default: %s)\n", defaultSocketPath)
		fmt.Fprintf(os.Stderr, "  BUREAU_PROXY_STREAM  Set to 1 to enable streaming by default\n")
		return 1
	}

	service := args[0]
	serviceArgs := args[1:]

	socketPath := os.Getenv("BUREAU_PROXY_SOCKET")
	if socketPath == "" {
		socketPath = defaultSocketPath
	}

	// Create HTTP client that uses Unix socket
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(_, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	// Read stdin if available (for commands like git credential helpers).
	// Check if stdin is a pipe/file rather than a terminal.
	var input string
	if stat, _ := os.Stdin.Stat(); (stat.Mode() & os.ModeCharDevice) == 0 {
		// stdin is not a terminal, read it
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to read stdin: %v\n", err)
			return 1
		}
		input = string(data)
	}

	// Build request
	reqBody := ProxyRequest{
		Service: service,
		Args:    serviceArgs,
		Input:   input,
		Stream:  stream,
	}

	reqData, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal request: %v\n", err)
		return 1
	}

	// Send request (URL host is ignored for Unix sockets, but required for valid URL)
	resp, err := client.Post(
		"http://localhost/v1/proxy",
		"application/json",
		bytes.NewReader(reqData),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to connect to proxy: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	// Handle response based on content type
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/x-ndjson") {
		return handleStreamingResponse(resp.Body)
	}
	return handleBufferedResponse(resp.Body)
}

// handleBufferedResponse handles a non-streaming JSON response.
func handleBufferedResponse(body io.Reader) int {
	data, err := netutil.ReadResponse(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to read response: %v\n", err)
		return 1
	}

	var proxyResp ProxyResponse
	if err := json.Unmarshal(data, &proxyResp); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to parse response: %v\n", err)
		return 1
	}

	// Output stdout/stderr
	if proxyResp.Stdout != "" {
		fmt.Print(proxyResp.Stdout)
	}
	if proxyResp.Stderr != "" {
		fmt.Fprint(os.Stderr, proxyResp.Stderr)
	}

	// Handle errors
	if proxyResp.Error != "" {
		fmt.Fprintf(os.Stderr, "error: %s\n", proxyResp.Error)
		if proxyResp.ExitCode == 0 {
			return 1
		}
	}

	return proxyResp.ExitCode
}

// handleStreamingResponse handles a newline-delimited JSON streaming response.
func handleStreamingResponse(body io.Reader) int {
	scanner := bufio.NewScanner(body)
	exitCode := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var chunk StreamChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to parse chunk: %v\n", err)
			continue
		}

		switch chunk.Type {
		case "stdout":
			fmt.Print(chunk.Data)
		case "stderr":
			fmt.Fprint(os.Stderr, chunk.Data)
		case "exit":
			exitCode = chunk.Code
		case "error":
			fmt.Fprintf(os.Stderr, "error: %s\n", chunk.Data)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error: stream read error: %v\n", err)
		return 1
	}

	return exitCode
}

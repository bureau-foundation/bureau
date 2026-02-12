// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/lib/netutil"
	"github.com/bureau-foundation/bureau/lib/version"
)

const defaultSocketPath = "/run/bureau/proxy.sock"

func main() {
	os.Exit(run())
}

func run() int {
	// Handle --version before anything else.
	for _, argument := range os.Args[1:] {
		if argument == "--version" {
			fmt.Printf("bureau-state-check %s\n", version.Info())
			return 0
		}
	}

	// Require sandbox environment.
	if os.Getenv("BUREAU_SANDBOX") != "1" {
		fmt.Fprintf(os.Stderr, "error: bureau-state-check must run inside a Bureau sandbox (BUREAU_SANDBOX=1 not set)\n")
		return 2
	}

	arguments, err := parseArguments(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		printUsage()
		return 2
	}

	socketPath := os.Getenv("BUREAU_PROXY_SOCKET")
	if socketPath == "" {
		socketPath = defaultSocketPath
	}

	actual, err := fetchField(context.Background(), socketPath, arguments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	if checkCondition(actual, arguments) {
		return 0
	}

	fmt.Fprintf(os.Stderr, "condition not met: field %q = %q\n", arguments.field, actual)
	return 1
}

// arguments holds the parsed command-line arguments.
type arguments struct {
	room      string
	eventType string
	stateKey  string
	field     string

	// Exactly one of these conditions is set.
	equals    string
	notEquals string
	in        []string
	notIn     []string

	// Which condition was specified.
	condition string
}

func parseArguments(args []string) (arguments, error) {
	var result arguments

	for i := 0; i < len(args); i++ {
		flag := args[i]
		if i+1 >= len(args) {
			return arguments{}, fmt.Errorf("flag %s requires a value", flag)
		}
		value := args[i+1]
		i++

		switch flag {
		case "--room":
			result.room = value
		case "--type":
			result.eventType = value
		case "--key":
			result.stateKey = value
		case "--field":
			result.field = value
		case "--equals":
			result.equals = value
			result.condition = "equals"
		case "--not-equals":
			result.notEquals = value
			result.condition = "not_equals"
		case "--in":
			result.in = strings.Split(value, ",")
			result.condition = "in"
		case "--not-in":
			result.notIn = strings.Split(value, ",")
			result.condition = "not_in"
		default:
			return arguments{}, fmt.Errorf("unknown flag: %s", flag)
		}
	}

	if result.room == "" {
		return arguments{}, fmt.Errorf("--room is required")
	}
	if result.eventType == "" {
		return arguments{}, fmt.Errorf("--type is required")
	}
	if result.field == "" {
		return arguments{}, fmt.Errorf("--field is required")
	}
	if result.condition == "" {
		return arguments{}, fmt.Errorf("exactly one condition required: --equals, --not-equals, --in, or --not-in")
	}

	return result, nil
}

// fetchField reads a state event from the proxy and extracts the named field.
func fetchField(ctx context.Context, socketPath string, args arguments) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}

	query := url.Values{
		"room": {args.room},
		"type": {args.eventType},
		"key":  {args.stateKey},
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://localhost/v1/matrix/state?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("proxy request failed: %w", err)
	}
	defer response.Body.Close()

	body, err := netutil.ReadResponse(response.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("state event %s/%s in %s: HTTP %d: %s",
			args.eventType, args.stateKey, args.room, response.StatusCode, body)
	}

	var content map[string]any
	if err := json.Unmarshal(body, &content); err != nil {
		return "", fmt.Errorf("parsing state event content: %w", err)
	}

	rawValue, exists := content[args.field]
	if !exists {
		return "", fmt.Errorf("field %q not found in state event content (available: %s)",
			args.field, availableFields(content))
	}

	return fmt.Sprintf("%v", rawValue), nil
}

// availableFields returns a comma-separated list of top-level field names.
func availableFields(content map[string]any) string {
	fields := make([]string, 0, len(content))
	for key := range content {
		fields = append(fields, key)
	}
	return strings.Join(fields, ", ")
}

// checkCondition evaluates the parsed condition against the actual field value.
func checkCondition(actual string, args arguments) bool {
	switch args.condition {
	case "equals":
		return actual == args.equals
	case "not_equals":
		return actual != args.notEquals
	case "in":
		for _, allowed := range args.in {
			if actual == allowed {
				return true
			}
		}
		return false
	case "not_in":
		for _, disallowed := range args.notIn {
			if actual == disallowed {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "\nusage: bureau-state-check --room ROOM --type EVENT_TYPE [--key STATE_KEY] --field FIELD CONDITION\n")
	fmt.Fprintf(os.Stderr, "\nconditions (exactly one required):\n")
	fmt.Fprintf(os.Stderr, "  --equals VALUE       field must equal VALUE\n")
	fmt.Fprintf(os.Stderr, "  --not-equals VALUE   field must not equal VALUE\n")
	fmt.Fprintf(os.Stderr, "  --in V1,V2,...       field must be one of the comma-separated values\n")
	fmt.Fprintf(os.Stderr, "  --not-in V1,V2,...   field must not be any of the comma-separated values\n")
	fmt.Fprintf(os.Stderr, "\nexit codes:\n")
	fmt.Fprintf(os.Stderr, "  0  condition matched\n")
	fmt.Fprintf(os.Stderr, "  1  condition did not match\n")
	fmt.Fprintf(os.Stderr, "  2  error\n")
	fmt.Fprintf(os.Stderr, "\nenvironment:\n")
	fmt.Fprintf(os.Stderr, "  BUREAU_PROXY_SOCKET  proxy socket path (default: %s)\n", defaultSocketPath)
	fmt.Fprintf(os.Stderr, "  BUREAU_SANDBOX       must be set to 1\n")
}

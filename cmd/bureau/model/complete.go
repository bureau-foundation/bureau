// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	modelschema "github.com/bureau-foundation/bureau/lib/schema/model"
	"github.com/bureau-foundation/bureau/lib/service"
)

type completeParams struct {
	ModelConnection
	cli.JSONOutput
	Model  string `json:"model"  flag:"model,m"  desc:"model alias or 'auto'" default:"auto"`
	System string `json:"system" flag:"system,s"  desc:"system prompt"`
	Prompt string `json:"prompt"                   desc:"user message (or read from stdin)" required:"true"`
}

// completeResult is the structured output for --json mode. Contains
// the full response text, model used, token usage, and cost.
type completeResult struct {
	Content          string             `json:"content"           desc:"complete response text"`
	Model            string             `json:"model"             desc:"provider model used"`
	Usage            *modelschema.Usage `json:"usage,omitempty"   desc:"token usage"`
	CostMicrodollars int64              `json:"cost_microdollars" desc:"cost in microdollars"`
}

func completeCommand() *cli.Command {
	var params completeParams

	return &cli.Command{
		Name:    "complete",
		Summary: "Send a completion request to the model service",
		Description: `Send a chat completion request and stream the response to stdout.

The prompt can be provided as a positional argument or piped via stdin.
When stdin is not a terminal, it is read as the prompt (pipe-friendly).

The model service streams response tokens as they arrive. In default
mode, tokens are printed to stdout incrementally. In --json mode, the
full response is collected and emitted as a single JSON object with
the response text, model used, token usage, and cost.`,
		Usage: "bureau model complete [--model ALIAS] [PROMPT]",
		Examples: []cli.Example{
			{
				Description: "Simple completion",
				Command:     "bureau model complete 'What is the capital of France?'",
			},
			{
				Description: "With a specific model and system prompt",
				Command:     "bureau model complete --model reasoning --system 'You are a code reviewer.' 'Review this function'",
			},
			{
				Description: "Pipe input from a file",
				Command:     "cat error.log | bureau model complete --model codex 'Explain this error'",
			},
			{
				Description: "JSON output for scripting",
				Command:     "bureau model complete --model codex --json 'Summarize this'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &completeResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/model/complete"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			// Resolve the prompt from args or stdin.
			prompt, err := resolvePrompt(params.Prompt, args)
			if err != nil {
				return err
			}

			// Build the message list.
			var messages []modelschema.Message
			if params.System != "" {
				messages = append(messages, modelschema.Message{
					Role:    "system",
					Content: params.System,
				})
			}
			messages = append(messages, modelschema.Message{
				Role:    "user",
				Content: prompt,
			})

			client, err := params.connect()
			if err != nil {
				return err
			}

			// Open a streaming completion.
			stream, err := client.OpenStream(ctx, modelschema.ActionComplete, &modelschema.Request{
				Model:    params.Model,
				Messages: messages,
				Stream:   true,
			})
			if err != nil {
				return classifyServiceError(err, "complete")
			}
			defer stream.Close()

			// Read response chunks.
			return readCompletionStream(stream, &params.JSONOutput)
		},
	}
}

// readCompletionStream reads model.Response messages from a stream
// until a done or error message arrives. In text mode, deltas are
// printed to stdout as they arrive. In JSON mode, the full content
// is collected and emitted as a completeResult.
func readCompletionStream(stream *service.ServiceStream, jsonOutput *cli.JSONOutput) error {
	var collected strings.Builder
	var finalModel string
	var finalUsage *modelschema.Usage
	var finalCost int64

	for {
		var response modelschema.Response
		if err := stream.Recv(&response); err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "EOF") {
				// Server closed the stream without a done message.
				// This should not happen in normal operation — treat
				// it as an error.
				return cli.Internal("stream ended without done message")
			}
			return cli.Internal("reading stream: %w", err)
		}

		switch response.Type {
		case modelschema.ResponseDelta:
			collected.WriteString(response.Content)
			if !jsonOutput.OutputJSON {
				fmt.Print(response.Content)
			}

		case modelschema.ResponseDone:
			if response.Content != "" {
				collected.WriteString(response.Content)
				if !jsonOutput.OutputJSON {
					fmt.Print(response.Content)
				}
			}
			finalModel = response.Model
			finalUsage = response.Usage
			finalCost = response.CostMicrodollars

			// Print trailing newline for terminal readability.
			if !jsonOutput.OutputJSON {
				fmt.Println()
			}

			result := completeResult{
				Content:          collected.String(),
				Model:            finalModel,
				Usage:            finalUsage,
				CostMicrodollars: finalCost,
			}

			if done, err := jsonOutput.EmitJSON(result); done {
				return err
			}

			// Print usage summary to stderr in text mode so it
			// doesn't interfere with pipe-friendly stdout output.
			if finalUsage != nil {
				fmt.Fprintf(os.Stderr, "[model=%s input=%d output=%d cost=$%.4f]\n",
					finalModel,
					finalUsage.InputTokens,
					finalUsage.OutputTokens,
					float64(finalCost)/1_000_000)
			}
			return nil

		case modelschema.ResponseError:
			return cli.Transient("model service error: %s", response.Error)

		default:
			return cli.Internal("unexpected response type %q", response.Type)
		}
	}
}

// resolvePrompt determines the prompt text from command-line arguments
// or stdin. Priority: positional arg > params.Prompt > stdin (if not
// a terminal).
func resolvePrompt(paramPrompt string, args []string) (string, error) {
	// Positional argument takes precedence.
	if len(args) == 1 {
		return args[0], nil
	}
	if len(args) > 1 {
		return "", cli.Validation("expected at most 1 positional argument, got %d", len(args))
	}

	// Explicit --prompt flag (set by MCP or JSON params).
	if paramPrompt != "" {
		return paramPrompt, nil
	}

	// Fall back to stdin if it's not a terminal.
	if !isTerminal(os.Stdin) {
		scanner := bufio.NewScanner(os.Stdin)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			return "", cli.Internal("reading stdin: %w", err)
		}
		text := strings.Join(lines, "\n")
		if strings.TrimSpace(text) == "" {
			return "", cli.Validation("no prompt provided and stdin is empty\n\nUsage: bureau model complete [PROMPT]")
		}
		return text, nil
	}

	return "", cli.Validation("prompt is required\n\nUsage: bureau model complete [PROMPT]")
}

// isTerminal reports whether the file is a terminal (not piped).
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// classifyServiceError wraps service errors with contextual hints.
func classifyServiceError(err error, action string) error {
	if serviceError, ok := err.(*service.ServiceError); ok {
		message := serviceError.Message
		if strings.Contains(message, "unknown model alias") {
			return cli.NotFound("unknown model alias: %s", message).
				WithHint("Use 'bureau model list' to see available model aliases.")
		}
		if strings.Contains(message, "quota exceeded") {
			return cli.Transient("quota exceeded: %s", message).
				WithHint("Use 'bureau model status' to check current quota usage.")
		}
		return cli.Transient("model service rejected %s request: %s", action, message)
	}
	return err
}

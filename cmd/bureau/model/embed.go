// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	modelschema "github.com/bureau-foundation/bureau/lib/schema/model"
)

type embedParams struct {
	ModelConnection
	cli.JSONOutput
	Model string `json:"model" flag:"model,m" desc:"model alias or 'auto'" default:"auto"`
	Input string `json:"input"                 desc:"text to embed (or read from stdin)" required:"true"`
}

// embedResult is the structured output for --json mode.
type embedResult struct {
	Dimensions       int                `json:"dimensions"        desc:"embedding vector dimensions"`
	Model            string             `json:"model"             desc:"provider model used"`
	Usage            *modelschema.Usage `json:"usage,omitempty"   desc:"token usage"`
	CostMicrodollars int64              `json:"cost_microdollars" desc:"cost in microdollars"`
	Vectors          [][]float32        `json:"vectors"           desc:"embedding vectors (float32)"`
}

func embedCommand() *cli.Command {
	var params embedParams

	return &cli.Command{
		Name:    "embed",
		Summary: "Generate embeddings for text input",
		Description: `Send text to the model service for embedding. The input can be
provided as a positional argument or piped via stdin.

The response contains embedding vectors as float32 arrays. In text
mode, a summary (dimensions, model, cost) is printed. In --json mode,
the full vectors are included in the output.`,
		Usage: "bureau model embed [--model ALIAS] [TEXT]",
		Examples: []cli.Example{
			{
				Description: "Embed a text string",
				Command:     "bureau model embed --model local-embed 'Bureau model service architecture'",
			},
			{
				Description: "Embed from stdin",
				Command:     "cat document.txt | bureau model embed --model local-embed",
			},
			{
				Description: "JSON output with full vectors",
				Command:     "bureau model embed --model local-embed --json 'search query'",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &embedResult{} },
		Annotations:    cli.Create(),
		RequiredGrants: []string{"command/model/embed"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			input, err := resolvePrompt(params.Input, args)
			if err != nil {
				return err
			}

			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var response modelschema.EmbedResponse
			err = client.Call(ctx, modelschema.ActionEmbed, &modelschema.Request{
				Model: params.Model,
				Input: [][]byte{[]byte(input)},
			}, &response)
			if err != nil {
				return classifyServiceError(err, "embed")
			}

			if response.Error != "" {
				return cli.Transient("embedding failed: %s", response.Error)
			}

			// Convert raw byte vectors to float32 slices.
			vectors := make([][]float32, len(response.Embeddings))
			dimensions := 0
			for index, raw := range response.Embeddings {
				floats := bytesToFloat32(raw)
				vectors[index] = floats
				if index == 0 {
					dimensions = len(floats)
				}
			}

			result := embedResult{
				Dimensions:       dimensions,
				Model:            response.Model,
				Usage:            response.Usage,
				CostMicrodollars: response.CostMicrodollars,
				Vectors:          vectors,
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			// Human-readable summary.
			fmt.Printf("Model:      %s\n", response.Model)
			fmt.Printf("Dimensions: %d\n", dimensions)
			if response.Usage != nil {
				fmt.Printf("Tokens:     %d\n", response.Usage.InputTokens)
			}
			fmt.Printf("Cost:       $%.6f\n", float64(response.CostMicrodollars)/1_000_000)

			return nil
		},
	}
}

// bytesToFloat32 converts a raw byte slice (little-endian float32s)
// into a []float32. Returns nil if the input length is not a multiple
// of 4.
func bytesToFloat32(raw []byte) []float32 {
	if len(raw)%4 != 0 {
		return nil
	}
	count := len(raw) / 4
	result := make([]float32, count)
	for index := 0; index < count; index++ {
		bits := binary.LittleEndian.Uint32(raw[index*4 : (index+1)*4])
		result[index] = math.Float32frombits(bits)
	}
	return result
}

// readStdin reads all lines from stdin and joins them with newlines.
// Used when the prompt is not provided as a positional argument and
// stdin is not a terminal.
func readStdin() (string, error) {
	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

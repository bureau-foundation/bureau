// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticket

import (
	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ticketui"
	tea "github.com/charmbracelet/bubbletea"
)

// viewerParams holds the parameters for the ticket viewer command.
type viewerParams struct {
	FilePath string `json:"file" flag:"file" desc:"path to beads JSONL file (default: .beads/issues.jsonl)"`
}

// ViewerCommand returns the "viewer" subcommand that launches the
// interactive ticket viewer TUI.
func ViewerCommand() *cli.Command {
	var params viewerParams

	return &cli.Command{
		Name:    "viewer",
		Summary: "Interactive ticket viewer",
		Description: `Launch an interactive terminal UI for browsing tickets.

By default, loads tickets from .beads/issues.jsonl in the current
directory. Use --file to specify an alternate JSONL path.`,
		Usage: "bureau ticket viewer [flags]",
		Examples: []cli.Example{
			{
				Description: "Open the ticket viewer with default beads file",
				Command:     "bureau ticket viewer",
			},
			{
				Description: "Open with a specific file",
				Command:     "bureau ticket viewer --file path/to/issues.jsonl",
			},
		},
		Params: func() any { return &params },
		Run: func(args []string) error {
			if len(args) > 0 {
				return cli.Validation("unexpected argument: %s", args[0])
			}

			filePath := params.FilePath
			if filePath == "" {
				filePath = ".beads/issues.jsonl"
			}

			source, err := ticketui.LoadBeadsFile(filePath)
			if err != nil {
				return cli.Internal("load tickets from %s: %w", filePath, err)
			}

			model := ticketui.NewModel(source)
			program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())
			_, err = program.Run()
			return err
		},
	}
}

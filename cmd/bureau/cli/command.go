// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"
)

// Command represents a CLI command or subcommand.
type Command struct {
	// Name is the command name as typed by the user (e.g., "matrix", "setup").
	Name string

	// Summary is a one-line description shown in the parent's help listing.
	Summary string

	// Description is a detailed multi-line description shown in the command's
	// own help output.
	Description string

	// Usage is the usage string (e.g., "bureau matrix setup [flags]").
	// If empty, it is synthesized from the command path and subcommands.
	Usage string

	// Examples are shown in the help output after the description.
	Examples []Example

	// Flags returns a configured *pflag.FlagSet for this command. Called
	// lazily on first use. If nil, the command accepts no flags.
	Flags func() *pflag.FlagSet

	// Subcommands are nested commands dispatched by the first positional arg.
	Subcommands []*Command

	// Run executes the command with the remaining args (after flag parsing).
	// Exactly one of Run or Subcommands should be set. If both are set,
	// Run is used when no subcommand matches.
	Run func(args []string) error

	// parent is set during dispatch to build the full command path for help.
	parent *Command
}

// Example is a usage example shown in help output.
type Example struct {
	// Description explains what the example does.
	Description string
	// Command is the literal command line.
	Command string
}

// ErrNotImplemented returns a standard error for commands that are defined
// in the CLI tree but not yet implemented. Using a shared function ensures
// consistent wording and makes it easy to grep for unfinished commands.
func ErrNotImplemented(command string) error {
	return fmt.Errorf("%s: not yet implemented", command)
}

// Execute parses args and dispatches to the appropriate subcommand or Run
// function. This is the main entry point for the command tree.
func (c *Command) Execute(args []string) error {
	// Check for help flags before anything else.
	if len(args) > 0 && isHelpFlag(args[0]) {
		c.PrintHelp(os.Stderr)
		return nil
	}

	// If we have subcommands, try to dispatch.
	if len(c.Subcommands) > 0 && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name := args[0]
		for _, sub := range c.Subcommands {
			if sub.Name == name {
				sub.parent = c
				return sub.Execute(args[1:])
			}
		}

		// Unknown subcommand — suggest the closest match.
		suggestion := suggestCommand(name, c.Subcommands)
		if suggestion != "" {
			return fmt.Errorf("unknown command %q (did you mean %q?)\n\nRun '%s --help' for usage.",
				name, suggestion, c.fullName())
		}
		return fmt.Errorf("unknown command %q\n\nRun '%s --help' for usage.",
			name, c.fullName())
	}

	// If we have subcommands but no args (and no Run), show help.
	if len(c.Subcommands) > 0 && c.Run == nil {
		if len(args) == 0 {
			c.PrintHelp(os.Stderr)
			return fmt.Errorf("subcommand required")
		}
		// args[0] starts with "-" but we have no Run function.
		if isHelpFlag(args[0]) {
			c.PrintHelp(os.Stderr)
			return nil
		}
		c.PrintHelp(os.Stderr)
		return fmt.Errorf("subcommand required (got flag %q)", args[0])
	}

	// Parse flags if defined.
	if c.Flags != nil {
		flagSet := c.Flags()

		// Suppress the stdlib flag package's default error output and
		// usage dump. We format our own error messages with suggestions.
		flagSet.SetOutput(io.Discard)

		if err := flagSet.Parse(args); err != nil {
			// Build a helpful error message: error line, suggestion if
			// applicable, then a pointer to --help for full usage.
			errMsg := err.Error()

			if strings.Contains(errMsg, "unknown flag") {
				// Recreate the flagSet to get a clean copy for suggestion
				// lookup (the failed parse may have consumed state).
				suggestion := suggestFlag(args, c.Flags())
				if suggestion != "" {
					return fmt.Errorf("%s (did you mean %s?)\n\nRun '%s --help' for usage.",
						errMsg, suggestion, c.fullName())
				}
			}

			return fmt.Errorf("%s\n\nRun '%s --help' for usage.",
				errMsg, c.fullName())
		}
		args = flagSet.Args()
	}

	if c.Run != nil {
		return c.Run(args)
	}

	// No Run, no subcommands matched — show help.
	c.PrintHelp(os.Stderr)
	return fmt.Errorf("no action defined for %q", c.fullName())
}

// PrintHelp writes structured help output to w.
func (c *Command) PrintHelp(w io.Writer) {
	name := c.fullName()

	// Description or summary.
	if c.Description != "" {
		fmt.Fprintf(w, "%s\n\n", c.Description)
	} else if c.Summary != "" {
		fmt.Fprintf(w, "%s\n\n", c.Summary)
	}

	// Usage line.
	if c.Usage != "" {
		fmt.Fprintf(w, "Usage:\n  %s\n", c.Usage)
	} else if len(c.Subcommands) > 0 {
		fmt.Fprintf(w, "Usage:\n  %s <command> [flags]\n", name)
	} else {
		fmt.Fprintf(w, "Usage:\n  %s [flags]\n", name)
	}

	// Subcommands.
	if len(c.Subcommands) > 0 {
		fmt.Fprintf(w, "\nCommands:\n")
		tw := tabwriter.NewWriter(w, 2, 0, 3, ' ', 0)
		for _, sub := range c.Subcommands {
			fmt.Fprintf(tw, "  %s\t%s\n", sub.Name, sub.Summary)
		}
		tw.Flush()
	}

	// Flags.
	if c.Flags != nil {
		flagSet := c.Flags()
		var flagHelp strings.Builder
		flagSet.SetOutput(&flagHelp)
		flagSet.PrintDefaults()
		if flagHelp.Len() > 0 {
			fmt.Fprintf(w, "\nFlags:\n%s", flagHelp.String())
		}
	}

	// Examples.
	if len(c.Examples) > 0 {
		fmt.Fprintf(w, "\nExamples:\n")
		for _, example := range c.Examples {
			if example.Description != "" {
				fmt.Fprintf(w, "  # %s\n", example.Description)
			}
			fmt.Fprintf(w, "  %s\n", example.Command)
			if example.Description != "" {
				fmt.Fprintln(w)
			}
		}
	}

	// Footer: help hint for subcommands.
	if len(c.Subcommands) > 0 {
		fmt.Fprintf(w, "\nRun '%s <command> --help' for more information on a command.\n", name)
	}
}

// fullName returns the complete command path (e.g., "bureau matrix setup").
func (c *Command) fullName() string {
	if c.parent == nil {
		return c.Name
	}
	return c.parent.fullName() + " " + c.Name
}

// isHelpFlag returns true for common help flag variants.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestCommand_Execute_DispatchesToSubcommand(t *testing.T) {
	var called string

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{
				Name: "version",
				Run: func(args []string) error {
					called = "version"
					return nil
				},
			},
			{
				Name: "matrix",
				Run: func(args []string) error {
					called = "matrix"
					return nil
				},
			},
		},
	}

	if err := root.Execute([]string{"matrix"}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if called != "matrix" {
		t.Errorf("dispatched to %q, want %q", called, "matrix")
	}
}

func TestCommand_Execute_NestedSubcommands(t *testing.T) {
	var called string
	var receivedArgs []string

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{
				Name: "matrix",
				Subcommands: []*Command{
					{
						Name: "setup",
						Run: func(args []string) error {
							called = "matrix setup"
							receivedArgs = args
							return nil
						},
					},
				},
			},
		},
	}

	if err := root.Execute([]string{"matrix", "setup", "extra-arg"}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if called != "matrix setup" {
		t.Errorf("dispatched to %q, want %q", called, "matrix setup")
	}
	if len(receivedArgs) != 1 || receivedArgs[0] != "extra-arg" {
		t.Errorf("args = %v, want [extra-arg]", receivedArgs)
	}
}

func TestCommand_Execute_FlagParsing(t *testing.T) {
	var socketPath string
	var target string

	command := &Command{
		Name: "observe",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("observe", pflag.ContinueOnError)
			flagSet.StringVar(&socketPath, "socket", "/default.sock", "socket path")
			return flagSet
		},
		Run: func(args []string) error {
			if len(args) > 0 {
				target = args[0]
			}
			return nil
		},
	}

	if err := command.Execute([]string{"--socket", "/custom.sock", "iree/amdgpu/pm"}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if socketPath != "/custom.sock" {
		t.Errorf("socketPath = %q, want %q", socketPath, "/custom.sock")
	}
	if target != "iree/amdgpu/pm" {
		t.Errorf("target = %q, want %q", target, "iree/amdgpu/pm")
	}
}

func TestCommand_Execute_UnknownFlagSuggestion(t *testing.T) {
	command := &Command{
		Name: "observe",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("observe", pflag.ContinueOnError)
			flagSet.Bool("readonly", false, "read-only mode")
			flagSet.String("socket", "/default.sock", "socket path")
			return flagSet
		},
		Run: func(args []string) error { return nil },
	}

	err := command.Execute([]string{"--readnoly"})
	if err == nil {
		t.Fatal("Execute() = nil, want error for unknown flag")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "did you mean --readonly") {
		t.Errorf("error = %q, want suggestion for '--readonly'", errStr)
	}
	// Suggestion should be on the same line as the error, not buried.
	if !strings.Contains(errStr, "readnoly") {
		t.Errorf("error = %q, should mention the bad flag", errStr)
	}
	// Should include a pointer to --help.
	if !strings.Contains(errStr, "--help") {
		t.Errorf("error = %q, should point to --help", errStr)
	}
}

func TestCommand_Execute_UnknownFlagNoSuggestion(t *testing.T) {
	command := &Command{
		Name: "observe",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("observe", pflag.ContinueOnError)
			flagSet.Bool("readonly", false, "read-only mode")
			return flagSet
		},
		Run: func(args []string) error { return nil },
	}

	err := command.Execute([]string{"--zzzzzzzzz"})
	if err == nil {
		t.Fatal("Execute() = nil, want error for unknown flag")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("error = %q, should not suggest for distant flag", err.Error())
	}
	if !strings.Contains(err.Error(), "--help") {
		t.Errorf("error = %q, should point to --help", err.Error())
	}
}

func TestCommand_Execute_UnknownSubcommandSuggestion(t *testing.T) {
	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "observe"},
			{Name: "matrix"},
			{Name: "version"},
		},
	}

	err := root.Execute([]string{"matrx"})
	if err == nil {
		t.Fatal("Execute() = nil, want error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "did you mean \"matrix\"") {
		t.Errorf("error = %q, want suggestion for 'matrix'", err.Error())
	}
}

func TestCommand_Execute_UnknownSubcommandNoSuggestion(t *testing.T) {
	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "observe"},
			{Name: "matrix"},
		},
	}

	err := root.Execute([]string{"zzzzzzz"})
	if err == nil {
		t.Fatal("Execute() = nil, want error for unknown subcommand")
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("error = %q, should not contain suggestion for distant input", err.Error())
	}
}

func TestCommand_Execute_HelpFlag(t *testing.T) {
	for _, helpArg := range []string{"-h", "--help", "help"} {
		t.Run(helpArg, func(t *testing.T) {
			root := &Command{
				Name:    "bureau",
				Summary: "AI agent orchestration",
				Subcommands: []*Command{
					{Name: "matrix", Summary: "Matrix operations"},
				},
			}

			err := root.Execute([]string{helpArg})
			if err != nil {
				t.Errorf("Execute(%q) error: %v", helpArg, err)
			}
		})
	}
}

func TestCommand_Execute_NoArgsShowsHelp(t *testing.T) {
	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "matrix", Summary: "Matrix operations"},
		},
	}

	err := root.Execute([]string{})
	if err == nil {
		t.Fatal("Execute() = nil, want error for missing subcommand")
	}
	if !strings.Contains(err.Error(), "subcommand required") {
		t.Errorf("error = %q, want 'subcommand required'", err.Error())
	}
}

func TestCommand_PrintHelp(t *testing.T) {
	command := &Command{
		Name:        "bureau",
		Description: "AI agent orchestration system.",
		Subcommands: []*Command{
			{Name: "observe", Summary: "Attach to a terminal session"},
			{Name: "matrix", Summary: "Matrix homeserver operations"},
			{Name: "version", Summary: "Print version information"},
		},
		Examples: []Example{
			{
				Description: "Observe an agent's terminal",
				Command:     "bureau observe iree/amdgpu/pm",
			},
			{
				Description: "Bootstrap the Matrix homeserver",
				Command:     "bureau matrix setup --registration-token-file /path/to/token",
			},
		},
	}

	var buffer bytes.Buffer
	command.PrintHelp(&buffer)
	output := buffer.String()

	// Verify structural elements are present.
	for _, want := range []string{
		"AI agent orchestration system.",
		"Usage:",
		"bureau <command> [flags]",
		"Commands:",
		"observe",
		"Attach to a terminal session",
		"matrix",
		"Matrix homeserver operations",
		"Examples:",
		"bureau observe iree/amdgpu/pm",
		"bureau matrix setup",
		"Run 'bureau <command> --help'",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q\n\nFull output:\n%s", want, output)
		}
	}
}

func TestCommand_PrintHelp_WithFlags(t *testing.T) {
	command := &Command{
		Name:    "observe",
		Summary: "Attach to a terminal session",
		Usage:   "bureau observe <target> [flags]",
		Flags: func() *pflag.FlagSet {
			flagSet := pflag.NewFlagSet("observe", pflag.ContinueOnError)
			flagSet.String("socket", "/run/bureau/observe.sock", "daemon observation socket")
			flagSet.Bool("readonly", false, "observe without input")
			return flagSet
		},
	}

	var buffer bytes.Buffer
	command.PrintHelp(&buffer)
	output := buffer.String()

	for _, want := range []string{
		"bureau observe <target> [flags]",
		"Flags:",
		"socket",
		"readonly",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q\n\nFull output:\n%s", want, output)
		}
	}
}

func TestCommand_FullName(t *testing.T) {
	root := &Command{Name: "bureau"}
	matrix := &Command{Name: "matrix", parent: root}
	setup := &Command{Name: "setup", parent: matrix}

	if got := root.fullName(); got != "bureau" {
		t.Errorf("root.fullName() = %q, want %q", got, "bureau")
	}
	if got := matrix.fullName(); got != "bureau matrix" {
		t.Errorf("matrix.fullName() = %q, want %q", got, "bureau matrix")
	}
	if got := setup.fullName(); got != "bureau matrix setup" {
		t.Errorf("setup.fullName() = %q, want %q", got, "bureau matrix setup")
	}
}

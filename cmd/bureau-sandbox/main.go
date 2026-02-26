// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/sandbox"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Set up logging.
	logLevel := slog.LevelInfo
	if os.Getenv("BUREAU_DEBUG") != "" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = runCmd(args, logger)
	case "validate":
		err = validateCmd(args, logger)
	case "test":
		err = testCmd(args, logger)
	case "version", "--version", "-v":
		fmt.Printf("bureau-sandbox %s\n", version.Info())
		return
	case "help", "--help", "-h":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		// Check for exit code errors.
		if code, ok := sandbox.IsExitError(err); ok {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`bureau-sandbox - Run commands in isolated bubblewrap sandboxes

USAGE
    bureau-sandbox <command> [flags] [-- <args>...]

COMMANDS
    run           Run a command in the sandbox
    validate      Validate sandbox configuration
    test          Run escape detection tests
    version       Show version

EXAMPLES
    # Run a command with an explicit profile file
    bureau-sandbox run --profile-file=myprofile.yaml --working-directory=/path/to/work -- bash

    # Validate configuration before running
    bureau-sandbox validate --profile-file=myprofile.yaml --working-directory=/path/to/work

    # Run with GPU support
    bureau-sandbox run --profile-file=myprofile.yaml --gpu --working-directory=/path/to/work -- python train.py

    # Dry run to see the bwrap command
    bureau-sandbox run --profile-file=myprofile.yaml --working-directory=/path/to/work --dry-run -- bash

PROFILE FILES
    Profile files are YAML containing a sandbox profile definition:

        description: "My sandbox profile"
        filesystem:
          - source: /usr
            dest: /usr
            mode: ro
        namespaces:
          pid: true
          net: true
        environment:
          PATH: "/usr/bin:/bin"
        security:
          new_session: true
          die_with_parent: true
          no_new_privs: true

    In production, sandbox configuration comes from Matrix templates resolved
    by the daemon. This CLI is for manual testing and development.

ENVIRONMENT
    BUREAU_ROOT          Base directory for Bureau (default: ~/.cache/bureau)
    BUREAU_PROXY_SOCKET  Path to proxy Unix socket (default: /run/bureau/proxy.sock)
    BUREAU_DEBUG         Enable debug logging

For more information, see: https://github.com/bureau-foundation/bureau
`)
}

// loadProfileFile loads a sandbox profile from a YAML file.
// The file should contain a single profile definition (not wrapped in a
// "profiles:" key). Inheritance is not supported in standalone profile files;
// template inheritance is resolved by the daemon via Matrix.
func loadProfileFile(path string) (*sandbox.Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profile file: %w", err)
	}

	var profile sandbox.Profile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("parsing profile file %s: %w", path, err)
	}

	if profile.Inherit != "" {
		return nil, fmt.Errorf("profile file %s uses 'inherit: %s' but inheritance is not "+
			"supported in standalone profile files; use a fully-resolved profile or "+
			"templates via Matrix", path, profile.Inherit)
	}

	return &profile, nil
}

// runCmd implements the "run" command.
func runCmd(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)

	profileFile := fs.String("profile-file", "", "Path to a YAML sandbox profile definition (required)")
	workingDirectory := fs.String("working-directory", "", "Host path mounted at /workspace inside the sandbox (required)")
	proxySocket := fs.String("proxy-socket", "", "Override proxy socket path")
	scopeName := fs.String("name", "", "Systemd scope name for resource tracking")
	gpu := fs.Bool("gpu", false, "Enable GPU passthrough")
	bazelCache := fs.String("bazel-cache", "", "Shared Bazel cache directory")
	dryRun := fs.Bool("dry-run", false, "Print command without executing")
	verbose := fs.Bool("verbose", false, "Show bwrap command being executed")

	// Repeatable flags for extra mounts and env vars.
	var extraBinds stringSlice
	var extraEnvs stringSlice
	fs.Var(&extraBinds, "bind", "Extra bind mount (source:dest[:mode]), repeatable")
	fs.Var(&extraBinds, "ro-bind", "Extra read-only bind (source:dest), repeatable")
	fs.Var(&extraEnvs, "env", "Extra environment variable (KEY=VALUE), repeatable")

	fs.Usage = func() {
		fmt.Print(`bureau-sandbox run - Run a command in the sandbox

USAGE
    bureau-sandbox run [flags] -- <command> [args...]

FLAGS
`)
		fs.PrintDefaults()
		fmt.Print(`
EXAMPLES
    bureau-sandbox run --profile-file=dev.yaml --working-directory=/work -- bash
    bureau-sandbox run --profile-file=dev.yaml --working-directory=/work --gpu -- python train.py
    bureau-sandbox run --profile-file=dev.yaml --working-directory=/work --dry-run -- bash
`)
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Find command separator.
	command := fs.Args()
	if len(command) == 0 {
		return fmt.Errorf("command is required after --")
	}

	if *profileFile == "" {
		return fmt.Errorf("--profile-file is required: specify a YAML file containing the sandbox profile definition")
	}

	if *workingDirectory == "" {
		return fmt.Errorf("--working-directory is required")
	}

	// Load profile from file.
	profile, err := loadProfileFile(*profileFile)
	if err != nil {
		return err
	}

	// Parse extra env vars.
	extraEnvMap := make(map[string]string)
	for _, env := range extraEnvs {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid env format %q: must be KEY=VALUE", env)
		}
		extraEnvMap[parts[0]] = parts[1]
	}

	// Create sandbox.
	sb, err := sandbox.New(sandbox.Config{
		Profile:          profile,
		WorkingDirectory: *workingDirectory,
		ProxySocket:      *proxySocket,
		ScopeName:        *scopeName,
		GPU:              *gpu,
		BazelCache:       *bazelCache,
		ExtraBinds:       extraBinds,
		ExtraEnv:         extraEnvMap,
		Logger:           logger,
	})
	if err != nil {
		return err
	}

	// Dry run.
	if *dryRun || *verbose {
		fullCmd, err := sb.DryRun(command)
		if err != nil {
			return err
		}
		if *dryRun {
			fmt.Println(strings.Join(fullCmd, " \\\n  "))
			return nil
		}
		if *verbose {
			logger.Info("executing sandbox command", "command", strings.Join(fullCmd, " "))
		}
	}

	// Set up signal handling.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalChannel
		cancel()
	}()

	// Run.
	return sb.Run(ctx, command)
}

// validateCmd implements the "validate" command.
func validateCmd(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)

	profileFile := fs.String("profile-file", "", "Path to a YAML sandbox profile definition (required)")
	workingDirectory := fs.String("working-directory", "", "Host path mounted at /workspace inside the sandbox (required)")
	proxySocket := fs.String("proxy-socket", "", "Override proxy socket path")
	gpu := fs.Bool("gpu", false, "Also validate GPU requirements")

	fs.Usage = func() {
		fmt.Print(`bureau-sandbox validate - Validate sandbox configuration

USAGE
    bureau-sandbox validate [flags]

FLAGS
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *profileFile == "" {
		return fmt.Errorf("--profile-file is required: specify a YAML file containing the sandbox profile definition")
	}

	if *workingDirectory == "" {
		return fmt.Errorf("--working-directory is required")
	}

	// Load profile from file.
	profile, err := loadProfileFile(*profileFile)
	if err != nil {
		return err
	}

	// Run validation.
	validator := sandbox.NewValidator()
	validator.ValidateAll(profile, *workingDirectory, *proxySocket)

	if *gpu {
		validator.ValidateGPU()
	}

	validator.PrintResults(os.Stdout)

	if validator.HasErrors() {
		return fmt.Errorf("validation failed")
	}
	return nil
}

// testCmd implements the "test" command.
func testCmd(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)

	category := fs.String("category", "", "Run only tests in this category")

	fs.Usage = func() {
		fmt.Print(`bureau-sandbox test - Run escape detection tests

This command runs inside a sandbox to verify isolation is working.
Tests attempt various escape vectors and report whether they were blocked.

USAGE
    bureau-sandbox test [flags]

FLAGS
`)
		fs.PrintDefaults()
		fmt.Print(`
CATEGORIES
    network     Network isolation tests
    filesystem  Filesystem isolation tests
    process     Process isolation tests
    privilege   Privilege escalation tests
    terminal    Terminal injection tests

NOTE
    This command should be run INSIDE a sandbox. To test your sandbox:

    bureau-sandbox run --profile-file=dev.yaml --working-directory=/tmp/test -- \
        bureau-sandbox test
`)
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Check if we're inside a sandbox.
	if os.Getenv("BUREAU_SANDBOX") != "1" {
		fmt.Println("Warning: Not running inside a sandbox (BUREAU_SANDBOX != 1)")
		fmt.Println("For proper testing, run this command inside a sandbox:")
		fmt.Println()
		fmt.Println("  bureau-sandbox run --profile-file=<profile.yaml> --working-directory=/tmp/test -- \\")
		fmt.Println("      bureau-sandbox test")
		fmt.Println()
		fmt.Println("Running tests anyway (results may not reflect sandbox isolation)...")
		fmt.Println()
	}

	// Run tests.
	runner := sandbox.NewEscapeTestRunner()
	ctx := context.Background()

	if *category != "" {
		runner.RunCategory(ctx, *category)
	} else {
		runner.RunAll(ctx)
	}

	runner.PrintResults(os.Stdout)

	if runner.HasFailures() {
		return fmt.Errorf("escape tests failed")
	}
	return nil
}

// stringSlice implements flag.Value for repeatable string flags.
type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ", ")
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

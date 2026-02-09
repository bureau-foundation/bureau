// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

// bureau-sandbox runs commands in an isolated bubblewrap sandbox.
//
// Usage:
//
//	bureau-sandbox run [flags] -- <command> [args...]
//	bureau-sandbox validate [flags]
//	bureau-sandbox list-profiles
//	bureau-sandbox show-profile <name>
//	bureau-sandbox test [flags]
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

	"github.com/bureau-foundation/bureau/lib/config"
	"github.com/bureau-foundation/bureau/lib/version"
	"github.com/bureau-foundation/bureau/sandbox"
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

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = runCmd(args, logger)
	case "validate":
		err = validateCmd(args, logger)
	case "list-profiles":
		err = listProfilesCmd(args)
	case "show-profile":
		err = showProfileCmd(args)
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
    list-profiles List available profiles
    show-profile  Show profile details
    test          Run escape detection tests
    version       Show version

EXAMPLES
    # Run a command in the developer profile
    bureau-sandbox run --profile=developer --worktree=/path/to/work -- bash

    # Validate configuration before running
    bureau-sandbox validate --profile=developer --worktree=/path/to/work

    # Run with GPU support
    bureau-sandbox run --profile=developer --gpu --worktree=/path/to/work -- python train.py

    # Dry run to see the bwrap command
    bureau-sandbox run --profile=developer --worktree=/path/to/work --dry-run -- bash

ENVIRONMENT
    BUREAU_ROOT          Base directory for Bureau (default: ~/.cache/bureau)
    BUREAU_PROXY_SOCKET  Path to proxy Unix socket (default: /run/bureau/proxy.sock)
    BUREAU_DEBUG         Enable debug logging

For more information, see: https://github.com/bureau-foundation/bureau
`)
}

// loadProfiles loads sandbox profiles from the appropriate source.
// If BUREAU_CONFIG is set and contains sandbox.profiles_file, load from there.
// Otherwise fall back to standard search paths.
func loadProfiles(logger *slog.Logger) (*sandbox.ProfileLoader, error) {
	loader := sandbox.NewProfileLoader()
	loader.SetLogger(logger)

	// Load defaults first.
	if err := loader.LoadDefaults(); err != nil {
		return nil, err
	}

	// Check if BUREAU_CONFIG specifies a profiles file.
	if configPath := os.Getenv("BUREAU_CONFIG"); configPath != "" {
		cfg, err := config.LoadFile(configPath)
		if err != nil {
			logger.Warn("failed to load BUREAU_CONFIG, using search paths", "error", err)
		} else if cfg.Sandbox.ProfilesFile != "" {
			logger.Info("loading profiles from config", "path", cfg.Sandbox.ProfilesFile)
			if err := loader.LoadFile(cfg.Sandbox.ProfilesFile); err != nil {
				return nil, fmt.Errorf("failed to load profiles from %s: %w", cfg.Sandbox.ProfilesFile, err)
			}
			return loader, nil
		}
	}

	// Fall back to search paths.
	for _, path := range sandbox.ConfigSearchPaths() {
		if _, err := os.Stat(path); err == nil {
			if err := loader.LoadFile(path); err != nil {
				return nil, fmt.Errorf("failed to load %s: %w", path, err)
			}
		} else {
			loader.SetLogger(logger) // Re-set to log "not found" messages
		}
	}

	return loader, nil
}

// runCmd implements the "run" command.
func runCmd(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)

	profile := fs.String("profile", "developer", "Profile name")
	worktree := fs.String("worktree", "", "Path to agent worktree (required)")
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
    bureau-sandbox run --profile=developer --worktree=/work -- bash
    bureau-sandbox run --profile=developer --worktree=/work --gpu -- python train.py
    bureau-sandbox run --profile=developer --worktree=/work --dry-run -- bash
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

	if *worktree == "" {
		return fmt.Errorf("--worktree is required")
	}

	// Load profiles with verbose logging.
	loader, err := loadProfiles(logger)
	if err != nil {
		return fmt.Errorf("failed to load profiles: %w", err)
	}

	// Resolve profile.
	prof, err := loader.Resolve(*profile)
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
		Profile:     prof,
		Worktree:    *worktree,
		ProxySocket: *proxySocket,
		ScopeName:   *scopeName,
		GPU:         *gpu,
		BazelCache:  *bazelCache,
		ExtraBinds:  extraBinds,
		ExtraEnv:    extraEnvMap,
		Logger:      logger,
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Run.
	return sb.Run(ctx, command)
}

// validateCmd implements the "validate" command.
func validateCmd(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)

	profile := fs.String("profile", "developer", "Profile name")
	worktree := fs.String("worktree", "", "Path to agent worktree (required)")
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

	if *worktree == "" {
		return fmt.Errorf("--worktree is required")
	}

	// Load profiles with verbose logging.
	loader, err := loadProfiles(logger)
	if err != nil {
		return fmt.Errorf("failed to load profiles: %w", err)
	}

	// Resolve profile.
	prof, err := loader.Resolve(*profile)
	if err != nil {
		return err
	}

	// Run validation.
	validator := sandbox.NewValidator()
	validator.ValidateAll(prof, *worktree, *proxySocket)

	if *gpu {
		validator.ValidateGPU()
	}

	validator.PrintResults(os.Stdout)

	if validator.HasErrors() {
		return fmt.Errorf("validation failed")
	}
	return nil
}

// listProfilesCmd implements the "list-profiles" command.
func listProfilesCmd(args []string) error {
	loader, err := sandbox.LoadFromSearchPaths()
	if err != nil {
		return fmt.Errorf("failed to load profiles: %w", err)
	}

	profiles := loader.List()
	fmt.Println("Available profiles:")
	for _, name := range profiles {
		prof, err := loader.Resolve(name)
		if err != nil {
			fmt.Printf("  %s (error: %v)\n", name, err)
			continue
		}
		fmt.Printf("  %s - %s\n", name, prof.Description)
	}

	return nil
}

// showProfileCmd implements the "show-profile" command.
func showProfileCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("profile name required")
	}
	name := args[0]

	loader, err := sandbox.LoadFromSearchPaths()
	if err != nil {
		return fmt.Errorf("failed to load profiles: %w", err)
	}

	prof, err := loader.Resolve(name)
	if err != nil {
		return err
	}

	fmt.Printf("Profile: %s\n", prof.Name)
	fmt.Printf("Description: %s\n", prof.Description)
	fmt.Println()

	fmt.Println("Namespaces:")
	fmt.Printf("  PID: %v\n", prof.Namespaces.PID)
	fmt.Printf("  Net: %v\n", prof.Namespaces.Net)
	fmt.Printf("  IPC: %v\n", prof.Namespaces.IPC)
	fmt.Printf("  UTS: %v\n", prof.Namespaces.UTS)
	fmt.Printf("  Cgroup: %v\n", prof.Namespaces.Cgroup)
	fmt.Println()

	fmt.Println("Security:")
	fmt.Printf("  New Session: %v\n", prof.Security.NewSession)
	fmt.Printf("  Die With Parent: %v\n", prof.Security.DieWithParent)
	fmt.Printf("  No New Privs: %v\n", prof.Security.NoNewPrivs)
	fmt.Println()

	fmt.Println("Resources:")
	if prof.Resources.TasksMax > 0 {
		fmt.Printf("  Tasks Max: %d\n", prof.Resources.TasksMax)
	} else {
		fmt.Printf("  Tasks Max: unlimited\n")
	}
	if prof.Resources.MemoryMax != "" {
		fmt.Printf("  Memory Max: %s\n", prof.Resources.MemoryMax)
	} else {
		fmt.Printf("  Memory Max: unlimited\n")
	}
	if prof.Resources.CPUQuota != "" {
		fmt.Printf("  CPU Quota: %s\n", prof.Resources.CPUQuota)
	} else {
		fmt.Printf("  CPU Quota: unlimited\n")
	}
	fmt.Println()

	fmt.Println("Filesystem Mounts:")
	for _, m := range prof.Filesystem {
		mode := m.Mode
		if mode == "" {
			mode = "rw"
		}
		optional := ""
		if m.Optional {
			optional = " (optional)"
		}
		if m.Type == "" || m.Type == "bind" || m.Type == "dev-bind" {
			fmt.Printf("  %s -> %s [%s]%s\n", m.Source, m.Dest, mode, optional)
		} else {
			fmt.Printf("  %s at %s%s\n", m.Type, m.Dest, optional)
		}
	}
	fmt.Println()

	fmt.Println("Environment:")
	for k, v := range prof.Environment {
		fmt.Printf("  %s=%s\n", k, v)
	}

	return nil
}

// testCmd implements the "test" command.
func testCmd(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)

	profile := fs.String("profile", "developer", "Profile name")
	worktree := fs.String("worktree", "", "Path to test worktree")
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

    bureau-sandbox run --profile=developer --worktree=/tmp/test -- \
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
		if *worktree != "" {
			fmt.Printf("  bureau-sandbox run --profile=%s --worktree=%s -- \\\n", *profile, *worktree)
		} else {
			fmt.Printf("  bureau-sandbox run --profile=%s --worktree=/tmp/test -- \\\n", *profile)
		}
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

// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubExec replaces pluginExecFunc for the duration of a test. Returns
// a function that retrieves the captured call (binary, argv, env) or
// reports that exec was never called.
func stubExec(t *testing.T) func() (string, []string, []string) {
	t.Helper()

	var (
		called bool
		binary string
		argv   []string
		env    []string
	)

	original := pluginExecFunc
	pluginExecFunc = func(path string, args []string, environment []string) error {
		called = true
		binary = path
		argv = args
		env = environment
		// Return an error so Execute doesn't actually replace the
		// process. The dispatch code treats any return from exec as
		// a failure, which is correct behavior for tests.
		return os.ErrPermission
	}
	t.Cleanup(func() { pluginExecFunc = original })

	return func() (string, []string, []string) {
		t.Helper()
		if !called {
			t.Fatal("pluginExecFunc was not called")
		}
		return binary, argv, env
	}
}

// installFakePlugin creates a fake executable script in a temp directory
// and prepends that directory to PATH. Returns the expected absolute
// path of the installed plugin.
func installFakePlugin(t *testing.T, name string) string {
	t.Helper()

	directory := t.TempDir()
	pluginPath := filepath.Join(directory, name)

	// Create a minimal executable. Content doesn't matter — the test
	// stubs out exec before it runs.
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write fake plugin: %v", err)
	}

	t.Setenv("PATH", directory+":"+os.Getenv("PATH"))
	return pluginPath
}

func TestPluginDispatch_FoundOnPath(t *testing.T) {
	pluginPath := installFakePlugin(t, "bureau-hello")
	getCall := stubExec(t)

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	// Execute returns an error because our stubExec returns
	// os.ErrPermission (simulating exec failure after the binary
	// was found). The important thing is that exec WAS called.
	err := root.Execute([]string{"hello", "world", "--verbose"})
	if err == nil {
		t.Fatal("Execute() = nil, want error from stubbed exec")
	}

	binary, argv, _ := getCall()
	if binary != pluginPath {
		t.Errorf("exec binary = %q, want %q", binary, pluginPath)
	}
	// argv[0] is the binary path, then the remaining args.
	wantArgv := []string{pluginPath, "world", "--verbose"}
	if len(argv) != len(wantArgv) {
		t.Fatalf("argv length = %d, want %d: %v", len(argv), len(wantArgv), argv)
	}
	for index, want := range wantArgv {
		if argv[index] != want {
			t.Errorf("argv[%d] = %q, want %q", index, argv[index], want)
		}
	}
}

func TestPluginDispatch_NotFound_FallsThrough(t *testing.T) {
	// Don't install any plugin — expect the dispatch to return nil
	// and fall through to suggestion logic.
	original := pluginExecFunc
	pluginExecFunc = func(string, []string, []string) error {
		t.Fatal("pluginExecFunc should not be called when no plugin exists")
		return nil
	}
	t.Cleanup(func() { pluginExecFunc = original })

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	err := root.Execute([]string{"nonexistent-plugin-xyzzy"})
	if err == nil {
		t.Fatal("Execute() = nil, want error for unknown command")
	}
	// Should get a suggestion/unknown command error, not a plugin error.
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want 'unknown command' (fallthrough to suggestions)", err.Error())
	}
}

func TestPluginDispatch_RootLevelOnly(t *testing.T) {
	// Install a plugin that would match if dispatch fired at
	// non-root levels.
	installFakePlugin(t, "bureau-setup")

	original := pluginExecFunc
	pluginExecFunc = func(string, []string, []string) error {
		t.Fatal("pluginExecFunc should not be called for non-root dispatch")
		return nil
	}
	t.Cleanup(func() { pluginExecFunc = original })

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{
				Name: "matrix",
				Subcommands: []*Command{
					{Name: "doctor"},
				},
			},
		},
	}

	// "bureau matrix setup" should NOT try bureau-setup as a plugin
	// because "setup" is at the "matrix" level, not the root level.
	err := root.Execute([]string{"matrix", "setup"})
	if err == nil {
		t.Fatal("Execute() = nil, want error for unknown subcommand")
	}
	// Should get a suggestion error, not a plugin dispatch.
	if strings.Contains(err.Error(), "exec plugin") {
		t.Errorf("error = %q, should not attempt plugin dispatch at non-root level", err.Error())
	}
}

func TestPluginDispatch_EnvPluginMarker(t *testing.T) {
	installFakePlugin(t, "bureau-test-env")
	getCall := stubExec(t)

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	_ = root.Execute([]string{"test-env"})
	_, _, env := getCall()

	assertEnvContains(t, env, "BUREAU_PLUGIN", "1")
}

func TestPluginDispatch_EnvFromMachineConf(t *testing.T) {
	confContent := "BUREAU_HOMESERVER_URL=http://matrix.test:6167\n" +
		"BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/worker\n" +
		"BUREAU_SERVER_NAME=test.local\n" +
		"BUREAU_FLEET=bureau/fleet/prod\n"
	confPath := filepath.Join(t.TempDir(), "machine.conf")
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("write machine.conf: %v", err)
	}
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	installFakePlugin(t, "bureau-test-conf")
	getCall := stubExec(t)

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	_ = root.Execute([]string{"test-conf"})
	_, _, env := getCall()

	assertEnvContains(t, env, "BUREAU_SERVER_NAME", "test.local")
	assertEnvContains(t, env, "BUREAU_FLEET", "bureau/fleet/prod")
	assertEnvContains(t, env, "BUREAU_HOMESERVER_URL", "http://matrix.test:6167")
	assertEnvContains(t, env, "BUREAU_MACHINE_NAME", "bureau/fleet/prod/machine/worker")
	assertEnvContains(t, env, "BUREAU_MACHINE_CONF", confPath)
}

func TestPluginDispatch_EnvSessionFilePath(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	t.Setenv("BUREAU_SESSION_FILE", sessionPath)

	installFakePlugin(t, "bureau-test-session")
	getCall := stubExec(t)

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	_ = root.Execute([]string{"test-session"})
	_, _, env := getCall()

	assertEnvContains(t, env, "BUREAU_SESSION_FILE", sessionPath)
}

func TestPluginDispatch_ExistingEnvNotOverwritten(t *testing.T) {
	// Set BUREAU_SERVER_NAME explicitly in the environment.
	t.Setenv("BUREAU_SERVER_NAME", "explicit-override.local")

	// Machine.conf has a different value.
	confContent := "BUREAU_SERVER_NAME=from-conf.local\n"
	confPath := filepath.Join(t.TempDir(), "machine.conf")
	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		t.Fatalf("write machine.conf: %v", err)
	}
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	installFakePlugin(t, "bureau-test-override")
	getCall := stubExec(t)

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	_ = root.Execute([]string{"test-override"})
	_, _, env := getCall()

	// Explicit env var should win over machine.conf.
	assertEnvContains(t, env, "BUREAU_SERVER_NAME", "explicit-override.local")
}

func TestPluginDispatch_NoArgsAfterName(t *testing.T) {
	pluginPath := installFakePlugin(t, "bureau-noargs")
	getCall := stubExec(t)

	root := &Command{
		Name: "bureau",
		Subcommands: []*Command{
			{Name: "version"},
		},
	}

	_ = root.Execute([]string{"noargs"})
	_, argv, _ := getCall()

	// argv should be just the binary path — no extra args.
	if len(argv) != 1 {
		t.Errorf("argv = %v, want just [%s]", argv, pluginPath)
	}
}

func TestBuildPluginEnv_PreservesExistingVars(t *testing.T) {
	// Ensure that arbitrary existing env vars survive buildPluginEnv.
	t.Setenv("MY_CUSTOM_VAR", "preserved")
	// Point machine.conf at nothing to avoid injecting extra vars.
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent"))

	env := buildPluginEnv()

	assertEnvContains(t, env, "MY_CUSTOM_VAR", "preserved")
	assertEnvContains(t, env, "BUREAU_PLUGIN", "1")
}

func TestBuildPluginEnv_ConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	// Point machine.conf at nothing.
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent"))

	// Unset BUREAU_CONFIG_DIR so buildPluginEnv computes it from XDG.
	// t.Setenv("", "") would set it to empty string which envSet
	// treats as present. Must actually remove it from the environment.
	originalConfigDir, hadConfigDir := os.LookupEnv("BUREAU_CONFIG_DIR")
	os.Unsetenv("BUREAU_CONFIG_DIR")
	if hadConfigDir {
		t.Cleanup(func() { os.Setenv("BUREAU_CONFIG_DIR", originalConfigDir) })
	}

	env := buildPluginEnv()

	assertEnvContains(t, env, "BUREAU_CONFIG_DIR", "/custom/config/bureau")
}

func TestFindPlugin_NextToSelf(t *testing.T) {
	// os.Executable() returns the test binary path. Create a fake
	// plugin next to it to verify the "companion binary" search.
	selfPath, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable() failed: %v", err)
	}

	selfDir := filepath.Dir(selfPath)
	pluginName := "bureau-companion-test"
	pluginPath := filepath.Join(selfDir, pluginName)

	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write companion plugin: %v", err)
	}
	t.Cleanup(func() { os.Remove(pluginPath) })

	found := findPlugin(pluginName)
	if found != pluginPath {
		t.Errorf("findPlugin(%q) = %q, want %q", pluginName, found, pluginPath)
	}
}

func TestFindPlugin_PrefersCompanionOverPath(t *testing.T) {
	// If the same plugin name exists both next to self and on PATH,
	// the companion (next to self) should win.
	selfPath, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable() failed: %v", err)
	}

	selfDir := filepath.Dir(selfPath)
	pluginName := "bureau-precedence-test"

	// Create companion version.
	companionPath := filepath.Join(selfDir, pluginName)
	if err := os.WriteFile(companionPath, []byte("#!/bin/sh\n# companion"), 0755); err != nil {
		t.Fatalf("write companion plugin: %v", err)
	}
	t.Cleanup(func() { os.Remove(companionPath) })

	// Create PATH version in a different directory.
	installFakePlugin(t, pluginName)

	found := findPlugin(pluginName)
	if found != companionPath {
		t.Errorf("findPlugin(%q) = %q, want companion at %q", pluginName, found, companionPath)
	}
}

func TestFindPlugin_FallsBackToPath(t *testing.T) {
	pluginName := "bureau-pathonly-test"
	pathPlugin := installFakePlugin(t, pluginName)

	found := findPlugin(pluginName)
	if found != pathPlugin {
		t.Errorf("findPlugin(%q) = %q, want PATH result %q", pluginName, found, pathPlugin)
	}
}

func TestFindPlugin_NotFound(t *testing.T) {
	found := findPlugin("bureau-does-not-exist-xyzzy")
	if found != "" {
		t.Errorf("findPlugin(nonexistent) = %q, want empty string", found)
	}
}

// assertEnvContains checks that an env slice contains key=value.
func assertEnvContains(t *testing.T, env []string, key, wantValue string) {
	t.Helper()
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			gotValue := strings.TrimPrefix(entry, prefix)
			if gotValue != wantValue {
				t.Errorf("env %s = %q, want %q", key, gotValue, wantValue)
			}
			return
		}
	}
	t.Errorf("env missing %s (want %q)", key, wantValue)
}

// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMachineConfPath_Default(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", "")
	if got := MachineConfPath(); got != DefaultMachineConfPath {
		t.Errorf("MachineConfPath() = %q, want %q", got, DefaultMachineConfPath)
	}
}

func TestMachineConfPath_EnvOverride(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", "/custom/path/machine.conf")
	if got := MachineConfPath(); got != "/custom/path/machine.conf" {
		t.Errorf("MachineConfPath() = %q, want /custom/path/machine.conf", got)
	}
}

func TestLoadMachineConf_AllKeys(t *testing.T) {
	confPath := writeMachineConfFile(t,
		"BUREAU_HOMESERVER_URL=http://matrix.example.com:6167\n"+
			"BUREAU_MACHINE_NAME=bureau/fleet/prod/machine/worker-01\n"+
			"BUREAU_SERVER_NAME=example.com\n"+
			"BUREAU_FLEET=bureau/fleet/prod\n"+
			"BUREAU_SYSTEM_USER=bureau-test\n"+
			"BUREAU_OPERATORS_GROUP=bureau-test-operators\n",
	)
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	conf := LoadMachineConf()
	if conf.HomeserverURL != "http://matrix.example.com:6167" {
		t.Errorf("HomeserverURL = %q, want http://matrix.example.com:6167", conf.HomeserverURL)
	}
	if conf.MachineName != "bureau/fleet/prod/machine/worker-01" {
		t.Errorf("MachineName = %q, want bureau/fleet/prod/machine/worker-01", conf.MachineName)
	}
	if conf.ServerName != "example.com" {
		t.Errorf("ServerName = %q, want example.com", conf.ServerName)
	}
	if conf.Fleet != "bureau/fleet/prod" {
		t.Errorf("Fleet = %q, want bureau/fleet/prod", conf.Fleet)
	}
	if conf.SystemUser != "bureau-test" {
		t.Errorf("SystemUser = %q, want bureau-test", conf.SystemUser)
	}
	if conf.OperatorsGroup != "bureau-test-operators" {
		t.Errorf("OperatorsGroup = %q, want bureau-test-operators", conf.OperatorsGroup)
	}
}

func TestLoadMachineConf_MissingFile(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent"))

	conf := LoadMachineConf()
	if conf != (MachineConf{}) {
		t.Errorf("LoadMachineConf() for missing file = %+v, want zero value", conf)
	}
}

func TestLoadMachineConf_PartialKeys(t *testing.T) {
	confPath := writeMachineConfFile(t,
		"BUREAU_SERVER_NAME=partial.example.com\n"+
			"BUREAU_FLEET=bureau/fleet/staging\n",
	)
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	conf := LoadMachineConf()
	if conf.ServerName != "partial.example.com" {
		t.Errorf("ServerName = %q, want partial.example.com", conf.ServerName)
	}
	if conf.Fleet != "bureau/fleet/staging" {
		t.Errorf("Fleet = %q, want bureau/fleet/staging", conf.Fleet)
	}
	if conf.HomeserverURL != "" {
		t.Errorf("HomeserverURL = %q, want empty", conf.HomeserverURL)
	}
	if conf.MachineName != "" {
		t.Errorf("MachineName = %q, want empty", conf.MachineName)
	}
}

func TestLoadMachineConf_CommentsAndBlanks(t *testing.T) {
	confPath := writeMachineConfFile(t,
		"# Bureau machine configuration.\n"+
			"# Written by bureau machine doctor --fix.\n"+
			"\n"+
			"BUREAU_SERVER_NAME=commented.example.com\n"+
			"\n"+
			"# Fleet configuration\n"+
			"BUREAU_FLEET=bureau/fleet/dev\n",
	)
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	conf := LoadMachineConf()
	if conf.ServerName != "commented.example.com" {
		t.Errorf("ServerName = %q, want commented.example.com", conf.ServerName)
	}
	if conf.Fleet != "bureau/fleet/dev" {
		t.Errorf("Fleet = %q, want bureau/fleet/dev", conf.Fleet)
	}
}

func TestResolveServerName_ExplicitWins(t *testing.T) {
	confPath := writeMachineConfFile(t, "BUREAU_SERVER_NAME=from-conf.example.com\n")
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	if got := ResolveServerName("explicit.example.com"); got != "explicit.example.com" {
		t.Errorf("ResolveServerName(explicit) = %q, want explicit.example.com", got)
	}
}

func TestResolveServerName_MachineConf(t *testing.T) {
	confPath := writeMachineConfFile(t, "BUREAU_SERVER_NAME=from-conf.example.com\n")
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	if got := ResolveServerName(""); got != "from-conf.example.com" {
		t.Errorf("ResolveServerName('') = %q, want from-conf.example.com", got)
	}
}

func TestResolveServerName_Fallback(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent"))

	if got := ResolveServerName(""); got != "bureau.local" {
		t.Errorf("ResolveServerName('') without machine.conf = %q, want bureau.local", got)
	}
}

func TestResolveFleet_ExplicitWins(t *testing.T) {
	confPath := writeMachineConfFile(t, "BUREAU_FLEET=bureau/fleet/conf\n")
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	if got := ResolveFleet("bureau/fleet/explicit"); got != "bureau/fleet/explicit" {
		t.Errorf("ResolveFleet(explicit) = %q, want bureau/fleet/explicit", got)
	}
}

func TestResolveFleet_MachineConf(t *testing.T) {
	confPath := writeMachineConfFile(t, "BUREAU_FLEET=bureau/fleet/from-conf\n")
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	if got := ResolveFleet(""); got != "bureau/fleet/from-conf" {
		t.Errorf("ResolveFleet('') = %q, want bureau/fleet/from-conf", got)
	}
}

func TestResolveFleet_NoFallback(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent"))

	if got := ResolveFleet(""); got != "" {
		t.Errorf("ResolveFleet('') without machine.conf = %q, want empty", got)
	}
}

func TestResolveHomeserverURL_ExplicitWins(t *testing.T) {
	confPath := writeMachineConfFile(t, "BUREAU_HOMESERVER_URL=http://conf:6167\n")
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	if got := ResolveHomeserverURL("http://explicit:6167", "http://fallback:6167"); got != "http://explicit:6167" {
		t.Errorf("ResolveHomeserverURL(explicit) = %q, want http://explicit:6167", got)
	}
}

func TestResolveHomeserverURL_MachineConf(t *testing.T) {
	confPath := writeMachineConfFile(t, "BUREAU_HOMESERVER_URL=http://conf:6167\n")
	t.Setenv("BUREAU_MACHINE_CONF", confPath)

	if got := ResolveHomeserverURL("", "http://fallback:6167"); got != "http://conf:6167" {
		t.Errorf("ResolveHomeserverURL('') = %q, want http://conf:6167", got)
	}
}

func TestResolveHomeserverURL_Fallback(t *testing.T) {
	t.Setenv("BUREAU_MACHINE_CONF", filepath.Join(t.TempDir(), "nonexistent"))

	if got := ResolveHomeserverURL("", "http://fallback:6167"); got != "http://fallback:6167" {
		t.Errorf("ResolveHomeserverURL('', fallback) = %q, want http://fallback:6167", got)
	}
}

// writeMachineConfFile creates a temporary machine.conf file with the
// given content and returns its path.
func writeMachineConfFile(t *testing.T, content string) string {
	t.Helper()
	confPath := filepath.Join(t.TempDir(), "machine.conf")
	if err := os.WriteFile(confPath, []byte(content), 0644); err != nil {
		t.Fatalf("write machine.conf: %v", err)
	}
	return confPath
}

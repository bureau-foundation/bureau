// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func TestBindFlags_BasicTypes(t *testing.T) {
	type params struct {
		Name     string        `flag:"name" desc:"the name"`
		Verbose  bool          `flag:"verbose,v" desc:"enable verbose output"`
		Count    int           `flag:"count" desc:"number of items"`
		Offset   int64         `flag:"offset" desc:"byte offset"`
		Rate     float64       `flag:"rate" desc:"sampling rate"`
		Timeout  time.Duration `flag:"timeout" desc:"request timeout"`
		Tags     []string      `flag:"tags" desc:"tag list"`
		Untagged string        // no flag tag — should be skipped
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	err := flagSet.Parse([]string{
		"--name", "alice",
		"-v",
		"--count", "42",
		"--offset", "1099511627776",
		"--rate", "0.95",
		"--timeout", "30s",
		"--tags", "a,b,c",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Name != "alice" {
		t.Errorf("Name = %q, want %q", p.Name, "alice")
	}
	if !p.Verbose {
		t.Error("Verbose = false, want true")
	}
	if p.Count != 42 {
		t.Errorf("Count = %d, want 42", p.Count)
	}
	if p.Offset != 1099511627776 {
		t.Errorf("Offset = %d, want 1099511627776", p.Offset)
	}
	if p.Rate != 0.95 {
		t.Errorf("Rate = %f, want 0.95", p.Rate)
	}
	if p.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", p.Timeout)
	}
	if len(p.Tags) != 3 || p.Tags[0] != "a" || p.Tags[1] != "b" || p.Tags[2] != "c" {
		t.Errorf("Tags = %v, want [a b c]", p.Tags)
	}
	if p.Untagged != "" {
		t.Errorf("Untagged = %q, want empty (should be skipped)", p.Untagged)
	}
}

func TestBindFlags_Defaults(t *testing.T) {
	type params struct {
		Host    string        `flag:"host" desc:"server host" default:"localhost"`
		Port    int           `flag:"port" desc:"server port" default:"8080"`
		Offset  int64         `flag:"offset" desc:"byte offset" default:"100"`
		Rate    float64       `flag:"rate" desc:"rate" default:"0.5"`
		Timeout time.Duration `flag:"timeout" desc:"timeout" default:"10s"`
		Debug   bool          `flag:"debug" desc:"debug mode" default:"true"`
		Tags    []string      `flag:"tags" desc:"tags" default:"x,y"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	// Parse with no arguments — should get all defaults.
	if err := flagSet.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Host != "localhost" {
		t.Errorf("Host = %q, want %q", p.Host, "localhost")
	}
	if p.Port != 8080 {
		t.Errorf("Port = %d, want 8080", p.Port)
	}
	if p.Offset != 100 {
		t.Errorf("Offset = %d, want 100", p.Offset)
	}
	if p.Rate != 0.5 {
		t.Errorf("Rate = %f, want 0.5", p.Rate)
	}
	if p.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", p.Timeout)
	}
	if !p.Debug {
		t.Error("Debug = false, want true")
	}
	if len(p.Tags) != 2 || p.Tags[0] != "x" || p.Tags[1] != "y" {
		t.Errorf("Tags = %v, want [x y]", p.Tags)
	}
}

func TestBindFlags_DefaultsOverriddenByCLI(t *testing.T) {
	type params struct {
		Host string `flag:"host" desc:"server host" default:"localhost"`
		Port int    `flag:"port" desc:"server port" default:"8080"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	if err := flagSet.Parse([]string{"--host", "example.com", "--port", "9090"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Host != "example.com" {
		t.Errorf("Host = %q, want %q", p.Host, "example.com")
	}
	if p.Port != 9090 {
		t.Errorf("Port = %d, want 9090", p.Port)
	}
}

// TestParamsBinder implements FlagBinder for testing. Named and embedded
// fields use this to verify that BindFlags calls AddFlags instead of
// reflecting tags. Exported so that reflect can call Interface() on it
// when embedded.
type TestParamsBinder struct {
	Alpha string
	Beta  int
}

func (b *TestParamsBinder) AddFlags(flagSet *pflag.FlagSet) {
	flagSet.StringVar(&b.Alpha, "alpha", "", "alpha value")
	flagSet.IntVar(&b.Beta, "beta", 0, "beta value")
}

func TestBindFlags_NamedFlagBinder(t *testing.T) {
	type params struct {
		Binder TestParamsBinder
		Extra  string `flag:"extra" desc:"extra flag"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	if err := flagSet.Parse([]string{"--alpha", "hello", "--beta", "7", "--extra", "world"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Binder.Alpha != "hello" {
		t.Errorf("Binder.Alpha = %q, want %q", p.Binder.Alpha, "hello")
	}
	if p.Binder.Beta != 7 {
		t.Errorf("Binder.Beta = %d, want 7", p.Binder.Beta)
	}
	if p.Extra != "world" {
		t.Errorf("Extra = %q, want %q", p.Extra, "world")
	}
}

func TestBindFlags_EmbeddedFlagBinder(t *testing.T) {
	type params struct {
		TestParamsBinder
		Extra string `flag:"extra" desc:"extra flag"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	if err := flagSet.Parse([]string{"--alpha", "hello", "--extra", "world"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Alpha != "hello" {
		t.Errorf("Alpha = %q, want %q", p.Alpha, "hello")
	}
	if p.Extra != "world" {
		t.Errorf("Extra = %q, want %q", p.Extra, "world")
	}
}

func TestBindFlags_EmbeddedStructRecursion(t *testing.T) {
	type inner struct {
		Foo string `flag:"foo" desc:"foo flag"`
		Bar int    `flag:"bar" desc:"bar flag"`
	}
	type params struct {
		inner
		Baz bool `flag:"baz" desc:"baz flag"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	if err := flagSet.Parse([]string{"--foo", "hello", "--bar", "5", "--baz"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Foo != "hello" {
		t.Errorf("Foo = %q, want %q", p.Foo, "hello")
	}
	if p.Bar != 5 {
		t.Errorf("Bar = %d, want 5", p.Bar)
	}
	if !p.Baz {
		t.Error("Baz = false, want true")
	}
}

func TestBindFlags_Shorthand(t *testing.T) {
	type params struct {
		Output  string `flag:"output,o" desc:"output path"`
		Verbose bool   `flag:"verbose,v" desc:"verbose mode"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	if err := flagSet.Parse([]string{"-o", "/tmp/out", "-v"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if p.Output != "/tmp/out" {
		t.Errorf("Output = %q, want %q", p.Output, "/tmp/out")
	}
	if !p.Verbose {
		t.Error("Verbose = false, want true")
	}
}

func TestBindFlags_ErrorNotPointer(t *testing.T) {
	type params struct {
		Name string `flag:"name"`
	}
	var p params
	err := BindFlags(p, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err == nil {
		t.Fatal("expected error for non-pointer, got nil")
	}
	if want := "params must be a pointer to a struct"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
}

func TestBindFlags_ErrorNotStruct(t *testing.T) {
	s := "not a struct"
	err := BindFlags(&s, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err == nil {
		t.Fatal("expected error for non-struct, got nil")
	}
}

func TestBindFlags_ErrorBadDefault(t *testing.T) {
	type params struct {
		Count int `flag:"count" default:"not_a_number"`
	}
	var p params
	err := BindFlags(&p, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err == nil {
		t.Fatal("expected error for bad default, got nil")
	}
}

func TestFlagsFromParams(t *testing.T) {
	type params struct {
		Name string `flag:"name" desc:"the name" default:"world"`
	}

	var p params
	flagSet := FlagsFromParams("test", &p)

	if err := flagSet.Parse([]string{"--name", "alice"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Name != "alice" {
		t.Errorf("Name = %q, want %q", p.Name, "alice")
	}
}

func TestFlagsFromParams_DefaultUsedWhenNotParsed(t *testing.T) {
	type params struct {
		Name string `flag:"name" desc:"the name" default:"world"`
	}

	var p params
	flagSet := FlagsFromParams("test", &p)

	if err := flagSet.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Name != "world" {
		t.Errorf("Name = %q, want %q", p.Name, "world")
	}
}

func TestFlagsFromParams_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil input, got none")
		}
	}()
	FlagsFromParams("test", nil)
}

func TestBindFlags_FieldsWithoutTagSkipped(t *testing.T) {
	type params struct {
		Tagged   string `flag:"tagged" desc:"has tag"`
		NoTag    string
		JSONOnly string `json:"json_only"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	// Only --tagged should be registered.
	if flagSet.Lookup("tagged") == nil {
		t.Error("expected --tagged to be registered")
	}
	if flagSet.Lookup("no-tag") != nil {
		t.Error("expected no --no-tag flag")
	}
	if flagSet.Lookup("json_only") != nil {
		t.Error("expected no --json_only flag")
	}
}

func TestBindFlags_SessionConfigCompatibility(t *testing.T) {
	// Verify that SessionConfig (which implements FlagBinder via AddFlags)
	// works as a named struct field in a params struct.
	type params struct {
		Session    SessionConfig
		ServerName string `flag:"server-name" desc:"server name" default:"bureau.local"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	// SessionConfig flags should be registered.
	if flagSet.Lookup("credential-file") == nil {
		t.Error("expected --credential-file from SessionConfig")
	}
	if flagSet.Lookup("homeserver") == nil {
		t.Error("expected --homeserver from SessionConfig")
	}
	if flagSet.Lookup("token") == nil {
		t.Error("expected --token from SessionConfig")
	}
	if flagSet.Lookup("user-id") == nil {
		t.Error("expected --user-id from SessionConfig")
	}
	// Own flags should also be registered.
	if flagSet.Lookup("server-name") == nil {
		t.Error("expected --server-name")
	}

	if err := flagSet.Parse([]string{"--credential-file", "/tmp/creds", "--server-name", "example.com"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Session.CredentialFile != "/tmp/creds" {
		t.Errorf("Session.CredentialFile = %q, want %q", p.Session.CredentialFile, "/tmp/creds")
	}
	if p.ServerName != "example.com" {
		t.Errorf("ServerName = %q, want %q", p.ServerName, "example.com")
	}
}

func TestBindFlags_PositionalArgsRemain(t *testing.T) {
	type params struct {
		Format string `flag:"format" desc:"output format" default:"table"`
	}

	var p params
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := BindFlags(&p, flagSet); err != nil {
		t.Fatalf("BindFlags: %v", err)
	}

	if err := flagSet.Parse([]string{"--format", "json", "positional-arg"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	remaining := flagSet.Args()
	if len(remaining) != 1 || remaining[0] != "positional-arg" {
		t.Errorf("remaining args = %v, want [positional-arg]", remaining)
	}
	if p.Format != "json" {
		t.Errorf("Format = %q, want %q", p.Format, "json")
	}
}

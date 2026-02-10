// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for the proxy server.
type Config struct {
	// SocketPath is the path to the Unix socket.
	// Defaults to /run/bureau/proxy.sock.
	SocketPath string `yaml:"socket_path"`

	// AdminSocketPath is an optional separate Unix socket for admin operations.
	// When set, admin endpoints (dynamic service registration/removal) are
	// available on this socket. The daemon connects here to configure service
	// routing; agents never see this socket (it is not bind-mounted into
	// sandboxes).
	AdminSocketPath string `yaml:"admin_socket_path"`

	// ObserveSocketPath is an optional Unix socket for observation forwarding.
	// When set, the proxy listens on this socket and transparently forwards
	// observation requests to the daemon's observation relay, injecting the
	// proxy's own Matrix credentials (MATRIX_USER_ID and MATRIX_TOKEN).
	// This is the only path for sandboxed agents to observe other principals.
	ObserveSocketPath string `yaml:"observe_socket_path"`

	// DaemonObserveSocket is the path to the daemon's observation socket.
	// The proxy connects here when forwarding observation requests.
	// Defaults to /run/bureau/observe.sock if empty.
	DaemonObserveSocket string `yaml:"daemon_observe_socket"`

	// ListenAddress is an optional TCP address to listen on (e.g., "127.0.0.1:8080").
	// If set, the proxy listens on both the Unix socket and TCP.
	// This is useful for agents that can't use Unix sockets directly (e.g., HTTP SDKs).
	ListenAddress string `yaml:"listen_address"`

	// Services defines the available proxy services.
	Services map[string]ServiceConfig `yaml:"services"`
}

// ServiceConfig defines a single proxied service.
type ServiceConfig struct {
	// Type specifies the service type: "cli" or "http".
	Type string `yaml:"type"`

	// ---- CLI service fields ----

	// Binary is the path to the executable (for CLI services).
	Binary string `yaml:"binary"`

	// EnvVars maps environment variable names to credential configurations.
	// Supports two forms:
	//   Simple:   GH_TOKEN: github-pat
	//   Extended: GOOGLE_APPLICATION_CREDENTIALS: {credential: gcloud-sa, type: file}
	EnvVars map[string]EnvVarConfig `yaml:"env_vars"`

	// Allowed is a list of glob patterns for allowed commands (CLI) or paths (HTTP).
	// Empty means all commands/paths are allowed (subject to Blocked).
	Allowed []string `yaml:"allowed"`

	// Blocked is a list of glob patterns for blocked commands (CLI) or paths (HTTP).
	// Blocked takes precedence over Allowed.
	Blocked []string `yaml:"blocked"`

	// ---- HTTP service fields ----

	// Upstream is the target URL for HTTP proxy services (e.g., "https://api.openai.com").
	Upstream string `yaml:"upstream"`

	// InjectHeaders maps header names to credential names.
	// The credential value is injected as the header value.
	// Example: {"Authorization": "openai-bearer"} where openai-bearer credential
	// contains "Bearer sk-..."
	InjectHeaders map[string]string `yaml:"inject_headers"`

	// StripHeaders lists headers to remove from incoming requests.
	// Useful for removing the agent's dummy Authorization header.
	StripHeaders []string `yaml:"strip_headers"`
}

// EnvVarConfig specifies how a credential is injected into an environment variable.
type EnvVarConfig struct {
	// Credential is the name of the credential to inject.
	Credential string `yaml:"credential"`

	// Type specifies how to inject the credential:
	//   "value" (default): inject the credential content as the env var value
	//   "file": write credential to a temp file, inject the file path
	Type string `yaml:"type"`
}

// UnmarshalYAML implements custom unmarshaling to support both string and struct forms.
// Simple form:   GH_TOKEN: github-pat
// Extended form: GOOGLE_APPLICATION_CREDENTIALS: {credential: gcloud-sa, type: file}
func (e *EnvVarConfig) UnmarshalYAML(value *yaml.Node) error {
	// Try simple string form first
	if value.Kind == yaml.ScalarNode {
		e.Credential = value.Value
		e.Type = "value"
		return nil
	}

	// Try extended struct form
	type rawEnvVarConfig EnvVarConfig
	var raw rawEnvVarConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}

	e.Credential = raw.Credential
	e.Type = raw.Type
	if e.Type == "" {
		e.Type = "value"
	}

	return nil
}

// LoadConfig loads a configuration from a YAML file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Apply defaults
	if config.SocketPath == "" {
		config.SocketPath = "/run/bureau/proxy.sock"
	}

	return &config, nil
}

// Validate checks that the configuration is valid.
func (c *Config) Validate() error {
	if c.SocketPath == "" {
		return fmt.Errorf("socket_path is required")
	}

	for name, svc := range c.Services {
		if svc.Type == "" {
			return fmt.Errorf("service %q: type is required", name)
		}

		switch svc.Type {
		case "cli":
			if svc.Binary == "" {
				return fmt.Errorf("service %q: binary is required for cli services", name)
			}
			for envVar, envConfig := range svc.EnvVars {
				if envConfig.Credential == "" {
					return fmt.Errorf("service %q: env var %q: credential is required", name, envVar)
				}
				if envConfig.Type != "value" && envConfig.Type != "file" {
					return fmt.Errorf("service %q: env var %q: unknown type %q (supported: value, file)", name, envVar, envConfig.Type)
				}
			}

		case "http":
			if svc.Upstream == "" {
				return fmt.Errorf("service %q: upstream is required for http services", name)
			}

		default:
			return fmt.Errorf("service %q: unknown type %q (supported: cli, http)", name, svc.Type)
		}
	}

	return nil
}

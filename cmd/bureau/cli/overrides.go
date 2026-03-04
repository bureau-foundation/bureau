// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/bureau-foundation/bureau/lib/principal"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// PrincipalOverrides holds the CLI flags for per-principal instance
// customization. Embed this anonymously in a command's params struct
// to get the standard set of override flags:
//
//	type myCreateParams struct {
//	    cli.SessionConfig
//	    cli.PrincipalOverrides
//	    Name string `flag:"name" desc:"principal name"`
//	    cli.JSONOutput
//	}
//
// The struct tag reflection in [BindFlags] recurses into embedded
// structs, so all seven flags are registered automatically. Call
// [PrincipalOverrides.Parse] in the Run function to validate and
// convert the raw flag values into typed fields.
type PrincipalOverrides struct {
	ExtraCredential          []string `json:"extra_credential"           flag:"extra-credential"           desc:"extra credential KEY=VALUE for the encrypted bundle (repeatable)"`
	ExtraEnv                 []string `json:"extra_env"                  flag:"extra-env"                  desc:"extra environment variable KEY=VALUE for the sandbox (repeatable)"`
	CommandOverride          []string `json:"command_override"           flag:"command-override"           desc:"override template command argv (repeatable, one element per flag)"`
	EnvironmentOverride      string   `json:"environment_override"      flag:"environment-override"       desc:"override template Nix environment store path"`
	RequiredServicesOverride []string `json:"required_services_override" flag:"required-services-override" desc:"override template required services (repeatable, replaces all)"`
	SecretEnv                []string `json:"secret_env"                 flag:"secret-env"                 desc:"map credential to env var: KEY=ENV_VAR (repeatable)"`
	SecretFile               []string `json:"secret_file"                flag:"secret-file"                desc:"map credential to file: KEY=FILE_PATH (repeatable)"`
}

// ParsedOverrides holds the validated, typed values produced by
// [PrincipalOverrides.Parse]. These map directly to fields in
// [principal.CreateParams] and [schema.PrincipalAssignment].
type ParsedOverrides struct {
	ExtraCredentials          map[string]string
	ExtraEnvironmentVariables map[string]string
	CommandOverride           []string
	EnvironmentOverride       string
	RequiredServicesOverride  []string
	SecretsOverride           []schema.SecretBinding
}

// Parse validates the raw flag values and returns typed overrides.
// Returns an error if any KEY=VALUE pair is malformed.
func (o *PrincipalOverrides) Parse() (*ParsedOverrides, error) {
	extraCredentials, err := ParseKeyValuePairs(o.ExtraCredential)
	if err != nil {
		return nil, Validation("invalid --extra-credential: %w", err)
	}
	extraEnvironmentVariables, err := ParseKeyValuePairs(o.ExtraEnv)
	if err != nil {
		return nil, Validation("invalid --extra-env: %w", err)
	}
	secretsOverride, err := ParseSecretBindings(o.SecretEnv, o.SecretFile)
	if err != nil {
		return nil, Validation("invalid secret binding: %w", err)
	}
	return &ParsedOverrides{
		ExtraCredentials:          extraCredentials,
		ExtraEnvironmentVariables: extraEnvironmentVariables,
		CommandOverride:           o.CommandOverride,
		EnvironmentOverride:       o.EnvironmentOverride,
		RequiredServicesOverride:  o.RequiredServicesOverride,
		SecretsOverride:           secretsOverride,
	}, nil
}

// ApplyTo copies the override fields into a [principal.CreateParams].
// This is the common case for agent create and service create where
// the overrides apply directly to the principal being created.
func (p *ParsedOverrides) ApplyTo(params *principal.CreateParams) {
	params.ExtraCredentials = p.ExtraCredentials
	params.ExtraEnvironmentVariables = p.ExtraEnvironmentVariables
	params.CommandOverride = p.CommandOverride
	params.EnvironmentOverride = p.EnvironmentOverride
	params.RequiredServicesOverride = p.RequiredServicesOverride
	params.SecretsOverride = p.SecretsOverride
}

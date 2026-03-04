// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/schema"
	"github.com/spf13/pflag"
)

// FlagBinder is implemented by types that bind their own flags manually.
// When a struct field's type implements FlagBinder, [BindFlags] calls
// AddFlags instead of reflecting struct tags. This allows pre-existing
// types like [SessionConfig] to participate in the params system without
// modification.
type FlagBinder interface {
	AddFlags(flagSet *pflag.FlagSet)
}

// FlagsFromParams creates a [pflag.FlagSet] with flags bound to the tagged
// fields of params. params must be a pointer to a struct. Panics on
// invalid input (programming error, not runtime data).
//
// This is the convenience wrapper for the common pattern:
//
//	var params myParams
//	command := &cli.Command{
//	    Flags: func() *pflag.FlagSet {
//	        return cli.FlagsFromParams("mycommand", &params)
//	    },
//	    Run: func(_ context.Context, args []string, _ *slog.Logger) error {
//	        // params fields are populated after flag parsing
//	    },
//	}
func FlagsFromParams(name string, params any) *pflag.FlagSet {
	flagSet := pflag.NewFlagSet(name, pflag.ContinueOnError)
	if err := BindFlags(params, flagSet); err != nil {
		panic(fmt.Sprintf("cli.FlagsFromParams(%q): %v", name, err))
	}
	return flagSet
}

// BindFlags registers pflag entries for each tagged field in params.
// params must be a pointer to a struct.
//
// # Struct tags
//
// Three tags control flag binding:
//
//   - flag:"name" or flag:"name,n" — the long flag name and optional single-
//     character shorthand. Fields without a flag tag are skipped.
//   - desc:"help text" — the flag's help description. Also used as the JSON
//     Schema description in future schema generation.
//   - default:"value" — the default value, parsed according to the field's
//     Go type. If omitted, the type's zero value is used.
//
// # Supported field types
//
// string, bool, int, int64, float64, [time.Duration], []string.
//
// # Struct composition
//
// Embedded struct fields are handled in two ways:
//
//   - If the field's type (via pointer) implements [FlagBinder], AddFlags
//     is called. This supports types like [SessionConfig] that manage
//     their own flags.
//   - Otherwise, the embedded struct's fields are bound recursively.
//
// Named (non-embedded) struct fields that implement [FlagBinder] are also
// bound via AddFlags.
func BindFlags(params any, flagSet *pflag.FlagSet) error {
	value := reflect.ValueOf(params)
	if value.Kind() != reflect.Ptr || value.Elem().Kind() != reflect.Struct {
		return Validation("params must be a pointer to a struct, got %T", params)
	}
	return bindStructFields(value.Elem(), flagSet)
}

// bindStructFields iterates over struct fields and binds them to flagSet.
func bindStructFields(structValue reflect.Value, flagSet *pflag.FlagSet) error {
	structType := structValue.Type()

	for i := range structType.NumField() {
		field := structType.Field(i)
		fieldValue := structValue.Field(i)

		// Struct fields (embedded or named) that implement FlagBinder
		// are bound via their own AddFlags method. The field must be
		// exported for reflect to call Interface() on it.
		if field.Type.Kind() == reflect.Struct && field.IsExported() && fieldValue.CanAddr() {
			if binder, ok := fieldValue.Addr().Interface().(FlagBinder); ok {
				binder.AddFlags(flagSet)
				continue
			}
		}

		// Embedded structs without FlagBinder: recurse into their fields.
		// This handles both exported and unexported embedded types.
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			if err := bindStructFields(fieldValue, flagSet); err != nil {
				return Internal("embedded %s: %w", field.Name, err)
			}
			continue
		}

		// Skip fields without a flag tag.
		flagTag := field.Tag.Get("flag")
		if flagTag == "" {
			continue
		}

		name, shorthand := parseFlagTag(flagTag)
		description := field.Tag.Get("desc")
		defaultString := field.Tag.Get("default")

		if !fieldValue.CanAddr() {
			return Internal("field %s: not addressable", field.Name)
		}

		if err := bindField(fieldValue, flagSet, name, shorthand, description, defaultString); err != nil {
			return Internal("field %s: %w", field.Name, err)
		}
	}

	return nil
}

// parseFlagTag splits "name" into ("name", "") and "name,n" into ("name", "n").
func parseFlagTag(tag string) (string, string) {
	name, shorthand, _ := strings.Cut(tag, ",")
	return name, shorthand
}

// bindField creates a pflag binding for a single struct field.
func bindField(fieldValue reflect.Value, flagSet *pflag.FlagSet, name, shorthand, description, defaultString string) error {
	pointer := fieldValue.Addr().Interface()

	switch target := pointer.(type) {
	case *string:
		flagSet.StringVarP(target, name, shorthand, defaultString, description)

	case *bool:
		defaultValue, err := parseBoolDefault(defaultString)
		if err != nil {
			return Validation("default for --%s: %w", name, err)
		}
		flagSet.BoolVarP(target, name, shorthand, defaultValue, description)

	case *int:
		defaultValue, err := parseIntDefault(defaultString)
		if err != nil {
			return Validation("default for --%s: %w", name, err)
		}
		flagSet.IntVarP(target, name, shorthand, defaultValue, description)

	case *int64:
		defaultValue, err := parseInt64Default(defaultString)
		if err != nil {
			return Validation("default for --%s: %w", name, err)
		}
		flagSet.Int64VarP(target, name, shorthand, defaultValue, description)

	case *float64:
		defaultValue, err := parseFloat64Default(defaultString)
		if err != nil {
			return Validation("default for --%s: %w", name, err)
		}
		flagSet.Float64VarP(target, name, shorthand, defaultValue, description)

	case *time.Duration:
		defaultValue, err := parseDurationDefault(defaultString)
		if err != nil {
			return Validation("default for --%s: %w", name, err)
		}
		flagSet.DurationVarP(target, name, shorthand, defaultValue, description)

	case *[]string:
		var defaultValue []string
		if defaultString != "" {
			defaultValue = strings.Split(defaultString, ",")
		}
		flagSet.StringSliceVarP(target, name, shorthand, defaultValue, description)

	default:
		return Validation("unsupported type %s for flag --%s", fieldValue.Type(), name)
	}

	return nil
}

func parseBoolDefault(s string) (bool, error) {
	if s == "" {
		return false, nil
	}
	return strconv.ParseBool(s)
}

func parseIntDefault(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func parseInt64Default(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func parseFloat64Default(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

func parseDurationDefault(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// ParseKeyValuePairs parses a slice of "KEY=VALUE" strings into a map.
// Returns nil if pairs is empty. Returns an error if any pair is missing
// the "=" separator or has an empty key.
func ParseKeyValuePairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("expected KEY=VALUE, got %q", pair)
		}
		if key == "" {
			return nil, fmt.Errorf("empty key in %q", pair)
		}
		result[key] = value
	}
	return result, nil
}

// ParseSecretBindings builds a slice of schema.SecretBinding from two
// repeatable KEY=VALUE flag slices: --secret-env KEY=ENV_VAR maps a
// credential bundle key to an environment variable, and --secret-file
// KEY=FILE_PATH maps it to a file at /run/bureau/secrets/FILE_PATH.
//
// When the same KEY appears in both slices, the entries are merged into
// a single SecretBinding with both Env and File set. Returns nil if
// both slices are empty. Returns an error on missing "=", empty key,
// or empty value.
func ParseSecretBindings(envPairs, filePairs []string) ([]schema.SecretBinding, error) {
	if len(envPairs) == 0 && len(filePairs) == 0 {
		return nil, nil
	}

	// Accumulate bindings keyed by credential name so that a key
	// appearing in both --secret-env and --secret-file merges into
	// one SecretBinding.
	type entry struct {
		env  string
		file string
	}
	byKey := make(map[string]*entry)
	// Track insertion order so the result is deterministic.
	var keyOrder []string

	for _, pair := range envPairs {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("--secret-env: expected KEY=ENV_VAR, got %q", pair)
		}
		if key == "" {
			return nil, fmt.Errorf("--secret-env: empty key in %q", pair)
		}
		if value == "" {
			return nil, fmt.Errorf("--secret-env: empty environment variable name in %q", pair)
		}
		if existing, ok := byKey[key]; ok {
			existing.env = value
		} else {
			byKey[key] = &entry{env: value}
			keyOrder = append(keyOrder, key)
		}
	}

	for _, pair := range filePairs {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("--secret-file: expected KEY=FILE_PATH, got %q", pair)
		}
		if key == "" {
			return nil, fmt.Errorf("--secret-file: empty key in %q", pair)
		}
		if value == "" {
			return nil, fmt.Errorf("--secret-file: empty file path in %q", pair)
		}
		if existing, ok := byKey[key]; ok {
			existing.file = value
		} else {
			byKey[key] = &entry{file: value}
			keyOrder = append(keyOrder, key)
		}
	}

	bindings := make([]schema.SecretBinding, 0, len(byKey))
	for _, key := range keyOrder {
		entry := byKey[key]
		bindings = append(bindings, schema.SecretBinding{
			Key:  key,
			Env:  entry.env,
			File: entry.file,
		})
	}
	return bindings, nil
}

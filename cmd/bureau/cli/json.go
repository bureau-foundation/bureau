// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"reflect"
)

// JSONOutput is an embeddable struct that adds --json output support to
// a command's parameter struct. Embedding it provides the --json flag
// (via struct tag processing in [BindFlags]) and the [EmitJSON] method
// for conditional JSON output.
//
// Usage:
//
//	type listParams struct {
//	    cli.JSONOutput
//	    Room string `json:"room" flag:"room" desc:"target room"`
//	}
//
//	// In Run:
//	if done, err := params.EmitJSON(entries); done {
//	    return err
//	}
//	// ... text formatting ...
type JSONOutput struct {
	OutputJSON bool `json:"-" flag:"json" desc:"output as JSON"`
}

// EmitJSON writes result as indented JSON to stdout if --json is set.
// Returns (true, nil) on success, (true, err) on write failure, or
// (false, nil) when --json is not set and the caller should proceed
// with text formatting.
//
// Nil slices are normalized to empty slices before serialization, so
// callers never need to guard against null JSON output.
func (j *JSONOutput) EmitJSON(result any) (bool, error) {
	if !j.OutputJSON {
		return false, nil
	}
	return true, WriteJSON(normalizeNilSlice(result))
}

// JSONOutputter is implemented by params structs that support JSON
// output mode. The MCP server uses this interface to force JSON output
// when invoking CLI commands as tools.
type JSONOutputter interface {
	SetJSONOutput(bool)
}

// SetJSONOutput enables or disables JSON output mode, satisfying the
// [JSONOutputter] interface.
func (j *JSONOutput) SetJSONOutput(enabled bool) {
	j.OutputJSON = enabled
}

// WriteJSON marshals value as indented JSON and writes it to stdout.
// This is the low-level output function. Most commands should use
// [JSONOutput.EmitJSON] instead, which handles the --json flag check
// and nil-slice normalization automatically.
func WriteJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// normalizeNilSlice returns an empty slice of the same type if value
// is a nil slice, so that JSON serialization produces [] instead of
// null. Returns value unchanged for all other types.
func normalizeNilSlice(value any) any {
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Slice && v.IsNil() {
		return reflect.MakeSlice(v.Type(), 0, 0).Interface()
	}
	return value
}

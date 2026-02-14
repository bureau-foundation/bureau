// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Schema is a minimal JSON Schema representation covering the subset
// needed for MCP tool input descriptions. It maps directly to the
// JSON Schema object that MCP's inputSchema field expects.
type Schema struct {
	// Type is the JSON Schema type: "object", "string", "boolean",
	// "integer", "number", or "array".
	Type string `json:"type"`

	// Description is a human-readable explanation of the parameter.
	// Populated from the desc struct tag.
	Description string `json:"description,omitempty"`

	// Properties maps property names to their schemas. Only set when
	// Type is "object".
	Properties map[string]*Schema `json:"properties,omitempty"`

	// Required lists property names that must be provided. Only set
	// when Type is "object".
	Required []string `json:"required,omitempty"`

	// Default is the default value for the parameter. Populated from
	// the default struct tag, parsed to the appropriate Go type so
	// it marshals correctly (string as string, int as number, etc.).
	Default any `json:"default,omitempty"`

	// Items describes the element type for array schemas.
	Items *Schema `json:"items,omitempty"`

	// Format is an optional format hint (e.g., "duration" for
	// time.Duration fields serialized as strings like "30s").
	Format string `json:"format,omitempty"`
}

// ParamsSchema generates a JSON Schema from a parameter struct's type
// information. The schema describes the JSON input format for the
// command — property names come from json struct tags, descriptions
// from desc tags, and defaults from default tags.
//
// Fields are included in the schema when they have a json tag that is
// not "-". FlagBinder fields (like [SessionConfig]) are excluded since
// the MCP server handles authentication internally. Fields without a
// json tag are excluded.
//
// A field is marked required when it has a required:"true" tag. Fields
// with a default tag are always optional.
//
// params must be a pointer to a struct (same value passed to [BindFlags]).
func ParamsSchema(params any) (*Schema, error) {
	value := reflect.ValueOf(params)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return nil, fmt.Errorf("params must be a struct or pointer to struct, got %T", params)
	}

	return buildObjectSchema(value.Type())
}

// ParamsSchemaFromType generates a JSON Schema from a reflect.Type.
// The type must be a struct type.
func ParamsSchemaFromType(structType reflect.Type) (*Schema, error) {
	if structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("expected struct type, got %s", structType.Kind())
	}
	return buildObjectSchema(structType)
}

// buildObjectSchema constructs a JSON Schema object from a struct type.
func buildObjectSchema(structType reflect.Type) (*Schema, error) {
	schema := &Schema{
		Type:       "object",
		Properties: make(map[string]*Schema),
	}

	for i := range structType.NumField() {
		field := structType.Field(i)

		// Struct fields (embedded or named) that implement FlagBinder
		// are excluded — these are infrastructure (auth, connection
		// config) that the MCP server manages, not parameters the
		// agent provides. The field must be exported for the
		// Implements check.
		if field.Type.Kind() == reflect.Struct && field.IsExported() {
			if reflect.PointerTo(field.Type).Implements(flagBinderType) {
				continue
			}
		}

		// Embedded structs without FlagBinder: recurse and merge
		// their properties into the parent. This handles both
		// exported and unexported embedded types.
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			embedded, err := buildObjectSchema(field.Type)
			if err != nil {
				return nil, fmt.Errorf("embedded %s: %w", field.Name, err)
			}
			for name, prop := range embedded.Properties {
				schema.Properties[name] = prop
			}
			schema.Required = append(schema.Required, embedded.Required...)
			continue
		}

		// Skip unexported non-embedded fields.
		if !field.IsExported() {
			continue
		}

		// Determine the JSON property name.
		propertyName := jsonPropertyName(field)
		if propertyName == "" || propertyName == "-" {
			continue
		}

		// Build the property schema from the field's type and tags.
		propSchema, err := fieldSchema(field)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}

		schema.Properties[propertyName] = propSchema

		// Mark as required if explicitly tagged and no default provided.
		if field.Tag.Get("required") == "true" && field.Tag.Get("default") == "" {
			schema.Required = append(schema.Required, propertyName)
		}
	}

	// Remove properties key if empty (cleaner JSON output).
	if len(schema.Properties) == 0 {
		schema.Properties = nil
	}

	return schema, nil
}

// flagBinderType is the reflect.Type for the FlagBinder interface,
// cached for repeated use in type checks.
var flagBinderType = reflect.TypeOf((*FlagBinder)(nil)).Elem()

// jsonPropertyName extracts the JSON property name from a struct field's
// json tag. Returns "" if no json tag, or "-" if the field is excluded.
func jsonPropertyName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

// fieldSchema builds a JSON Schema for a single struct field based on
// its Go type and struct tags.
func fieldSchema(field reflect.StructField) (*Schema, error) {
	schema := &Schema{
		Description: field.Tag.Get("desc"),
	}

	// Set the schema type based on the Go type.
	switch field.Type.Kind() {
	case reflect.String:
		schema.Type = "string"
		// time.Duration is a special case: it's an int64 in Go but
		// serialized as a human-readable string like "30s" in JSON.
		if field.Type == reflect.TypeOf(time.Duration(0)) {
			schema.Format = "duration"
		}
	case reflect.Bool:
		schema.Type = "boolean"
	case reflect.Int, reflect.Int64:
		// Distinguish time.Duration from plain integers.
		if field.Type == reflect.TypeOf(time.Duration(0)) {
			schema.Type = "string"
			schema.Format = "duration"
		} else {
			schema.Type = "integer"
		}
	case reflect.Float64:
		schema.Type = "number"
	case reflect.Slice:
		if field.Type.Elem().Kind() == reflect.String {
			schema.Type = "array"
			schema.Items = &Schema{Type: "string"}
		} else {
			return nil, fmt.Errorf("unsupported slice element type %s", field.Type.Elem())
		}
	default:
		return nil, fmt.Errorf("unsupported type %s", field.Type)
	}

	// Parse default value.
	if defaultString := field.Tag.Get("default"); defaultString != "" {
		defaultValue, err := parseDefault(field.Type, defaultString)
		if err != nil {
			return nil, fmt.Errorf("default: %w", err)
		}
		schema.Default = defaultValue
	}

	return schema, nil
}

// parseDefault parses a default value string into the appropriate Go type
// so it marshals to the correct JSON type (number, boolean, etc.).
func parseDefault(fieldType reflect.Type, value string) (any, error) {
	// Handle time.Duration specially — it stays as a string in JSON.
	if fieldType == reflect.TypeOf(time.Duration(0)) {
		// Validate it parses, but keep the string representation.
		if _, err := time.ParseDuration(value); err != nil {
			return nil, err
		}
		return value, nil
	}

	switch fieldType.Kind() {
	case reflect.String:
		return value, nil
	case reflect.Bool:
		return strconv.ParseBool(value)
	case reflect.Int:
		v, err := strconv.Atoi(value)
		if err != nil {
			return nil, err
		}
		return v, nil
	case reflect.Int64:
		return strconv.ParseInt(value, 10, 64)
	case reflect.Float64:
		return strconv.ParseFloat(value, 64)
	case reflect.Slice:
		if fieldType.Elem().Kind() == reflect.String {
			return strings.Split(value, ","), nil
		}
		return nil, fmt.Errorf("unsupported slice type %s", fieldType)
	default:
		return nil, fmt.Errorf("unsupported type %s", fieldType)
	}
}

// SchemaJSON is a convenience function that generates a JSON Schema
// from a parameter struct and marshals it to indented JSON.
func SchemaJSON(params any) ([]byte, error) {
	schema, err := ParamsSchema(params)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(schema, "", "  ")
}

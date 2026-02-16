// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Schema is a JSON Schema representation covering the subset needed
// for MCP tool input and output descriptions. It maps directly to the
// JSON Schema objects that MCP's inputSchema and outputSchema fields
// expect.
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

	// AdditionalProperties describes the value type for map-typed
	// object schemas (e.g., map[string]int produces
	// {"type": "object", "additionalProperties": {"type": "integer"}}).
	AdditionalProperties *Schema `json:"additionalProperties,omitempty"`

	// Format is an optional format hint (e.g., "duration" for
	// time.Duration fields serialized as strings like "30s",
	// "date-time" for time.Time fields).
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
		return nil, Internal("params must be a struct or pointer to struct, got %T", params)
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
		return nil, Internal("expected struct type, got %s", structType.Kind())
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
				return nil, Internal("embedded %s: %w", field.Name, err)
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
			return nil, Internal("field %s: %w", field.Name, err)
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
// its Go type and struct tags. Primitive types (string, bool, int,
// float, []string, time.Duration) are handled directly with support
// for desc and default tags. Compound types (nested structs, complex
// slices, maps, pointers, arrays) delegate to [schemaForType] and
// overlay the desc tag.
func fieldSchema(field reflect.StructField) (*Schema, error) {
	description := field.Tag.Get("desc")

	fieldType := field.Type
	if fieldType.Kind() == reflect.Ptr {
		fieldType = fieldType.Elem()
	}

	// Primitive types support default values via struct tags.
	switch fieldType.Kind() {
	case reflect.String:
		return fieldSchemaWithDefault(&Schema{Type: "string", Description: description}, field)
	case reflect.Bool:
		return fieldSchemaWithDefault(&Schema{Type: "boolean", Description: description}, field)
	case reflect.Int, reflect.Int64:
		if fieldType == durationType {
			return fieldSchemaWithDefault(&Schema{Type: "string", Format: "duration", Description: description}, field)
		}
		return fieldSchemaWithDefault(&Schema{Type: "integer", Description: description}, field)
	case reflect.Float64:
		return fieldSchemaWithDefault(&Schema{Type: "number", Description: description}, field)
	case reflect.Slice:
		if fieldType.Elem().Kind() == reflect.String {
			return fieldSchemaWithDefault(&Schema{Type: "array", Items: &Schema{Type: "string"}, Description: description}, field)
		}
	}

	// Compound types (nested structs, complex slices, maps, arrays)
	// delegate to schemaForType for recursive schema generation.
	// Default values are not supported on compound types.
	schema, err := schemaForType(fieldType)
	if err != nil {
		return nil, err
	}
	schema.Description = description
	return schema, nil
}

// fieldSchemaWithDefault applies the default struct tag (if present)
// to a primitive field schema.
func fieldSchemaWithDefault(schema *Schema, field reflect.StructField) (*Schema, error) {
	if defaultString := field.Tag.Get("default"); defaultString != "" {
		defaultValue, err := parseDefault(field.Type, defaultString)
		if err != nil {
			return nil, Internal("default: %w", err)
		}
		schema.Default = defaultValue
	}
	return schema, nil
}

// parseDefault parses a default value string into the appropriate Go type
// so it marshals to the correct JSON type (number, boolean, etc.).
func parseDefault(fieldType reflect.Type, value string) (any, error) {
	// Handle time.Duration specially — it stays as a string in JSON.
	if fieldType == durationType {
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
		return nil, Internal("unsupported slice type %s", fieldType)
	default:
		return nil, Internal("unsupported type %s", fieldType)
	}
}

// OutputSchema generates a JSON Schema from a command's output type.
// Unlike [ParamsSchema] (which only handles struct types with
// input-specific tag conventions), OutputSchema handles the full range
// of command output shapes: structs, slices of structs, and primitives.
//
// Pointers are dereferenced. A slice value produces an array schema
// with items derived from the element type. A struct value produces
// an object schema using the same [buildObjectSchema] logic as input
// schemas (json tags for property names, desc tags for descriptions).
//
// output is the value returned by [Command.Output] — typically a
// pointer to the output type's zero value.
func OutputSchema(output any) (*Schema, error) {
	typ := reflect.TypeOf(output)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return schemaForType(typ)
}

// Well-known types that require special JSON Schema handling because
// their JSON representation differs from their Go structure.
var (
	timeType       = reflect.TypeOf(time.Time{})
	durationType   = reflect.TypeOf(time.Duration(0))
	rawMessageType = reflect.TypeOf(json.RawMessage{})
	byteSliceType  = reflect.TypeOf([]byte{})
)

// schemaForType generates a JSON Schema from a reflect.Type. It
// handles structs (via [buildObjectSchema]), slices, arrays, maps,
// pointers, and Go primitives. Types with custom JSON marshaling
// (time.Time, json.RawMessage, []byte) are special-cased to match
// their serialized form rather than their Go structure.
func schemaForType(typ reflect.Type) (*Schema, error) {
	// Types with custom JSON marshaling produce schemas matching
	// their serialized form, not their Go structure.
	switch typ {
	case timeType:
		return &Schema{Type: "string", Format: "date-time"}, nil
	case durationType:
		return &Schema{Type: "string", Format: "duration"}, nil
	case rawMessageType:
		// json.RawMessage passes through arbitrary JSON.
		return &Schema{}, nil
	case byteSliceType:
		// Go's json.Marshal encodes []byte as base64.
		return &Schema{Type: "string", Format: "byte"}, nil
	}

	switch typ.Kind() {
	case reflect.Struct:
		return buildObjectSchema(typ)
	case reflect.Slice:
		items, err := schemaForType(typ.Elem())
		if err != nil {
			return nil, Internal("array element: %w", err)
		}
		return &Schema{Type: "array", Items: items}, nil
	case reflect.Array:
		items, err := schemaForType(typ.Elem())
		if err != nil {
			return nil, Internal("array element: %w", err)
		}
		return &Schema{Type: "array", Items: items}, nil
	case reflect.Ptr:
		return schemaForType(typ.Elem())
	case reflect.String:
		return &Schema{Type: "string"}, nil
	case reflect.Bool:
		return &Schema{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &Schema{Type: "integer"}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}, nil
	case reflect.Map:
		if !mapKeyMarshalable(typ.Key().Kind()) {
			return nil, Internal("unsupported map key type %s", typ.Key())
		}
		// For map[K]any, the value type is interface{} which accepts
		// any JSON value — produce a plain object without
		// additionalProperties.
		if typ.Elem().Kind() == reflect.Interface {
			return &Schema{Type: "object"}, nil
		}
		valueSchema, err := schemaForType(typ.Elem())
		if err != nil {
			return &Schema{Type: "object"}, nil
		}
		return &Schema{Type: "object", AdditionalProperties: valueSchema}, nil
	case reflect.Interface:
		// interface{} / any — no type constraint.
		return &Schema{}, nil
	default:
		return nil, Internal("unsupported type %s (%s)", typ, typ.Kind())
	}
}

// mapKeyMarshalable returns true if the given kind can be used as a
// JSON object key. Go's json.Marshal converts integer keys to decimal
// strings and supports string keys directly.
func mapKeyMarshalable(kind reflect.Kind) bool {
	switch kind {
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
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

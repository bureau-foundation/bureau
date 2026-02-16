// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestParamsSchema_BasicTypes(t *testing.T) {
	type params struct {
		Name    string        `json:"name" flag:"name" desc:"the name"`
		Verbose bool          `json:"verbose" flag:"verbose" desc:"verbose output"`
		Count   int           `json:"count" flag:"count" desc:"number of items"`
		Offset  int64         `json:"offset" flag:"offset" desc:"byte offset"`
		Rate    float64       `json:"rate" flag:"rate" desc:"sampling rate"`
		Timeout time.Duration `json:"timeout" flag:"timeout" desc:"request timeout"`
		Tags    []string      `json:"tags" flag:"tags" desc:"tag list"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if schema.Type != "object" {
		t.Errorf("schema.Type = %q, want %q", schema.Type, "object")
	}

	cases := []struct {
		property    string
		schemaType  string
		description string
		format      string
	}{
		{"name", "string", "the name", ""},
		{"verbose", "boolean", "verbose output", ""},
		{"count", "integer", "number of items", ""},
		{"offset", "integer", "byte offset", ""},
		{"rate", "number", "sampling rate", ""},
		{"timeout", "string", "request timeout", "duration"},
		{"tags", "array", "tag list", ""},
	}

	for _, tc := range cases {
		prop, ok := schema.Properties[tc.property]
		if !ok {
			t.Errorf("missing property %q", tc.property)
			continue
		}
		if prop.Type != tc.schemaType {
			t.Errorf("%s.Type = %q, want %q", tc.property, prop.Type, tc.schemaType)
		}
		if prop.Description != tc.description {
			t.Errorf("%s.Description = %q, want %q", tc.property, prop.Description, tc.description)
		}
		if prop.Format != tc.format {
			t.Errorf("%s.Format = %q, want %q", tc.property, prop.Format, tc.format)
		}
	}

	// Verify array items schema.
	tagsProp := schema.Properties["tags"]
	if tagsProp.Items == nil {
		t.Fatal("tags.Items is nil")
	}
	if tagsProp.Items.Type != "string" {
		t.Errorf("tags.Items.Type = %q, want %q", tagsProp.Items.Type, "string")
	}
}

func TestParamsSchema_Defaults(t *testing.T) {
	type params struct {
		Host    string        `json:"host" flag:"host" desc:"server host" default:"localhost"`
		Port    int           `json:"port" flag:"port" desc:"server port" default:"8080"`
		Rate    float64       `json:"rate" flag:"rate" desc:"rate" default:"0.5"`
		Debug   bool          `json:"debug" flag:"debug" desc:"debug mode" default:"true"`
		Timeout time.Duration `json:"timeout" flag:"timeout" desc:"timeout" default:"10s"`
		Tags    []string      `json:"tags" flag:"tags" desc:"tags" default:"x,y"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	cases := []struct {
		property string
		expected any
	}{
		{"host", "localhost"},
		{"port", 8080},
		{"rate", 0.5},
		{"debug", true},
		{"timeout", "10s"},
		{"tags", []string{"x", "y"}},
	}

	for _, tc := range cases {
		prop := schema.Properties[tc.property]
		if prop == nil {
			t.Errorf("missing property %q", tc.property)
			continue
		}
		if !defaultsEqual(prop.Default, tc.expected) {
			t.Errorf("%s.Default = %v (%T), want %v (%T)",
				tc.property, prop.Default, prop.Default, tc.expected, tc.expected)
		}
	}
}

func TestParamsSchema_Required(t *testing.T) {
	type params struct {
		Room       string `json:"room" desc:"room alias" required:"true"`
		ServerName string `json:"server_name" flag:"server-name" desc:"server name" default:"bureau.local"`
		Optional   string `json:"optional" flag:"optional" desc:"optional field"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if len(schema.Required) != 1 || schema.Required[0] != "room" {
		t.Errorf("Required = %v, want [room]", schema.Required)
	}
}

func TestParamsSchema_RequiredWithDefaultNotRequired(t *testing.T) {
	// A field with both required:"true" and default:"..." should NOT
	// be in the required list — the default makes it optional.
	type params struct {
		Name string `json:"name" desc:"the name" required:"true" default:"world"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if len(schema.Required) != 0 {
		t.Errorf("Required = %v, want empty (field has default)", schema.Required)
	}
}

func TestParamsSchema_JSONDashExcluded(t *testing.T) {
	type params struct {
		ServerName string `json:"server_name" flag:"server-name" desc:"server name"`
		OutputJSON bool   `json:"-" flag:"json" desc:"output as JSON"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if _, ok := schema.Properties["server_name"]; !ok {
		t.Error("expected server_name property")
	}
	// OutputJSON should be excluded (json:"-").
	if len(schema.Properties) != 1 {
		t.Errorf("expected 1 property, got %d: %v", len(schema.Properties), propertyNames(schema))
	}
}

func TestParamsSchema_FlagBinderExcluded(t *testing.T) {
	type params struct {
		Session    SessionConfig
		ServerName string `json:"server_name" flag:"server-name" desc:"server name"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	// SessionConfig implements FlagBinder — should be excluded.
	if len(schema.Properties) != 1 {
		t.Errorf("expected 1 property (server_name only), got %d: %v",
			len(schema.Properties), propertyNames(schema))
	}
	if _, ok := schema.Properties["server_name"]; !ok {
		t.Error("expected server_name property")
	}
}

func TestParamsSchema_EmbeddedStructRecursion(t *testing.T) {
	type inner struct {
		Foo string `json:"foo" flag:"foo" desc:"foo param"`
	}
	type params struct {
		inner
		Bar string `json:"bar" flag:"bar" desc:"bar param"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if _, ok := schema.Properties["foo"]; !ok {
		t.Error("expected foo property from embedded struct")
	}
	if _, ok := schema.Properties["bar"]; !ok {
		t.Error("expected bar property")
	}
}

func TestParamsSchema_NoJSONTagSkipped(t *testing.T) {
	type params struct {
		WithTag    string `json:"with_tag" flag:"with-tag" desc:"has json tag"`
		WithoutTag string `flag:"without-tag" desc:"no json tag"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if _, ok := schema.Properties["with_tag"]; !ok {
		t.Error("expected with_tag property")
	}
	if len(schema.Properties) != 1 {
		t.Errorf("expected 1 property, got %d: %v", len(schema.Properties), propertyNames(schema))
	}
}

func TestParamsSchema_JSONOnlyField(t *testing.T) {
	// A field with json tag but no flag tag should still appear in
	// the schema — it's a parameter that comes from JSON input but
	// is positional in CLI mode.
	type params struct {
		Room       string `json:"room" desc:"room alias localpart" required:"true"`
		ServerName string `json:"server_name" flag:"server-name" desc:"server name" default:"bureau.local"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	if _, ok := schema.Properties["room"]; !ok {
		t.Error("expected room property (JSON-only, no flag tag)")
	}
	if _, ok := schema.Properties["server_name"]; !ok {
		t.Error("expected server_name property")
	}
}

func TestSchemaJSON_RoundTrip(t *testing.T) {
	type params struct {
		Room       string `json:"room" desc:"room alias localpart" required:"true"`
		ServerName string `json:"server_name" flag:"server-name" desc:"Matrix server name" default:"bureau.local"`
	}

	data, err := SchemaJSON(&params{})
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}

	// Verify it's valid JSON and round-trips back to Schema.
	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if schema.Type != "object" {
		t.Errorf("Type = %q, want %q", schema.Type, "object")
	}
	if len(schema.Properties) != 2 {
		t.Errorf("expected 2 properties, got %d", len(schema.Properties))
	}
	if len(schema.Required) != 1 || schema.Required[0] != "room" {
		t.Errorf("Required = %v, want [room]", schema.Required)
	}

	// Verify the JSON structure matches MCP expectations.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	properties, ok := raw["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties is not an object")
	}
	serverName, ok := properties["server_name"].(map[string]any)
	if !ok {
		t.Fatal("server_name is not an object")
	}
	if serverName["default"] != "bureau.local" {
		t.Errorf("server_name.default = %v, want %q", serverName["default"], "bureau.local")
	}
}

func TestParamsSchemaFromType(t *testing.T) {
	type params struct {
		Name string `json:"name" desc:"the name"`
	}

	schema, err := ParamsSchemaFromType(reflect.TypeOf(params{}))
	if err != nil {
		t.Fatalf("ParamsSchemaFromType: %v", err)
	}
	if _, ok := schema.Properties["name"]; !ok {
		t.Error("expected name property")
	}
}

// --- OutputSchema tests ---

func TestOutputSchema_Struct(t *testing.T) {
	type entry struct {
		Name        string `json:"name"        desc:"entry name"`
		Description string `json:"description" desc:"human-readable description"`
		Count       int    `json:"count"       desc:"item count"`
	}

	schema, err := OutputSchema(&entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	if schema.Type != "object" {
		t.Errorf("Type = %q, want %q", schema.Type, "object")
	}
	if len(schema.Properties) != 3 {
		t.Errorf("expected 3 properties, got %d: %v", len(schema.Properties), propertyNames(schema))
	}

	nameProp := schema.Properties["name"]
	if nameProp == nil {
		t.Fatal("missing name property")
	}
	if nameProp.Type != "string" {
		t.Errorf("name.Type = %q, want %q", nameProp.Type, "string")
	}
	if nameProp.Description != "entry name" {
		t.Errorf("name.Description = %q, want %q", nameProp.Description, "entry name")
	}

	countProp := schema.Properties["count"]
	if countProp == nil {
		t.Fatal("missing count property")
	}
	if countProp.Type != "integer" {
		t.Errorf("count.Type = %q, want %q", countProp.Type, "integer")
	}
}

func TestOutputSchema_SliceOfStructs(t *testing.T) {
	type entry struct {
		Name  string `json:"name"  desc:"entry name"`
		Steps int    `json:"steps" desc:"step count"`
	}

	schema, err := OutputSchema(&[]entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	if schema.Type != "array" {
		t.Fatalf("Type = %q, want %q", schema.Type, "array")
	}
	if schema.Items == nil {
		t.Fatal("Items is nil for array schema")
	}
	if schema.Items.Type != "object" {
		t.Errorf("Items.Type = %q, want %q", schema.Items.Type, "object")
	}
	if len(schema.Items.Properties) != 2 {
		t.Errorf("expected 2 item properties, got %d", len(schema.Items.Properties))
	}
	if schema.Items.Properties["name"] == nil {
		t.Error("missing name property in items")
	}
	if schema.Items.Properties["steps"] == nil {
		t.Error("missing steps property in items")
	}
}

func TestOutputSchema_SliceOfStrings(t *testing.T) {
	schema, err := OutputSchema(&[]string{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	if schema.Type != "array" {
		t.Fatalf("Type = %q, want %q", schema.Type, "array")
	}
	if schema.Items == nil {
		t.Fatal("Items is nil for array schema")
	}
	if schema.Items.Type != "string" {
		t.Errorf("Items.Type = %q, want %q", schema.Items.Type, "string")
	}
}

func TestOutputSchema_Primitive(t *testing.T) {
	schema, err := OutputSchema(new(string))
	if err != nil {
		t.Fatalf("OutputSchema(string): %v", err)
	}
	if schema.Type != "string" {
		t.Errorf("Type = %q, want %q", schema.Type, "string")
	}

	schema, err = OutputSchema(new(int))
	if err != nil {
		t.Fatalf("OutputSchema(int): %v", err)
	}
	if schema.Type != "integer" {
		t.Errorf("Type = %q, want %q", schema.Type, "integer")
	}

	schema, err = OutputSchema(new(bool))
	if err != nil {
		t.Fatalf("OutputSchema(bool): %v", err)
	}
	if schema.Type != "boolean" {
		t.Errorf("Type = %q, want %q", schema.Type, "boolean")
	}
}

func TestOutputSchema_MapStringKeys(t *testing.T) {
	schema, err := OutputSchema(&map[string]any{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("Type = %q, want %q", schema.Type, "object")
	}
}

func TestOutputSchema_JSONRoundTrip(t *testing.T) {
	// Verify that a slice-of-struct output schema produces valid JSON
	// that matches MCP's outputSchema expectations.
	type entry struct {
		Name  string `json:"name"  desc:"item name"`
		Value int    `json:"value" desc:"item value"`
	}

	schema, err := OutputSchema(&[]entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if raw["type"] != "array" {
		t.Errorf("type = %v, want %q", raw["type"], "array")
	}
	items, ok := raw["items"].(map[string]any)
	if !ok {
		t.Fatal("items is not an object")
	}
	if items["type"] != "object" {
		t.Errorf("items.type = %v, want %q", items["type"], "object")
	}
	properties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("items.properties is not an object")
	}
	nameProp, ok := properties["name"].(map[string]any)
	if !ok {
		t.Fatal("items.properties.name is not an object")
	}
	if nameProp["type"] != "string" {
		t.Errorf("name.type = %v, want %q", nameProp["type"], "string")
	}
}

// --- Compound type tests ---

func TestOutputSchema_NestedStruct(t *testing.T) {
	type inner struct {
		Name  string `json:"name"  desc:"inner name"`
		Count int    `json:"count" desc:"inner count"`
	}
	type outer struct {
		ID    string `json:"id"    desc:"outer ID"`
		Child inner  `json:"child" desc:"nested child object"`
	}

	schema, err := OutputSchema(&outer{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	if schema.Type != "object" {
		t.Fatalf("Type = %q, want %q", schema.Type, "object")
	}

	childProp := schema.Properties["child"]
	if childProp == nil {
		t.Fatal("missing child property")
	}
	if childProp.Type != "object" {
		t.Errorf("child.Type = %q, want %q", childProp.Type, "object")
	}
	if childProp.Description != "nested child object" {
		t.Errorf("child.Description = %q, want %q", childProp.Description, "nested child object")
	}
	if childProp.Properties["name"] == nil {
		t.Error("missing child.name property")
	}
	if childProp.Properties["count"] == nil {
		t.Error("missing child.count property")
	}
	if childProp.Properties["name"].Type != "string" {
		t.Errorf("child.name.Type = %q, want %q", childProp.Properties["name"].Type, "string")
	}
}

func TestOutputSchema_SliceOfStructsField(t *testing.T) {
	type item struct {
		Value string `json:"value" desc:"item value"`
	}
	type container struct {
		Items []item `json:"items" desc:"list of items"`
	}

	schema, err := OutputSchema(&container{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	itemsProp := schema.Properties["items"]
	if itemsProp == nil {
		t.Fatal("missing items property")
	}
	if itemsProp.Type != "array" {
		t.Errorf("items.Type = %q, want %q", itemsProp.Type, "array")
	}
	if itemsProp.Description != "list of items" {
		t.Errorf("items.Description = %q, want %q", itemsProp.Description, "list of items")
	}
	if itemsProp.Items == nil {
		t.Fatal("items.Items is nil")
	}
	if itemsProp.Items.Type != "object" {
		t.Errorf("items.Items.Type = %q, want %q", itemsProp.Items.Type, "object")
	}
	if itemsProp.Items.Properties["value"] == nil {
		t.Error("missing items.Items.value property")
	}
}

func TestOutputSchema_MapStringInt(t *testing.T) {
	type stats struct {
		ByStatus map[string]int `json:"by_status" desc:"count per status"`
	}

	schema, err := OutputSchema(&stats{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	prop := schema.Properties["by_status"]
	if prop == nil {
		t.Fatal("missing by_status property")
	}
	if prop.Type != "object" {
		t.Errorf("by_status.Type = %q, want %q", prop.Type, "object")
	}
	if prop.Description != "count per status" {
		t.Errorf("by_status.Description = %q, want %q", prop.Description, "count per status")
	}
	if prop.AdditionalProperties == nil {
		t.Fatal("by_status.AdditionalProperties is nil")
	}
	if prop.AdditionalProperties.Type != "integer" {
		t.Errorf("by_status.AdditionalProperties.Type = %q, want %q",
			prop.AdditionalProperties.Type, "integer")
	}
}

func TestOutputSchema_MapIntInt(t *testing.T) {
	type stats struct {
		ByPriority map[int]int `json:"by_priority" desc:"count per priority level"`
	}

	schema, err := OutputSchema(&stats{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	prop := schema.Properties["by_priority"]
	if prop == nil {
		t.Fatal("missing by_priority property")
	}
	if prop.Type != "object" {
		t.Errorf("by_priority.Type = %q, want %q", prop.Type, "object")
	}
	if prop.AdditionalProperties == nil {
		t.Fatal("by_priority.AdditionalProperties is nil")
	}
	if prop.AdditionalProperties.Type != "integer" {
		t.Errorf("by_priority.AdditionalProperties.Type = %q, want %q",
			prop.AdditionalProperties.Type, "integer")
	}
}

func TestOutputSchema_PointerField(t *testing.T) {
	type score struct {
		Value int `json:"value" desc:"score value"`
	}
	type entry struct {
		Score *score `json:"score" desc:"optional score"`
	}

	schema, err := OutputSchema(&entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	scoreProp := schema.Properties["score"]
	if scoreProp == nil {
		t.Fatal("missing score property")
	}
	if scoreProp.Type != "object" {
		t.Errorf("score.Type = %q, want %q", scoreProp.Type, "object")
	}
	if scoreProp.Description != "optional score" {
		t.Errorf("score.Description = %q, want %q", scoreProp.Description, "optional score")
	}
	if scoreProp.Properties["value"] == nil {
		t.Error("missing score.value property")
	}
}

func TestOutputSchema_TimeField(t *testing.T) {
	type entry struct {
		CreatedAt time.Time `json:"created_at" desc:"creation timestamp"`
	}

	schema, err := OutputSchema(&entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	prop := schema.Properties["created_at"]
	if prop == nil {
		t.Fatal("missing created_at property")
	}
	if prop.Type != "string" {
		t.Errorf("created_at.Type = %q, want %q", prop.Type, "string")
	}
	if prop.Format != "date-time" {
		t.Errorf("created_at.Format = %q, want %q", prop.Format, "date-time")
	}
	if prop.Description != "creation timestamp" {
		t.Errorf("created_at.Description = %q, want %q", prop.Description, "creation timestamp")
	}
}

func TestOutputSchema_ByteArray(t *testing.T) {
	type hash [32]byte
	type entry struct {
		FileHash hash `json:"file_hash" desc:"content hash"`
	}

	schema, err := OutputSchema(&entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	prop := schema.Properties["file_hash"]
	if prop == nil {
		t.Fatal("missing file_hash property")
	}
	// [32]byte marshals as a JSON array of 32 integers.
	if prop.Type != "array" {
		t.Errorf("file_hash.Type = %q, want %q", prop.Type, "array")
	}
	if prop.Items == nil {
		t.Fatal("file_hash.Items is nil")
	}
	if prop.Items.Type != "integer" {
		t.Errorf("file_hash.Items.Type = %q, want %q", prop.Items.Type, "integer")
	}
}

func TestOutputSchema_MapStringAny(t *testing.T) {
	// map[string]any produces an untyped object schema (no
	// additionalProperties since the value type is unknown).
	schema, err := OutputSchema(&map[string]any{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("Type = %q, want %q", schema.Type, "object")
	}
	// No additionalProperties for any-valued maps.
	if schema.AdditionalProperties != nil {
		t.Errorf("AdditionalProperties should be nil for map[string]any")
	}
}

func TestOutputSchema_ByteSlice(t *testing.T) {
	type entry struct {
		Data []byte `json:"data" desc:"binary data"`
	}

	schema, err := OutputSchema(&entry{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	prop := schema.Properties["data"]
	if prop == nil {
		t.Fatal("missing data property")
	}
	// []byte is base64-encoded in JSON.
	if prop.Type != "string" {
		t.Errorf("data.Type = %q, want %q", prop.Type, "string")
	}
	if prop.Format != "byte" {
		t.Errorf("data.Format = %q, want %q", prop.Format, "byte")
	}
}

func TestParamsSchema_NestedStructField(t *testing.T) {
	// Verify that nested struct fields also work in params schemas,
	// not just output schemas.
	type options struct {
		Verbose bool `json:"verbose" desc:"verbose output"`
	}
	type params struct {
		Name    string  `json:"name"    desc:"the name"`
		Options options `json:"options" desc:"command options"`
	}

	schema, err := ParamsSchema(&params{})
	if err != nil {
		t.Fatalf("ParamsSchema: %v", err)
	}

	optionsProp := schema.Properties["options"]
	if optionsProp == nil {
		t.Fatal("missing options property")
	}
	if optionsProp.Type != "object" {
		t.Errorf("options.Type = %q, want %q", optionsProp.Type, "object")
	}
	if optionsProp.Properties["verbose"] == nil {
		t.Error("missing options.verbose property")
	}
}

func TestOutputSchema_DeeplyNested(t *testing.T) {
	// Three levels of nesting: outer → middle → inner.
	type inner struct {
		Value string `json:"value" desc:"leaf value"`
	}
	type middle struct {
		Items []inner `json:"items" desc:"inner items"`
	}
	type outer struct {
		Child middle `json:"child" desc:"middle layer"`
	}

	schema, err := OutputSchema(&outer{})
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}

	child := schema.Properties["child"]
	if child == nil {
		t.Fatal("missing child")
	}
	items := child.Properties["items"]
	if items == nil {
		t.Fatal("missing child.items")
	}
	if items.Type != "array" {
		t.Errorf("items.Type = %q, want %q", items.Type, "array")
	}
	if items.Items == nil {
		t.Fatal("items.Items is nil")
	}
	if items.Items.Properties["value"] == nil {
		t.Error("missing items.Items.value")
	}
	if items.Items.Properties["value"].Description != "leaf value" {
		t.Errorf("leaf description = %q, want %q",
			items.Items.Properties["value"].Description, "leaf value")
	}
}

// defaultsEqual compares default values, handling []string specially
// since reflect.DeepEqual is not available in this comparison context
// and direct == comparison doesn't work for slices.
func defaultsEqual(got, want any) bool {
	// Handle []string comparison.
	gotSlice, gotIsSlice := got.([]string)
	wantSlice, wantIsSlice := want.([]string)
	if gotIsSlice && wantIsSlice {
		if len(gotSlice) != len(wantSlice) {
			return false
		}
		for i := range gotSlice {
			if gotSlice[i] != wantSlice[i] {
				return false
			}
		}
		return true
	}

	return got == want
}

// propertyNames returns a sorted list of property names for error messages.
func propertyNames(schema *Schema) []string {
	var names []string
	for name := range schema.Properties {
		names = append(names, name)
	}
	return names
}

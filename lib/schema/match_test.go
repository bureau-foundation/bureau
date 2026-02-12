// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"math"
	"testing"
)

// --- JSON Round-Trip Tests ---

func TestMatchValue_UnmarshalBareString(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`"active"`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	matched, err := m.Evaluate("active")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected Eq(\"active\") to match \"active\"")
	}

	// Round-trip: bare string should marshal back to bare string.
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `"active"` {
		t.Errorf("marshal = %s, want %q", data, `"active"`)
	}
}

func TestMatchValue_UnmarshalBareNumber(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`2`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	matched, err := m.Evaluate(float64(2))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected Eq(2) to match 2.0")
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `2` {
		t.Errorf("marshal = %s, want %q", data, "2")
	}
}

func TestMatchValue_UnmarshalBareBool(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`true`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	matched, err := m.Evaluate(true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected Eq(true) to match true")
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `true` {
		t.Errorf("marshal = %s, want %q", data, "true")
	}
}

func TestMatchValue_UnmarshalNull(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`null`), &m); err == nil {
		t.Error("expected error for null, got nil")
	}
}

func TestMatchValue_UnmarshalSingleOperator(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$lte": 2}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Should match 1 and 2, not 3.
	for _, test := range []struct {
		value   float64
		matched bool
	}{
		{1, true},
		{2, true},
		{3, false},
	} {
		matched, err := m.Evaluate(test.value)
		if err != nil {
			t.Fatalf("evaluate(%v): %v", test.value, err)
		}
		if matched != test.matched {
			t.Errorf("evaluate(%v) = %v, want %v", test.value, matched, test.matched)
		}
	}

	// Round-trip: single non-eq operator should marshal as $-prefix object.
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal round-tripped: %v", err)
	}
	if val, ok := roundTripped["$lte"]; !ok || val != float64(2) {
		t.Errorf("round-trip got %v, want {\"$lte\": 2}", roundTripped)
	}
}

func TestMatchValue_UnmarshalRange(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$gte": 1, "$lte": 3}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, test := range []struct {
		value   float64
		matched bool
	}{
		{0, false},
		{1, true},
		{2, true},
		{3, true},
		{4, false},
	} {
		matched, err := m.Evaluate(test.value)
		if err != nil {
			t.Fatalf("evaluate(%v): %v", test.value, err)
		}
		if matched != test.matched {
			t.Errorf("evaluate(%v) = %v, want %v", test.value, matched, test.matched)
		}
	}
}

func TestMatchValue_UnmarshalIn(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$in": [1, 2, 3]}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, test := range []struct {
		value   any
		matched bool
	}{
		{float64(1), true},
		{float64(2), true},
		{float64(3), true},
		{float64(4), false},
		{"1", false}, // String "1" != number 1.
	} {
		matched, err := m.Evaluate(test.value)
		if err != nil {
			t.Fatalf("evaluate(%v): %v", test.value, err)
		}
		if matched != test.matched {
			t.Errorf("evaluate(%v) = %v, want %v", test.value, matched, test.matched)
		}
	}
}

func TestMatchValue_UnmarshalInHeterogeneous(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$in": ["auto", 0, true]}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, test := range []struct {
		value   any
		matched bool
	}{
		{"auto", true},
		{float64(0), true},
		{true, true},
		{"manual", false},
		{float64(1), false},
		{false, false},
	} {
		matched, err := m.Evaluate(test.value)
		if err != nil {
			t.Fatalf("evaluate(%v): %v", test.value, err)
		}
		if matched != test.matched {
			t.Errorf("evaluate(%v) = %v, want %v", test.value, matched, test.matched)
		}
	}
}

// --- Unmarshal Error Tests ---

func TestMatchValue_UnmarshalUnknownOperator(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$regex": ".*"}`), &m); err == nil {
		t.Error("expected error for unknown operator $regex")
	}
}

func TestMatchValue_UnmarshalNonDollarKey(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"lte": 2}`), &m); err == nil {
		t.Error("expected error for key without $ prefix")
	}
}

func TestMatchValue_UnmarshalEmptyObject(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{}`), &m); err == nil {
		t.Error("expected error for empty object")
	}
}

func TestMatchValue_UnmarshalLteWithString(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$lte": "foo"}`), &m); err == nil {
		t.Error("expected error for $lte with string value")
	}
}

func TestMatchValue_UnmarshalInWithEmptyArray(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$in": []}`), &m); err == nil {
		t.Error("expected error for $in with empty array")
	}
}

func TestMatchValue_UnmarshalInWithNonArray(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$in": "not-array"}`), &m); err == nil {
		t.Error("expected error for $in with non-array value")
	}
}

func TestMatchValue_UnmarshalInWithNestedObject(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$in": [{"nested": true}]}`), &m); err == nil {
		t.Error("expected error for $in with object element")
	}
}

func TestMatchValue_UnmarshalEqWithArray(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$eq": [1, 2]}`), &m); err == nil {
		t.Error("expected error for $eq with array value")
	}
}

// --- Validation Tests ---

func TestMatchValue_ValidateConstructors(t *testing.T) {
	t.Parallel()
	valid := []MatchValue{
		Eq("active"),
		Eq(float64(2)),
		Eq(true),
		Lt(3),
		Lte(3),
		Gt(0),
		Gte(0),
		In("a", "b"),
		In(float64(1), float64(2)),
		In("auto", float64(0), true),
	}
	for _, m := range valid {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", m.StringValue(), err)
		}
	}
}

func TestMatchValue_ValidateRejectsNaN(t *testing.T) {
	t.Parallel()
	m := Eq(math.NaN())
	if err := m.Validate(); err == nil {
		t.Error("expected Validate to reject NaN")
	}
}

func TestMatchValue_ValidateRejectsInf(t *testing.T) {
	t.Parallel()
	m := Lt(math.Inf(1))
	if err := m.Validate(); err == nil {
		t.Error("expected Validate to reject Inf")
	}
}

func TestMatchValue_ValidateRejectsEmptyCriteria(t *testing.T) {
	t.Parallel()
	m := MatchValue{}
	if err := m.Validate(); err == nil {
		t.Error("expected Validate to reject empty criteria")
	}
}

func TestMatchValue_ValidateRejectsDuplicateOperator(t *testing.T) {
	t.Parallel()
	m := MatchValue{criteria: []matchCriterion{
		{op: "lte", value: float64(3)},
		{op: "lte", value: float64(5)},
	}}
	if err := m.Validate(); err == nil {
		t.Error("expected Validate to reject duplicate $lte")
	}
}

func TestMatchValue_ValidateRejectsNaNInArray(t *testing.T) {
	t.Parallel()
	m := In(float64(1), math.NaN())
	if err := m.Validate(); err == nil {
		t.Error("expected Validate to reject NaN in $in array")
	}
}

func TestContentMatch_Validate(t *testing.T) {
	t.Parallel()
	cm := ContentMatch{
		"status":   Eq("active"),
		"priority": Lte(2),
	}
	if err := cm.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestContentMatch_ValidateRejectsBadEntry(t *testing.T) {
	t.Parallel()
	cm := ContentMatch{
		"status":   Eq("active"),
		"priority": MatchValue{}, // Empty.
	}
	if err := cm.Validate(); err == nil {
		t.Error("expected Validate to reject empty MatchValue")
	}
}

// --- Evaluation: eq ---

func TestEvaluate_EqStringMatch(t *testing.T) {
	t.Parallel()
	m := Eq("active")
	matched, err := m.Evaluate("active")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected match")
	}
}

func TestEvaluate_EqStringMismatch(t *testing.T) {
	t.Parallel()
	m := Eq("active")
	matched, err := m.Evaluate("pending")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("expected no match")
	}
}

func TestEvaluate_EqNumericMatch(t *testing.T) {
	t.Parallel()
	m := Eq(float64(42))
	matched, err := m.Evaluate(float64(42))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected match")
	}
}

func TestEvaluate_EqTypeMismatch(t *testing.T) {
	t.Parallel()
	// String "2" must not match number 2.0.
	m := Eq("2")
	matched, err := m.Evaluate(float64(2))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("string Eq(\"2\") should not match float64(2)")
	}
}

func TestEvaluate_EqBoolMatch(t *testing.T) {
	t.Parallel()
	m := Eq(true)
	matched, err := m.Evaluate(true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected match")
	}
}

func TestEvaluate_EqBoolNotMatchNumber(t *testing.T) {
	t.Parallel()
	// Bool true must not match number 1.
	m := Eq(true)
	matched, err := m.Evaluate(float64(1))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("bool Eq(true) should not match float64(1)")
	}
}

func TestEvaluate_EqAgainstNil(t *testing.T) {
	t.Parallel()
	m := Eq("active")
	matched, err := m.Evaluate(nil)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("Eq(\"active\") should not match nil")
	}
}

// --- Evaluation: comparison operators ---

func TestEvaluate_Lt(t *testing.T) {
	t.Parallel()
	m := Lt(3)
	for _, test := range []struct {
		value   float64
		matched bool
	}{
		{2, true},
		{3, false},
		{4, false},
	} {
		matched, err := m.Evaluate(test.value)
		if err != nil {
			t.Fatalf("evaluate(%v): %v", test.value, err)
		}
		if matched != test.matched {
			t.Errorf("Lt(3).Evaluate(%v) = %v, want %v", test.value, matched, test.matched)
		}
	}
}

func TestEvaluate_Gt(t *testing.T) {
	t.Parallel()
	m := Gt(0)
	for _, test := range []struct {
		value   float64
		matched bool
	}{
		{-1, false},
		{0, false},
		{1, true},
	} {
		matched, err := m.Evaluate(test.value)
		if err != nil {
			t.Fatalf("evaluate(%v): %v", test.value, err)
		}
		if matched != test.matched {
			t.Errorf("Gt(0).Evaluate(%v) = %v, want %v", test.value, matched, test.matched)
		}
	}
}

func TestEvaluate_ComparisonAgainstString(t *testing.T) {
	t.Parallel()
	// Comparison against non-numeric target is a clean mismatch.
	m := Lte(5)
	matched, err := m.Evaluate("not-a-number")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("Lte(5) should not match string")
	}
}

// --- Evaluation: in ---

func TestEvaluate_InScalarMatch(t *testing.T) {
	t.Parallel()
	m := In("active", "pending")
	matched, err := m.Evaluate("active")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("expected match")
	}
}

func TestEvaluate_InScalarMismatch(t *testing.T) {
	t.Parallel()
	m := In("active", "pending")
	matched, err := m.Evaluate("closed")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("expected no match")
	}
}

func TestEvaluate_InTypeMismatch(t *testing.T) {
	t.Parallel()
	// String "1" is not in numeric set [1, 2, 3].
	m := In(float64(1), float64(2), float64(3))
	matched, err := m.Evaluate("1")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("string \"1\" should not match numeric In set")
	}
}

// --- Evaluation: array targets ---

func TestEvaluate_EqArrayContainment(t *testing.T) {
	t.Parallel()
	m := Eq("bug")
	matched, err := m.Evaluate([]any{"bug", "feature", "security"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("Eq(\"bug\") should match array containing \"bug\"")
	}
}

func TestEvaluate_EqArrayNotContained(t *testing.T) {
	t.Parallel()
	m := Eq("performance")
	matched, err := m.Evaluate([]any{"bug", "feature"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("Eq(\"performance\") should not match array without it")
	}
}

func TestEvaluate_EqArrayTypeMismatch(t *testing.T) {
	t.Parallel()
	// String "42" should not match number 42 in array.
	m := Eq("42")
	matched, err := m.Evaluate([]any{float64(42), true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("string Eq(\"42\") should not match numeric array element")
	}
}

func TestEvaluate_EqNumericArrayContainment(t *testing.T) {
	t.Parallel()
	// Numeric Eq(42) should match number 42 in array.
	m := Eq(float64(42))
	matched, err := m.Evaluate([]any{float64(42), true})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("numeric Eq(42) should match numeric 42 in array")
	}
}

func TestEvaluate_ComparisonAgainstArray(t *testing.T) {
	t.Parallel()
	// {"$lte": 2} against [1, 3, 5] should match because 1 <= 2.
	m := Lte(2)
	matched, err := m.Evaluate([]any{float64(1), float64(3), float64(5)})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("Lte(2) should match array containing 1")
	}
}

func TestEvaluate_ComparisonAgainstArrayNoMatch(t *testing.T) {
	t.Parallel()
	// {"$lte": 0} against [1, 3, 5] should not match.
	m := Lte(0)
	matched, err := m.Evaluate([]any{float64(1), float64(3), float64(5)})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("Lte(0) should not match array [1, 3, 5]")
	}
}

func TestEvaluate_InArrayIntersection(t *testing.T) {
	t.Parallel()
	// In("bug", "feature") against ["bug", "security"] — intersection is {"bug"}.
	m := In("bug", "feature")
	matched, err := m.Evaluate([]any{"bug", "security"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("In set should intersect with array")
	}
}

func TestEvaluate_InArrayNoIntersection(t *testing.T) {
	t.Parallel()
	m := In("docs", "refactor")
	matched, err := m.Evaluate([]any{"bug", "security"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("In set should not intersect with array")
	}
}

func TestEvaluate_EmptyArray(t *testing.T) {
	t.Parallel()
	// Empty array never satisfies any criterion.
	m := Eq("bug")
	matched, err := m.Evaluate([]any{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("Eq should not match empty array")
	}
}

// --- Evaluation: multi-criteria (range) ---

func TestEvaluate_RangeMatch(t *testing.T) {
	t.Parallel()
	// Construct range via JSON.
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$gte": 1, "$lte": 3}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	matched, err := m.Evaluate(float64(2))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("2 should be in range [1, 3]")
	}
}

func TestEvaluate_RangeMismatch(t *testing.T) {
	t.Parallel()
	var m MatchValue
	if err := json.Unmarshal([]byte(`{"$gte": 1, "$lte": 3}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	matched, err := m.Evaluate(float64(5))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("5 should not be in range [1, 3]")
	}
}

// --- ContentMatch.Evaluate ---

func TestContentMatch_EvaluateAllMatch(t *testing.T) {
	t.Parallel()
	cm := ContentMatch{
		"status":   Eq("active"),
		"priority": Lte(2),
	}
	content := map[string]any{
		"status":   "active",
		"priority": float64(1),
	}

	matched, key, err := cm.Evaluate(content)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Errorf("expected match, failed on key %q", key)
	}
}

func TestContentMatch_EvaluateMissingKey(t *testing.T) {
	t.Parallel()
	cm := ContentMatch{
		"status": Eq("active"),
	}
	content := map[string]any{
		"priority": float64(1),
	}

	matched, key, err := cm.Evaluate(content)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("expected mismatch for missing key")
	}
	if key != "status" {
		t.Errorf("failed key = %q, want %q", key, "status")
	}
}

func TestContentMatch_EvaluateValueMismatch(t *testing.T) {
	t.Parallel()
	cm := ContentMatch{
		"status": Eq("active"),
	}
	content := map[string]any{
		"status": "pending",
	}

	matched, key, err := cm.Evaluate(content)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("expected mismatch")
	}
	if key != "status" {
		t.Errorf("failed key = %q, want %q", key, "status")
	}
}

func TestContentMatch_EvaluateEmpty(t *testing.T) {
	t.Parallel()
	// Empty ContentMatch matches everything.
	cm := ContentMatch{}
	matched, _, err := cm.Evaluate(map[string]any{"anything": "goes"})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("empty ContentMatch should match any content")
	}
}

// --- ContentMatch JSON round-trip ---

func TestContentMatch_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	// This is the key backward-compatibility test: a bare string value
	// round-trips through JSON as a bare string, not as {"$eq": "active"}.
	type wrapper struct {
		ContentMatch ContentMatch `json:"content_match,omitempty"`
	}

	original := wrapper{
		ContentMatch: ContentMatch{
			"status": Eq("active"),
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Verify wire format is backward-compatible.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	contentMatch, ok := raw["content_match"].(map[string]any)
	if !ok {
		t.Fatalf("content_match is not an object: %v", raw)
	}
	if status, ok := contentMatch["status"].(string); !ok || status != "active" {
		t.Errorf("wire format status = %v, want bare string \"active\"", contentMatch["status"])
	}

	// Verify it round-trips back.
	var decoded wrapper
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal decoded: %v", err)
	}
	matched, err := decoded.ContentMatch["status"].Evaluate("active")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Error("round-tripped ContentMatch should match")
	}
}

func TestContentMatch_JSONRoundTripWithOperators(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		ContentMatch ContentMatch `json:"content_match,omitempty"`
	}

	// Marshal a ContentMatch with a range expression.
	input := `{"content_match": {"priority": {"$gte": 1, "$lte": 3}, "status": "active"}}`

	var decoded wrapper
	if err := json.Unmarshal([]byte(input), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify priority range works.
	content := map[string]any{
		"status":   "active",
		"priority": float64(2),
	}
	matched, key, err := decoded.ContentMatch.Evaluate(content)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched {
		t.Errorf("expected match, failed on key %q", key)
	}

	// Verify out-of-range priority fails.
	content["priority"] = float64(5)
	matched, _, err = decoded.ContentMatch.Evaluate(content)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Error("priority 5 should not match range [1, 3]")
	}
}

// --- StringValue ---

func TestMatchValue_StringValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		match    MatchValue
		expected string
	}{
		{"bare string", Eq("active"), "active"},
		{"bare number", Eq(float64(2)), "2"},
		{"bare bool", Eq(true), "true"},
		{"lt", Lt(3), "<3"},
		{"lte", Lte(3), "<=3"},
		{"gt", Gt(0), ">0"},
		{"gte", Gte(1), ">=1"},
		{"in strings", In("a", "b", "c"), "in [a, b, c]"},
		{"in numbers", In(float64(1), float64(2)), "in [1, 2]"},
		{"empty", MatchValue{}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := test.match.StringValue()
			if got != test.expected {
				t.Errorf("StringValue() = %q, want %q", got, test.expected)
			}
		})
	}
}

// --- Marshal empty MatchValue ---

func TestMatchValue_MarshalEmpty(t *testing.T) {
	t.Parallel()
	m := MatchValue{}
	_, err := json.Marshal(m)
	if err == nil {
		t.Error("expected error marshaling empty MatchValue")
	}
}

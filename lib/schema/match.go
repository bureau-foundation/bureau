// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ContentMatch is a map of field names to match criteria. All entries must
// be satisfied (AND across keys). Each value can be a bare scalar (shorthand
// for equality) or a $-prefixed operator object supporting comparisons and
// set membership.
//
// JSON examples:
//
//	{"status": "active"}                     — string equality
//	{"priority": 2}                          — numeric equality
//	{"priority": {"$lte": 2}}                — comparison
//	{"priority": {"$gte": 1, "$lte": 3}}     — range (operators AND)
//	{"hour": {"$in": [9, 10, 11, 12]}}       — set membership
type ContentMatch map[string]MatchValue

// Validate checks that every entry has valid operators and value types.
func (cm ContentMatch) Validate() error {
	for key, matcher := range cm {
		if err := matcher.Validate(); err != nil {
			return fmt.Errorf("content_match[%q]: %w", key, err)
		}
	}
	return nil
}

// Evaluate checks all entries against the given content map. Returns:
//   - (true, "", nil): all entries matched.
//   - (false, key, nil): the named key did not match (clean mismatch).
//   - (false, key, err): the named key has an invalid match expression.
//
// A missing key in content is a clean mismatch, not an error.
func (cm ContentMatch) Evaluate(content map[string]any) (bool, string, error) {
	for key, matcher := range cm {
		actual, exists := content[key]
		if !exists {
			return false, key, nil
		}
		matched, err := matcher.Evaluate(actual)
		if err != nil {
			return false, key, err
		}
		if !matched {
			return false, key, nil
		}
	}
	return true, "", nil
}

// matchCriterion is a single operator-value pair within a MatchValue.
type matchCriterion struct {
	op    string // "eq", "lt", "lte", "gt", "gte", "in"
	value any    // string, float64, bool, or []any (for "in")
}

// MatchValue holds one or more criteria for a single content field. When
// multiple criteria are present, all must be satisfied (AND semantics).
// This enables range expressions like {"$gte": 1, "$lte": 3}.
//
// Construct via helpers: Eq, Lt, Lte, Gt, Gte, In. Multi-criterion
// values are typically constructed from JSON (the $-prefix wire format).
type MatchValue struct {
	criteria []matchCriterion
}

// validOps is the set of recognized operator names.
var validOps = map[string]bool{
	"eq": true, "lt": true, "lte": true,
	"gt": true, "gte": true, "in": true,
}

// --- Constructors ---

// Eq returns a MatchValue for exact equality. v must be string, float64,
// or bool. For array targets, equality means containment (any element
// equals v).
func Eq(v any) MatchValue {
	return MatchValue{criteria: []matchCriterion{{op: "eq", value: v}}}
}

// Lt returns a MatchValue for numeric less-than comparison.
func Lt(v float64) MatchValue {
	return MatchValue{criteria: []matchCriterion{{op: "lt", value: v}}}
}

// Lte returns a MatchValue for numeric less-than-or-equal comparison.
func Lte(v float64) MatchValue {
	return MatchValue{criteria: []matchCriterion{{op: "lte", value: v}}}
}

// Gt returns a MatchValue for numeric greater-than comparison.
func Gt(v float64) MatchValue {
	return MatchValue{criteria: []matchCriterion{{op: "gt", value: v}}}
}

// Gte returns a MatchValue for numeric greater-than-or-equal comparison.
func Gte(v float64) MatchValue {
	return MatchValue{criteria: []matchCriterion{{op: "gte", value: v}}}
}

// In returns a MatchValue for set membership. Each element must be string,
// float64, or bool. Heterogeneous element types are allowed. For scalar
// targets, checks whether the target is in the set. For array targets,
// checks whether any element of the target is in the set (intersection).
func In(values ...any) MatchValue {
	set := make([]any, len(values))
	copy(set, values)
	return MatchValue{criteria: []matchCriterion{{op: "in", value: set}}}
}

// --- JSON ---

// UnmarshalJSON decodes a MatchValue from JSON. Accepts three forms:
//   - Bare scalar (string, number, boolean): shorthand for {"$eq": value}.
//   - Object with $-prefixed keys: each key is an operator, all AND together.
//   - Null: rejected (match values must be concrete).
func (m *MatchValue) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("invalid match value: %w", err)
	}

	switch v := raw.(type) {
	case string:
		m.criteria = []matchCriterion{{op: "eq", value: v}}

	case float64:
		m.criteria = []matchCriterion{{op: "eq", value: v}}

	case bool:
		m.criteria = []matchCriterion{{op: "eq", value: v}}

	case map[string]any:
		if len(v) == 0 {
			return errors.New("match value object must have at least one $-operator key")
		}
		m.criteria = make([]matchCriterion, 0, len(v))
		for key, val := range v {
			if !strings.HasPrefix(key, "$") {
				return fmt.Errorf("match value object keys must start with $, got %q", key)
			}
			op := key[1:]
			if !validOps[op] {
				return fmt.Errorf("unknown operator $%s", op)
			}
			if err := checkOperatorValue(op, val); err != nil {
				return err
			}
			m.criteria = append(m.criteria, matchCriterion{op: op, value: val})
		}

	case nil:
		return errors.New("match value must not be null")

	default:
		return fmt.Errorf("match value must be string, number, boolean, or $-operator object, got %T", raw)
	}

	return nil
}

// checkOperatorValue validates that a JSON-decoded value is the right type
// for the given operator. Called during unmarshal to fail fast on bad configs.
func checkOperatorValue(op string, value any) error {
	switch op {
	case "eq":
		switch value.(type) {
		case string, float64, bool:
			return nil
		default:
			return fmt.Errorf("$eq value must be string, number, or boolean, got %T", value)
		}

	case "lt", "lte", "gt", "gte":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("$%s value must be a number, got %T", op, value)
		}
		return nil

	case "in":
		arr, ok := value.([]any)
		if !ok {
			return fmt.Errorf("$in value must be an array, got %T", value)
		}
		if len(arr) == 0 {
			return errors.New("$in value array must not be empty")
		}
		for i, element := range arr {
			switch element.(type) {
			case string, float64, bool:
				// OK.
			default:
				return fmt.Errorf("$in element %d must be string, number, or boolean, got %T", i, element)
			}
		}
		return nil

	default:
		return fmt.Errorf("unknown operator %q", op)
	}
}

// MarshalJSON encodes a MatchValue to JSON. A single eq criterion marshals
// as a bare value for backward compatibility. All other cases marshal as
// a $-prefixed operator object.
func (m MatchValue) MarshalJSON() ([]byte, error) {
	if len(m.criteria) == 0 {
		return nil, errors.New("cannot marshal empty MatchValue")
	}
	if len(m.criteria) == 1 && m.criteria[0].op == "eq" {
		return json.Marshal(m.criteria[0].value)
	}
	obj := make(map[string]any, len(m.criteria))
	for _, c := range m.criteria {
		obj["$"+c.op] = c.value
	}
	return json.Marshal(obj)
}

// --- Validation ---

// Validate checks that the MatchValue has valid operators and value types.
// Call this on values constructed in Go code (JSON-decoded values are
// validated during unmarshal).
func (m MatchValue) Validate() error {
	if len(m.criteria) == 0 {
		return errors.New("match value must have at least one criterion")
	}

	seen := make(map[string]bool, len(m.criteria))
	for _, c := range m.criteria {
		if !validOps[c.op] {
			return fmt.Errorf("unknown operator %q", c.op)
		}
		if seen[c.op] {
			return fmt.Errorf("duplicate operator $%s", c.op)
		}
		seen[c.op] = true

		if err := validateCriterion(c); err != nil {
			return err
		}
	}
	return nil
}

// validateCriterion checks a single operator-value pair for type
// correctness and finite numeric values.
func validateCriterion(c matchCriterion) error {
	switch c.op {
	case "eq":
		switch v := c.value.(type) {
		case string, bool:
			return nil
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("$eq: value must be finite, got %v", v)
			}
			return nil
		default:
			return fmt.Errorf("$eq: value must be string, number, or boolean, got %T", c.value)
		}

	case "lt", "lte", "gt", "gte":
		v, ok := c.value.(float64)
		if !ok {
			return fmt.Errorf("$%s: value must be a number, got %T", c.op, c.value)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("$%s: value must be finite, got %v", c.op, v)
		}
		return nil

	case "in":
		arr, ok := c.value.([]any)
		if !ok {
			return fmt.Errorf("$in: value must be an array, got %T", c.value)
		}
		if len(arr) == 0 {
			return errors.New("$in: value array must not be empty")
		}
		for i, element := range arr {
			switch v := element.(type) {
			case string, bool:
				// OK.
			case float64:
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return fmt.Errorf("$in: element %d must be finite, got %v", i, v)
				}
			default:
				return fmt.Errorf("$in: element %d must be string, number, or boolean, got %T", i, element)
			}
		}
		return nil

	default:
		return fmt.Errorf("unknown operator %q", c.op)
	}
}

// --- Evaluation ---

// Evaluate checks whether the actual value (from a JSON-decoded state
// event field) satisfies all criteria in this MatchValue. Returns:
//   - (true, nil): all criteria matched.
//   - (false, nil): clean mismatch (data doesn't satisfy criteria).
//   - (false, err): malformed expression (should not happen after Validate).
func (m MatchValue) Evaluate(actual any) (bool, error) {
	if len(m.criteria) == 0 {
		return false, errors.New("match value has no criteria")
	}
	for _, c := range m.criteria {
		matched, err := evaluateCriterion(c, actual)
		if err != nil {
			return false, err
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

// evaluateCriterion dispatches to scalar or array evaluation based on
// the actual value's type.
func evaluateCriterion(c matchCriterion, actual any) (bool, error) {
	if arr, ok := actual.([]any); ok {
		return evaluateArray(c, arr)
	}
	return evaluateScalar(c, actual)
}

// evaluateScalar checks a single criterion against a scalar value.
func evaluateScalar(c matchCriterion, actual any) (bool, error) {
	switch c.op {
	case "eq":
		// Type-aware equality: "2" != 2.0, true != 1.0.
		return c.value == actual, nil

	case "lt", "lte", "gt", "gte":
		expected, ok := c.value.(float64)
		if !ok {
			return false, fmt.Errorf("$%s: match value is not a number", c.op)
		}
		actualFloat, ok := actual.(float64)
		if !ok {
			// Target is not numeric — clean mismatch.
			return false, nil
		}
		switch c.op {
		case "lt":
			return actualFloat < expected, nil
		case "lte":
			return actualFloat <= expected, nil
		case "gt":
			return actualFloat > expected, nil
		case "gte":
			return actualFloat >= expected, nil
		}

	case "in":
		arr, ok := c.value.([]any)
		if !ok {
			return false, fmt.Errorf("$in: match value is not an array")
		}
		for _, element := range arr {
			// Type-aware equality per element.
			if element == actual {
				return true, nil
			}
		}
		return false, nil
	}

	return false, fmt.Errorf("unknown operator %q", c.op)
}

// evaluateArray checks a single criterion against an array target. All
// operators evaluate per-element: the criterion matches if any element
// in the array satisfies it. This extends the contract established by
// eq (containment = any element equals).
func evaluateArray(c matchCriterion, actual []any) (bool, error) {
	for _, element := range actual {
		matched, err := evaluateScalar(c, element)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

// --- Display ---

// StringValue returns a human-readable representation of the match criteria.
// For simple equality, returns the bare value ("active", "2", "true").
// For single operators, returns operator notation ("<=2", "in [1, 2, 3]").
// For multiple criteria, joins with " AND " (">=1 AND <=3").
func (m MatchValue) StringValue() string {
	if len(m.criteria) == 0 {
		return ""
	}
	if len(m.criteria) == 1 {
		return formatCriterion(m.criteria[0])
	}
	parts := make([]string, len(m.criteria))
	for i, c := range m.criteria {
		parts[i] = formatCriterion(c)
	}
	return strings.Join(parts, " AND ")
}

// formatCriterion returns a human-readable string for a single criterion.
func formatCriterion(c matchCriterion) string {
	switch c.op {
	case "eq":
		return formatMatchValue(c.value)
	case "lt":
		return "<" + formatMatchValue(c.value)
	case "lte":
		return "<=" + formatMatchValue(c.value)
	case "gt":
		return ">" + formatMatchValue(c.value)
	case "gte":
		return ">=" + formatMatchValue(c.value)
	case "in":
		arr, ok := c.value.([]any)
		if !ok {
			return fmt.Sprintf("in %v", c.value)
		}
		parts := make([]string, len(arr))
		for i, element := range arr {
			parts[i] = formatMatchValue(element)
		}
		return "in [" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("(%s %v)", c.op, c.value)
	}
}

// formatMatchValue formats a single value for display.
func formatMatchValue(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

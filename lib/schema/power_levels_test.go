// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package schema

import "testing"

func TestUserLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		powerLevels PowerLevels
		userID      string
		expected    int
	}{
		{
			name: "explicit user level",
			powerLevels: PowerLevels{
				Users: map[string]int{
					"@alice:test": 100,
					"@bob:test":   50,
				},
			},
			userID:   "@alice:test",
			expected: 100,
		},
		{
			name: "explicit zero level",
			powerLevels: PowerLevels{
				Users: map[string]int{
					"@alice:test": 0,
				},
			},
			userID:   "@alice:test",
			expected: 0,
		},
		{
			name: "falls back to users_default",
			powerLevels: PowerLevels{
				Users:        map[string]int{"@alice:test": 100},
				UsersDefault: intPointer(25),
			},
			userID:   "@unknown:test",
			expected: 25,
		},
		{
			name: "users_default explicitly zero",
			powerLevels: PowerLevels{
				UsersDefault: intPointer(0),
			},
			userID:   "@unknown:test",
			expected: 0,
		},
		{
			name:        "nil users map and nil users_default",
			powerLevels: PowerLevels{},
			userID:      "@unknown:test",
			expected:    0,
		},
		{
			name: "nil users map with users_default",
			powerLevels: PowerLevels{
				UsersDefault: intPointer(10),
			},
			userID:   "@unknown:test",
			expected: 10,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			level := test.powerLevels.UserLevel(test.userID)
			if level != test.expected {
				t.Errorf("UserLevel(%q) = %d, want %d", test.userID, level, test.expected)
			}
		})
	}
}

func intPointer(value int) *int {
	return &value
}

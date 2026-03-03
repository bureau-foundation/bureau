// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"
)

func TestMatchEntityCreation_PullRequest(t *testing.T) {
	body := `{"number": 42, "title": "Add feature", "html_url": "https://github.com/org/repo/pull/42"}`

	match, ok := matchEntityCreation("POST", "/repos/org/repo/pulls", 201, []byte(body))
	if !ok {
		t.Fatal("expected match for PR creation")
	}
	if match.Owner != "org" || match.Repo != "repo" {
		t.Errorf("owner/repo = %s/%s, want org/repo", match.Owner, match.Repo)
	}
	if match.EntityType != "pull_request" {
		t.Errorf("entity_type = %q, want pull_request", match.EntityType)
	}
	if match.Number != 42 {
		t.Errorf("number = %d, want 42", match.Number)
	}
	if match.Title != "Add feature" {
		t.Errorf("title = %q, want 'Add feature'", match.Title)
	}
	if match.URL != "https://github.com/org/repo/pull/42" {
		t.Errorf("url = %q, want 'https://github.com/org/repo/pull/42'", match.URL)
	}
}

func TestMatchEntityCreation_Issue(t *testing.T) {
	body := `{"number": 7, "title": "Bug report", "html_url": "https://github.com/org/repo/issues/7"}`

	match, ok := matchEntityCreation("POST", "/repos/org/repo/issues", 201, []byte(body))
	if !ok {
		t.Fatal("expected match for issue creation")
	}
	if match.EntityType != "issue" {
		t.Errorf("entity_type = %q, want issue", match.EntityType)
	}
	if match.Number != 7 {
		t.Errorf("number = %d, want 7", match.Number)
	}
}

func TestMatchEntityCreation_Commit(t *testing.T) {
	body := `{"sha": "abc123def456789", "message": "Initial commit\n\nMore details here"}`

	match, ok := matchEntityCreation("POST", "/repos/org/repo/git/commits", 201, []byte(body))
	if !ok {
		t.Fatal("expected match for commit creation")
	}
	if match.EntityType != "commit" {
		t.Errorf("entity_type = %q, want commit", match.EntityType)
	}
	if match.SHA != "abc123def456789" {
		t.Errorf("sha = %q, want abc123def456789", match.SHA)
	}
	if match.Title != "Initial commit" {
		t.Errorf("title = %q, want 'Initial commit' (first line of message)", match.Title)
	}
}

func TestMatchEntityCreation_WrongMethod(t *testing.T) {
	body := `{"number": 1}`

	_, ok := matchEntityCreation("GET", "/repos/org/repo/pulls", 200, []byte(body))
	if ok {
		t.Fatal("GET should not match entity creation")
	}
}

func TestMatchEntityCreation_NonSuccessStatus(t *testing.T) {
	body := `{"number": 1}`

	_, ok := matchEntityCreation("POST", "/repos/org/repo/pulls", 422, []byte(body))
	if ok {
		t.Fatal("422 status should not match")
	}
}

func TestMatchEntityCreation_UnrelatedPath(t *testing.T) {
	body := `{"id": 123}`

	_, ok := matchEntityCreation("POST", "/repos/org/repo/comments", 201, []byte(body))
	if ok {
		t.Fatal("comments path should not match")
	}
}

func TestMatchEntityCreation_InvalidJSON(t *testing.T) {
	_, ok := matchEntityCreation("POST", "/repos/org/repo/pulls", 201, []byte("not json"))
	if ok {
		t.Fatal("invalid JSON should not match")
	}
}

func TestMatchEntityCreation_MissingNumberField(t *testing.T) {
	body := `{"title": "No number field"}`

	_, ok := matchEntityCreation("POST", "/repos/org/repo/pulls", 201, []byte(body))
	if ok {
		t.Fatal("missing number field should not match")
	}
}

func TestMatchEntityCreation_NestedOwnerRepo(t *testing.T) {
	body := `{"number": 1, "title": "Test"}`

	match, ok := matchEntityCreation("POST", "/repos/my-org/my-repo/pulls", 201, []byte(body))
	if !ok {
		t.Fatal("expected match for hyphenated owner/repo")
	}
	if match.Owner != "my-org" || match.Repo != "my-repo" {
		t.Errorf("owner/repo = %s/%s, want my-org/my-repo", match.Owner, match.Repo)
	}
}

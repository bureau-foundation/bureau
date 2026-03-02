// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package forgesub

import (
	"strings"
	"testing"
)

func TestExtractMentions_Simple(t *testing.T) {
	mentions := ExtractMentions("@alice please review this")
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_Multiple(t *testing.T) {
	mentions := ExtractMentions("@alice and @bob should look at this, cc @carol")
	assertMentions(t, mentions, []string{"alice", "bob", "carol"})
}

func TestExtractMentions_Deduplicated(t *testing.T) {
	mentions := ExtractMentions("@alice please review. @alice ping?")
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_CaseInsensitive(t *testing.T) {
	mentions := ExtractMentions("@Alice and @ALICE and @alice")
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_Hyphens(t *testing.T) {
	mentions := ExtractMentions("@reviewer-bot take a look")
	assertMentions(t, mentions, []string{"reviewer-bot"})
}

func TestExtractMentions_NoTrailingHyphen(t *testing.T) {
	// GitHub/Forgejo usernames can't end with a hyphen. The regex
	// should match the valid prefix and stop before the trailing
	// hyphen. "user-" should match "user" if followed by non-alnum,
	// or not match at all depending on context.
	mentions := ExtractMentions("@valid-user works, @trailing- does not end well")
	// "trailing-" — the regex requires the last char to be alphanumeric,
	// so it matches "trailing" (the single-char username path handles
	// this: [a-zA-Z0-9] alone is valid).
	assertContains(t, mentions, "valid-user")
	// "trailing" is extracted (the hyphen is not part of the username).
	assertContains(t, mentions, "trailing")
}

func TestExtractMentions_EmailExcluded(t *testing.T) {
	mentions := ExtractMentions("Contact user@example.com for help")
	if len(mentions) != 0 {
		t.Fatalf("expected no mentions from email, got %v", mentions)
	}
}

func TestExtractMentions_EmailInSentence(t *testing.T) {
	mentions := ExtractMentions("Email foo@bar.com or ask @alice")
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_FencedCodeBlockExcluded(t *testing.T) {
	text := "Look at this:\n```python\n@decorator\ndef foo():\n    pass\n```\nBut @alice should review"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_FencedCodeBlockWithLanguage(t *testing.T) {
	text := "```go\n// @generated\nfunc main() {}\n```\n@bob fix this"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"bob"})
}

func TestExtractMentions_InlineCodeExcluded(t *testing.T) {
	text := "Use `@Override` annotation. @alice should know"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_DoubleBacktickInlineCode(t *testing.T) {
	text := "The ``@special`` decorator. Ask @carol"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"carol"})
}

func TestExtractMentions_StartOfLine(t *testing.T) {
	text := "@alice at start of line"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_AfterParenthesis(t *testing.T) {
	text := "Assigned to (@alice) for review"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"alice"})
}

func TestExtractMentions_AfterComma(t *testing.T) {
	text := "cc @alice, @bob"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"alice", "bob"})
}

func TestExtractMentions_EmptyString(t *testing.T) {
	mentions := ExtractMentions("")
	if mentions != nil {
		t.Fatalf("expected nil for empty string, got %v", mentions)
	}
}

func TestExtractMentions_NoMentions(t *testing.T) {
	mentions := ExtractMentions("Nothing to see here")
	if mentions != nil {
		t.Fatalf("expected nil for no mentions, got %v", mentions)
	}
}

func TestExtractMentions_MaxUsernameLength(t *testing.T) {
	// 39 characters is the max GitHub/Forgejo username length.
	long := strings.Repeat("a", 39)
	text := "@" + long + " is valid"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{long})
}

func TestExtractMentions_TooLongUsername(t *testing.T) {
	// 40 characters exceeds the limit — the regex should only
	// capture up to 39.
	long := strings.Repeat("a", 40)
	text := "@" + long
	mentions := ExtractMentions(text)
	// Should capture the first 39 characters.
	if len(mentions) != 1 || len(mentions[0]) != 39 {
		t.Fatalf("expected 39-char capture, got %v", mentions)
	}
}

func TestExtractMentions_MultipleFencedBlocks(t *testing.T) {
	text := "@real1\n```\n@fake\n```\n@real2\n```\n@fake2\n```\n@real3"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"real1", "real2", "real3"})
}

func TestExtractMentions_UnclosedFencedBlock(t *testing.T) {
	// Unclosed fenced block — everything after ``` is treated as
	// code and excluded.
	text := "@before\n```\n@inside code"
	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"before"})
}

func TestExtractMentions_UnclosedInlineCode(t *testing.T) {
	// Unclosed backtick is treated as literal text, not code.
	text := "The `problem and @alice should fix it"
	mentions := ExtractMentions(text)
	assertContains(t, mentions, "alice")
}

func TestExtractMentions_MixedContent(t *testing.T) {
	text := `@reviewer-bot please look at this PR.

The change updates the ` + "`@deprecated`" + ` annotation handling.

` + "```go" + `
// @generated by protoc-gen-go
func init() {}
` + "```" + `

Also cc @team-lead for visibility. Email support@example.com if questions.`

	mentions := ExtractMentions(text)
	assertMentions(t, mentions, []string{"reviewer-bot", "team-lead"})
}

func TestExtractMentions_NumericUsername(t *testing.T) {
	mentions := ExtractMentions("@42 is a valid username")
	assertMentions(t, mentions, []string{"42"})
}

func TestExtractMentions_SingleCharUsername(t *testing.T) {
	mentions := ExtractMentions("@x please review")
	assertMentions(t, mentions, []string{"x"})
}

// --- Test helpers ---

func assertMentions(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d mentions %v, want %d mentions %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("mention[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func assertContains(t *testing.T, mentions []string, username string) {
	t.Helper()
	for _, mention := range mentions {
		if mention == username {
			return
		}
	}
	t.Errorf("mentions %v does not contain %q", mentions, username)
}

package main

import (
	"strings"
	"testing"
)

// TestSkillTextCoversJSONContract guards --skill against drifting from
// the JSON objects the code actually emits: every "type" value and
// contract-bearing field name must be documented. If this fails, the
// output code and skill.go changed out of step.
func TestSkillTextCoversJSONContract(t *testing.T) {
	// One token per emitted JSON "type", plus field names that agents
	// key on. Distinctive spellings only — no generic words.
	required := []string{
		`"span"`,
		`"region"`,
		"suggest_summary",
		"exemplar",
		"omitted_lines",
		"shown_lines",
		`"errors"`,
		"--get-region",
		"--expand",
		"--use-index",
		"--max-tokens",
		"--outline",
		"--rank",
		"--batch",
		"--suggest",
		"--sections",
		"--scope",
		"--ident",
		"--histogram",
		"--collapse",
		"--changed-since",
		"--files-from",
		"--with-file",
		"--without-file",
		"-U",
		"@:120-160",
		"--structural",
		":[name]",
		"--lang",
		"--capture",
		"--block",
		`"captures"`,
		`"truncated"`,
	}
	for _, tok := range required {
		if !strings.Contains(skillText, tok) {
			t.Errorf("skillText no longer documents %s", tok)
		}
	}

	// The contract section must name every discriminator type.
	for _, typ := range []string{"match", "count", "file", "outline", "variant", "suggest", "suggest_summary", "collapsed", "error", "summary"} {
		if !strings.Contains(skillText, "\n  "+typ) {
			t.Errorf("skillText JSON contract missing type %q", typ)
		}
	}
}

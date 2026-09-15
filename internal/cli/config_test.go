package cli

import "testing"

func TestWordPattern(t *testing.T) {
	if got := WordPattern("a.b", true); got != `\ba\.b\b` {
		t.Errorf("fixed: %q", got)
	}
	if got := WordPattern("err(or|no)", false); got != `\b(?:err(or|no))\b` {
		t.Errorf("regex: %q", got)
	}
}

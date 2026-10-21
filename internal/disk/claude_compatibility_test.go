package disk

import (
	"strings"
	"testing"
)

func TestClaudeCompatibilityManifestFailsClosed(t *testing.T) {
	valid := `{"schema":1,"adapter":"claude-local-handoff-v1","series":"2.1","versions":["2.1.305"]}`
	if !testedClaudeManifest([]byte(valid), "2.1.305 (Claude Code)") {
		t.Fatal("reviewed exact version rejected")
	}
	for _, raw := range []string{
		"", valid + `{}`, strings.Replace(valid, `"schema":1`, `"schema":2`, 1),
		strings.Replace(valid, `"series":"2.1"`, `"series":"2.2"`, 1),
		strings.Replace(valid, `"versions":["2.1.305"]`, `"versions":["2.1.305","2.1.305"]`, 1),
		strings.Replace(valid, `"versions":["2.1.305"]`, `"versions":[]`, 1),
		strings.Replace(valid, `"versions":["2.1.305"]`, `"versions":["2.1.305","2.2.0"]`, 1),
		strings.Replace(valid, `"schema":1`, `"unknown":true,"schema":1`, 1),
		strings.Repeat(" ", 64<<10) + valid,
	} {
		if testedClaudeManifest([]byte(raw), "2.1.305 (Claude Code)") {
			t.Fatal("invalid manifest accepted")
		}
	}
}

func TestClaudeUpdateWarningIsActionableAndSanitized(t *testing.T) {
	err := claudeCompatibilityError("2.1.306 (Claude Code)")
	for _, required := range []string{"2.1.306", "update Infinite Disk", "compatibility CI", "retained"} {
		if !strings.Contains(err.Error(), required) {
			t.Fatal("missing recovery guidance", required)
		}
	}
	if strings.Contains(claudeCompatibilityError("unexpected SECRET_VALUE").Error(), "SECRET_VALUE") {
		t.Fatal("raw native output leaked")
	}
	if !strings.Contains(claudeCompatibilityError("2.2.0 (Claude Code)").Error(), "2.2.0") {
		t.Fatal("new series lacks actionable observed version")
	}
}

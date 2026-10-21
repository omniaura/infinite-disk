package disk

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// This reviewed manifest is embedded in releases; runtime never downloads or
// trusts a mutable remote allowlist. CI must prove each new version's contract.
//
//go:embed claude_compatibility.json
var claudeCompatibilityManifest []byte

var claudeCompatVersion = regexp.MustCompile(`^2\.1\.(0|[1-9][0-9]{0,8})$`)
var claudeVersionOutput = regexp.MustCompile(`^((0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})) \(Claude Code\)$`)

func testedClaudeVersion(version string) bool {
	return testedClaudeManifest(claudeCompatibilityManifest, version)
}

func testedClaudeManifest(raw []byte, output string) bool {
	var manifest struct {
		Schema   int      `json:"schema"`
		Adapter  string   `json:"adapter"`
		Series   string   `json:"series"`
		Versions []string `json:"versions"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 64<<10 || d.Decode(&manifest) != nil || d.Decode(new(any)) != io.EOF || manifest.Schema != 1 || manifest.Adapter != "claude-local-handoff-v1" || manifest.Series != "2.1" || len(manifest.Versions) == 0 || len(manifest.Versions) > 4096 {
		return false
	}
	seen, found := map[string]bool{}, false
	for _, version := range manifest.Versions {
		if !claudeCompatVersion.MatchString(version) || seen[version] {
			return false
		}
		seen[version] = true
		found = found || output == version+" (Claude Code)"
	}
	return found
}

type untestedClaudeVersion struct{ version string }

func (e *untestedClaudeVersion) Error() string {
	return "Claude Code " + e.version + " is not yet verified for session handoff; update Infinite Disk or request Claude compatibility CI. Checkouts and sessions are retained"
}

func claudeCompatibilityError(output string) error {
	version := strings.TrimSuffix(output, " (Claude Code)")
	if !claudeVersionOutput.MatchString(output) || version == output {
		return &untestedClaudeVersion{version: "(unrecognized version)"}
	}
	return &untestedClaudeVersion{version: version}
}

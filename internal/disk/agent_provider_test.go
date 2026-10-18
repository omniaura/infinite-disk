package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dittoFixtureConfig(t *testing.T) Config {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.AgentProvider, c.AgentModel, c.AgentBaseURL = "ditto", "gm/gpt-6-luna", "https://router.heyditto.ai/v1"
	c.AgentKeyFile = filepath.Join(dir, "inference-key")
	if err := os.WriteFile(c.AgentKeyFile, []byte("ditto_inf_fixture_key_1234567890"), 0600); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDittoCredentialsAreExplicitAndIsolated(t *testing.T) {
	c := dittoFixtureConfig(t)
	t.Setenv("DITTO_API_KEY", "management-secret")
	t.Setenv("GM_API_KEY", "provider-secret")
	t.Setenv("OPENROUTER_API_KEY", "unselected-secret")
	t.Setenv("NODE_OPTIONS", "untrusted-option")
	env, err := AgentEnvironment(c)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"management-secret", "provider-secret", "unselected-secret", "NODE_OPTIONS"} {
		if strings.Contains(joined, forbidden) {
			t.Fatal("unselected credential crossed worker boundary")
		}
	}
	if !strings.Contains(joined, "DITTO_INFERENCE_API_KEY=ditto_inf_") {
		t.Fatal("inference credential missing")
	}
	if c.AgentEnabled {
		t.Fatal("paid monitor enabled")
	}
}

func TestDittoCredentialUnsafeFilesFailClosed(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink", "ancestor symlink", "hardlink", "management key", "provider key", "oversize", "missing"} {
		t.Run(kind, func(t *testing.T) {
			c := dittoFixtureConfig(t)
			switch kind {
			case "permissions":
				_ = os.Chmod(c.AgentKeyFile, 0640)
			case "symlink":
				link := c.AgentKeyFile + "-link"
				if err := os.Symlink(c.AgentKeyFile, link); err != nil {
					t.Fatal(err)
				}
				c.AgentKeyFile = link
			case "ancestor symlink":
				link := filepath.Join(filepath.Dir(c.AgentKeyFile), "link")
				if err := os.Symlink(filepath.Dir(c.AgentKeyFile), link); err != nil {
					t.Fatal(err)
				}
				c.AgentKeyFile = filepath.Join(link, "inference-key")
			case "hardlink":
				if err := os.Link(c.AgentKeyFile, c.AgentKeyFile+"-hardlink"); err != nil {
					t.Fatal(err)
				}
			case "management key":
				_ = os.WriteFile(c.AgentKeyFile, []byte("ditto_mcp_fixture_1234567890123456"), 0600)
			case "provider key":
				_ = os.WriteFile(c.AgentKeyFile, []byte("sk-provider-fixture"), 0600)
			case "oversize":
				_ = os.WriteFile(c.AgentKeyFile, []byte(strings.Repeat("x", 5000)), 0600)
			case "missing":
				c.AgentKeyFile += "-missing"
			}
			if _, err := AgentEnvironment(c); err == nil {
				t.Fatal("unsafe key accepted")
			}
		})
	}
}

func TestDittoURLsCannotRedirectCredentials(t *testing.T) {
	for _, url := range []string{"http://router.heyditto.ai/v1", "https://attacker.example/v1", "https://router.heyditto.ai.evil/v1", "https://key@router.heyditto.ai/v1", "https://router.heyditto.ai/v1?key=x", "https://router.heyditto.ai/v1/", "http://localhost/v1"} {
		c := dittoFixtureConfig(t)
		c.AgentBaseURL = url
		if c.ValidateAgent() == nil {
			t.Fatalf("unsafe URL accepted: %s", url)
		}
	}
	c := DefaultConfig()
	if c.ValidateAgent() != nil {
		t.Fatal("legacy OpenRouter configuration broken")
	}
}

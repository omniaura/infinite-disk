package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniaura/infinite-disk/internal/disk"
)

func TestAgentConfigureUsesAReferenceAndDoesNotEnableMonitor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := disk.DefaultConfig()
	c.StateDir, c.Roots = filepath.Join(base, "state"), []string{filepath.Join(base, "projects")}
	file, config := filepath.Join(base, "inference-key"), filepath.Join(base, "config.json")
	key := "ditto_inf_fixture_key_1234567890"
	if err := os.WriteFile(file, []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	if err := disk.SaveConfig(config, c); err != nil {
		t.Fatal(err)
	}
	cmd := New("test")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--config", config, "--json", "agent", "configure", "--provider", "ditto", "--model", "gm/gpt-6-luna", "--base-url", "https://router.heyditto.ai/v1", "--key-file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if json.Unmarshal(out.Bytes(), &value) != nil || value["scheduled_paid_inference"] != false {
		t.Fatal("invalid configuration receipt")
	}
	saved, err := disk.LoadConfig(config)
	if err != nil || saved.AgentEnabled || saved.AgentProvider != "ditto" || saved.AgentKeyFile != file {
		t.Fatal("configuration not persisted safely", err)
	}
	raw, err := os.ReadFile(config)
	if err != nil || strings.Contains(string(raw)+out.String(), key) {
		t.Fatal("credential leaked")
	}
}

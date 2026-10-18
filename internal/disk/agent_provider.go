package disk

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var agentModelID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9/_.:-]{0,159}$`)
var inferenceKey = regexp.MustCompile(`^ditto_inf_[a-zA-Z0-9_-]{16,}$`)

func (c Config) ValidateAgent() error {
	if !agentModelID.MatchString(c.AgentModel) {
		return errors.New("agent_model must be an explicit model ID")
	}
	switch c.AgentProvider {
	case "", "openrouter":
		if c.AgentBaseURL != "" || c.AgentKeyFile != "" {
			return errors.New("local key files and base URLs require agent_provider ditto")
		}
	case "ditto":
		// Fixed first-party hosts only: configuration cannot redirect a stored
		// inference key to an arbitrary URL or a local proxy. No credential URLs.
		if c.AgentBaseURL != "https://router.heyditto.ai/v1" && c.AgentBaseURL != "https://inference.heyditto.ai/v1" {
			return errors.New("Ditto inference requires an official HTTPS Router base URL ending in /v1")
		}
		if !filepath.IsAbs(c.AgentKeyFile) || filepath.Clean(c.AgentKeyFile) != c.AgentKeyFile {
			return errors.New("agent_key_file must be a normalized absolute private file path")
		}
	default:
		return errors.New("agent_provider must be openrouter or ditto")
	}
	return nil
}

// AgentEnvironment never inherits management keys, unrelated provider keys or
// arbitrary Node options. Only the selected provider's credential crosses the
// private worker boundary. Credentials never enter config, plans or logs.
func AgentEnvironment(c Config) ([]string, error) {
	if err := c.ValidateAgent(); err != nil {
		return nil, err
	}
	env := []string{}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	if c.AgentProvider == "ditto" {
		key, err := readInferenceKey(c.AgentKeyFile)
		if err != nil {
			return nil, err
		}
		return append(env, "DITTO_INFERENCE_API_KEY="+key), nil
	}
	if value := os.Getenv("OPENROUTER_API_KEY"); value != "" {
		env = append(env, "OPENROUTER_API_KEY="+value)
	}
	return env, nil
}

func readInferenceKey(path string) (string, error) {
	bad := errors.New("cannot load Ditto inference key: require a non-symlink, owner-only regular file containing an endpoint key, not a management or provider key")
	if err := noSymlinkAncestors(filepath.Dir(path)); err != nil {
		return "", bad
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 4096 {
		return "", bad
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || st.Nlink != 1 {
		return "", bad
	}
	f, err := os.Open(path)
	if err != nil {
		return "", bad
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm()&0077 != 0 {
		return "", bad
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", bad
	}
	key := strings.TrimSpace(string(b))
	if !inferenceKey.MatchString(key) {
		return "", bad
	}
	return key, nil
}

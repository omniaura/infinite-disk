package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	state := filepath.Join(home, ".local", "share", "infinite-disk")
	return Config{Version: ProtocolVersion, StateDir: state, IdleDays: 7, MinFreeGiB: 100,
		MinFreePercent: 15, PollSeconds: 60, AuditMinutes: 60, PeriodicHours: 24,
		MaxProjects: 500, MaxDepth: 7, AgentModel: "openai/gpt-4.1-mini",
		AllowedRemoteHosts: []string{"github.com"}, Roots: []string{
			filepath.Join(home, "code"), filepath.Join(home, "projects"),
			filepath.Join(home, ".codex", "worktrees"), filepath.Join(home, ".cursor", "worktrees"),
			filepath.Join(home, ".t3", "worktrees"),
			filepath.Join(home, ".grok", "worktrees"), filepath.Join(home, ".grokbot", "worktrees"),
			filepath.Join(home, "emdash", "worktrees"), filepath.Join(home, ".emdash", "worktrees"),
		}}
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "infinite-disk", "config.json")
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, c.Validate()
	}
	if err != nil {
		return c, errors.New("cannot read configuration")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid configuration")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("trailing configuration data")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if err := c.ValidateAgent(); err != nil {
		return err
	}
	if c.Version != ProtocolVersion || c.IdleDays < 1 || c.IdleDays > 36500 || c.PollSeconds < 30 || c.PollSeconds > 86400 || c.AuditMinutes < 1 || c.AuditMinutes > 525600 || c.PeriodicHours < 1 || c.PeriodicHours > 8760 || c.MaxProjects < 1 || c.MaxProjects > 5000 || c.MaxDepth < 1 || c.MaxDepth > 12 || c.MinFreePercent < 0 || c.MinFreePercent > 100 || c.MinFreeGiB > 1000000 {
		return errors.New("invalid configuration limits")
	}
	home, _ := os.UserHomeDir()
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || within(c.StateDir, home) || len(strings.Split(c.StateDir, string(filepath.Separator))) < 4 {
		return errors.New("state directory must be an absolute private path")
	}
	for _, root := range c.Roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) || filepath.Clean(root) == home {
			return errors.New("roots must be explicit project-parent directories, never home or filesystem root")
		}
		if within(root, c.StateDir) || within(c.StateDir, root) {
			return errors.New("state directory and discovery roots must not overlap")
		}
	}
	return nil
}

func within(parent, child string) bool {
	r, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

func ConfigDigest(c Config) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func atomicPrivate(path string, data []byte) error {
	if err := noSymlinkAncestors(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("refusing non-regular state file")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func noSymlinkAncestors(path string) error {
	for p := filepath.Clean(path); p != filepath.Dir(p); p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink ancestor is not allowed")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func SaveConfig(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivate(path, append(b, '\n'))
}

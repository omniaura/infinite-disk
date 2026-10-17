package disk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, errors.New("command output exceeded safety limit")
	}
	return b.Buffer.Write(p)
}

func gitCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	argv := append([]string{"--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "diff.autoRefreshIndex=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	cmd.Env = append(cmd.Env, env...)
	buf := &cappedBuffer{limit: 16 << 20}
	cmd.Stdout = buf
	if err := cmd.Run(); err != nil {
		return nil, errors.New("Git operation failed; no raw command output was logged")
	}
	return buf.Bytes(), nil
}

// Keep normal OS/Git authentication, but do not give repository processes model
// credentials or ambient Git overrides that could redirect inspection/writes.
func gitEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_") || strings.HasSuffix(key, "API_KEY") || strings.HasSuffix(key, "API_TOKEN") {
			continue
		}
		env = append(env, item)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
}

func gitText(ctx context.Context, dir string, args ...string) (string, error) {
	b, e := gitCommand(ctx, dir, nil, args...)
	return strings.TrimSpace(string(b)), e
}

var sensitiveName = regexp.MustCompile(`(?i)(^|/)(\.env($|\.)|credentials?($|\.)|id_(rsa|ed25519)($|\.)|.*\.(pem|p12|pfx|key|keystore)$|auth\.json$|secrets?($|\.)|.*\.sqlite(-wal|-shm)?$)`)
var secretValue = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----|sk-or-v1-[A-Za-z0-9]{20,}|(?:ghp_|github_pat_|xox[baprs]-)[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|(?i:(?:api[_-]?key|access[_-]?token|password|client[_-]?secret)\s*[:=]\s*["']?)[A-Za-z0-9_./+\-]{24,}`)

func digestFile(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeRelative(path string) bool {
	return path != "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator)) && !strings.ContainsRune(path, 0)
}

func changedPaths(status []byte) ([]string, error) {
	var paths []string
	for _, record := range bytes.Split(status, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(record) < 4 {
			return nil, errors.New("invalid Git status")
		}
		if record[0] == 'R' || record[1] == 'R' || record[0] == 'C' || record[1] == 'C' {
			return nil, errors.New("rename/copy index state requires manual review")
		}
		p := string(record[3:])
		if !safeRelative(p) {
			return nil, errors.New("unsafe Git path")
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func remoteHost(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n\x00") || strings.HasPrefix(raw, "-") {
		return "", errors.New("unsafe remote")
	}
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(parts[0], "/@ ") {
			return strings.ToLower(parts[0]), nil
		}
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Scheme != "https" {
		return "", errors.New("remote must be credential-free HTTPS or git@host SSH")
	}
	return strings.ToLower(u.Hostname()), nil
}

func allowedIgnored(path string) bool {
	path = strings.TrimSuffix(filepath.ToSlash(path), "/")
	for _, p := range strings.Split(path, "/") {
		if p == "node_modules" || p == ".venv" || p == "__pycache__" {
			return true
		}
	}
	return false
}

func fingerprint(ctx context.Context, path string) (string, []string, int, time.Time, error) {
	head, e := gitText(ctx, path, "rev-parse", "HEAD")
	if e != nil {
		return "", nil, 0, time.Time{}, e
	}
	status, e := gitCommand(ctx, path, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if e != nil {
		return "", nil, 0, time.Time{}, e
	}
	changed, e := changedPaths(status)
	if e != nil {
		return "", nil, 0, time.Time{}, e
	}
	h := sha256.New()
	h.Write([]byte(head))
	h.Write(status)
	refs, err := gitCommand(ctx, path, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return "", nil, 0, time.Time{}, err
	}
	for _, line := range bytes.Split(refs, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("refs/infinite-disk/recovery/")) || bytes.HasPrefix(line, []byte("refs/remotes/origin/infinite-disk/recovery/")) {
			continue
		}
		h.Write(line)
		h.Write([]byte{0})
	}
	localConfig, err := gitCommand(ctx, path, nil, "config", "--local", "--null", "--list")
	if err != nil {
		return "", nil, 0, time.Time{}, err
	}
	h.Write(localConfig)
	diff, e := gitCommand(ctx, path, nil, "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD", "--")
	if e != nil {
		return "", nil, 0, time.Time{}, e
	}
	h.Write(diff)
	index, e := gitText(ctx, path, "rev-parse", "--git-path", "index")
	if e != nil {
		return "", nil, 0, time.Time{}, e
	}
	if !filepath.IsAbs(index) {
		index = filepath.Join(path, index)
	}
	if ih, err := digestFile(index); err == nil {
		h.Write([]byte(ih))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, 0, time.Time{}, err
	}
	files, e := gitCommand(ctx, path, nil, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if e != nil {
		return "", nil, 0, time.Time{}, e
	}
	var newest time.Time
	for _, raw := range bytes.Split(files, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		relative := string(raw)
		if !safeRelative(relative) {
			return "", nil, 0, newest, errors.New("unsafe indexed path")
		}
		info, err := os.Lstat(filepath.Join(path, relative))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, 0, newest, err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	for _, relative := range changed {
		full := filepath.Join(path, relative)
		if err := noSymlinkAncestors(filepath.Dir(full)); err != nil {
			return "", nil, 0, newest, err
		}
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			h.Write([]byte("deleted:" + relative))
			continue
		}
		if err != nil {
			return "", nil, 0, newest, err
		}
		h.Write([]byte(relative))
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(full)
			if err != nil {
				return "", nil, 0, newest, err
			}
			h.Write([]byte(link))
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > 16<<20 {
			return "", nil, 0, newest, errors.New("changed special or large file requires manual preservation")
		}
		b, err := os.ReadFile(full)
		if err != nil {
			return "", nil, 0, newest, err
		}
		h.Write(b)
		if sensitiveName.MatchString(filepath.ToSlash(relative)) || secretValue.Match(b) {
			return "", nil, 0, newest, errors.New("sensitive changed file requires private backup")
		}
	}
	return hex.EncodeToString(h.Sum(nil)), changed, len(changed), newest, nil
}

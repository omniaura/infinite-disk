package disk

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const ServiceName = "dev.omniaura.infinite-disk"
const serviceMarker = "Managed by Infinite Disk; report-only"

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func unitQuote(s string) string {
	return strconv.Quote(strings.NewReplacer("%", "%%", "$", "$$").Replace(s))
}
func cronQuote(s string) string { return strings.ReplaceAll(shellQuote(s), "%", "\\%") }

func ServiceContent(platform, binary, config, state string) (string, error) {
	for _, p := range []string{binary, config, state} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\n\r") {
			return "", errors.New("service paths must be absolute and contain no control characters")
		}
	}
	args := []string{binary, "--config", config, "watch"}
	switch platform {
	case "darwin":
		var a strings.Builder
		for _, arg := range args {
			fmt.Fprintf(&a, "<string>%s</string>", xmlText(arg))
		}
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s -->
<plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array>%s</array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>60</integer><key>ProcessType</key><string>Background</string><key>LowPriorityIO</key><true/><key>Nice</key><integer>10</integer><key>StandardOutPath</key><string>/dev/null</string><key>StandardErrorPath</key><string>/dev/null</string></dict></plist>
`, serviceMarker, ServiceName, a.String()), nil
	case "linux":
		q := []string{}
		for _, arg := range args {
			q = append(q, unitQuote(arg))
		}
		return fmt.Sprintf("# %s\n[Unit]\nDescription=Infinite Disk report-only disk watcher\n[Service]\nType=simple\nExecStart=%s\nRestart=on-failure\nRestartSec=60\nNice=10\nUMask=0077\nNoNewPrivileges=yes\nPrivateTmp=yes\n[Install]\nWantedBy=default.target\n", serviceMarker, strings.Join(q, " ")), nil
	case "cron":
		return fmt.Sprintf("# BEGIN %s\n*/5 * * * * %s --config %s watch --once >/dev/null 2>&1\n# END %s\n", serviceMarker, cronQuote(binary), cronQuote(config), serviceMarker), nil
	default:
		return "", errors.New("native monitoring supports macOS launchd, Linux systemd, or explicit cron")
	}
}

func serviceFile(platform string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch platform {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", ServiceName+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "infinite-disk.service"), nil
	}
	return "", errors.New("not a file-backed service")
}

func runServiceCommand(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Run(); err != nil {
		return errors.New("native service operation failed; inspect service status")
	}
	return nil
}

func stopOwnedService(ctx context.Context, platform, file string) error {
	if platform == "linux" {
		return runServiceCommand(ctx, "systemctl", "--user", "disable", "--now", "infinite-disk.service")
	}
	if platform != "darwin" {
		return errors.New("unsupported service platform")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + ServiceName
	check := exec.CommandContext(ctx, "launchctl", "print", target)
	stderr := &cappedBuffer{limit: 64 << 10}
	check.Stderr = stderr
	if err := check.Run(); err != nil {
		if ctx.Err() == nil && !stderr.overflow && strings.Contains(stderr.String(), "Could not find service") {
			return nil
		}
		return errors.New("cannot verify native service state; owned service file retained")
	}
	return runServiceCommand(ctx, "launchctl", "bootout", target)
}

func copyExecutable(source, destination string) error {
	if info, err := os.Lstat(destination); err == nil && !info.Mode().IsRegular() {
		return errors.New("unsafe installed binary path")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(destination), ".binary-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err = io.Copy(out, in); err == nil {
		err = out.Chmod(0700)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), destination)
}

func InstallService(ctx context.Context, c Config, configPath, platform string, start bool) (string, error) {
	if platform == "auto" || platform == "" {
		platform = runtime.GOOS
	}
	if err := EnsureState(c); err != nil {
		return "", err
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	if _, err = os.Stat(configPath); err != nil {
		return "", errors.New("initialize a saved config before installing the service")
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	installed := filepath.Join(c.StateDir, "bin", "infinite-disk")
	if err = copyExecutable(self, installed); err != nil {
		return "", err
	}
	content, err := ServiceContent(platform, installed, configPath, c.StateDir)
	if err != nil {
		return "", err
	}
	if platform == "cron" {
		old, readErr := readCrontab(ctx)
		if readErr != nil {
			return "", readErr
		}
		text := string(old)
		if strings.Contains(text, "# BEGIN "+serviceMarker) {
			return "", errors.New("Infinite Disk cron entry already exists")
		}
		cmd := exec.CommandContext(ctx, "crontab", "-")
		cmd.Stdin = strings.NewReader(strings.TrimRight(text, "\n") + "\n" + content)
		if err = cmd.Run(); err != nil {
			return "", errors.New("could not install managed cron entry")
		}
		return "crontab", nil
	}
	file, err := serviceFile(platform)
	if err != nil {
		return "", err
	}
	if previous, err := os.ReadFile(file); err == nil && !strings.Contains(string(previous), serviceMarker) {
		return "", errors.New("refusing to overwrite an unrelated service")
	}
	if err = atomicPrivate(file, []byte(content)); err != nil {
		return "", err
	}
	if start {
		switch platform {
		case "darwin":
			_ = runServiceCommand(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid()), file)
			err = runServiceCommand(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), file)
		case "linux":
			if err = runServiceCommand(ctx, "systemctl", "--user", "daemon-reload"); err == nil {
				err = runServiceCommand(ctx, "systemctl", "--user", "enable", "--now", "infinite-disk.service")
			}
		}
	}
	return file, err
}

func readCrontab(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "crontab", "-l")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err == nil {
		return b, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.Contains(strings.ToLower(stderr.String()), "no crontab for") {
		return nil, nil
	}
	return nil, errors.New("could not read crontab; existing entries were not changed")
}

func UninstallService(ctx context.Context, platform string) error {
	if platform == "auto" || platform == "" {
		platform = runtime.GOOS
	}
	if platform == "cron" {
		old, err := exec.CommandContext(ctx, "crontab", "-l").Output()
		if err != nil {
			return errors.New("could not read crontab")
		}
		text := string(old)
		start := strings.Index(text, "# BEGIN "+serviceMarker)
		end := strings.Index(text, "# END "+serviceMarker)
		if start < 0 || end < start {
			return errors.New("managed cron block not found")
		}
		end += len("# END " + serviceMarker)
		text = text[:start] + strings.TrimPrefix(text[end:], "\n")
		cmd := exec.CommandContext(ctx, "crontab", "-")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	file, err := serviceFile(platform)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), serviceMarker) {
		return errors.New("refusing to remove an unrelated service")
	}
	switch platform {
	case "darwin":
		err = stopOwnedService(ctx, platform, file)
	case "linux":
		err = stopOwnedService(ctx, platform, file)
	}
	if err != nil {
		return err
	}
	if err = os.Remove(file); err != nil {
		return err
	}
	if platform == "linux" {
		return runServiceCommand(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}

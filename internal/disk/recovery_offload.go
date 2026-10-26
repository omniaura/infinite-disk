package disk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Recovery offload backs up infinite-disk's retained recovery data (the
// restore-tested thin bundles and session backups) to Ditto through the
// heyditto CLI's archive capsules. Local copies are kept: the archive is the
// off-device copy the README asks for, not a reason to delete.

// OffloadSource is one directory handed to `heyditto teleport archive`.
type OffloadSource struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// OffloadResult is heyditto's JSON report plus what was sent.
type OffloadResult struct {
	Version int             `json:"version"`
	Capsule string          `json:"capsule_name"`
	Sources []OffloadSource `json:"sources"`
	Ditto   json.RawMessage `json:"ditto"`
	At      time.Time       `json:"at"`
}

// RecoveryOffloadSources lists the retained recovery directories that exist.
func (e *Engine) RecoveryOffloadSources() []OffloadSource {
	var out []OffloadSource
	for _, s := range []OffloadSource{
		{Name: "infinite-disk-recovery", Dir: filepath.Join(e.Config.StateDir, consolidatedDir)},
		{Name: "infinite-disk-sessions", Dir: filepath.Join(e.Config.StateDir, "session-backups")},
		{Name: "infinite-disk-receipts", Dir: filepath.Join(e.Config.StateDir, coverageDir)},
	} {
		if info, err := os.Lstat(s.Dir); err == nil && info.IsDir() {
			out = append(out, s)
		}
	}
	return out
}

// OffloadRecovery runs `heyditto teleport archive` over the retained recovery
// data. heyditto authenticates with the user's own login; no Ditto credential
// passes through infinite-disk.
func (e *Engine) OffloadRecovery(ctx context.Context, heyditto, capsule string) (OffloadResult, error) {
	res := OffloadResult{Version: ProtocolVersion, Capsule: capsule, At: e.Now().UTC()}
	unlock, err := acquireLock(e.Config)
	if err != nil {
		return res, err
	}
	defer unlock()
	res.Sources = e.RecoveryOffloadSources()
	if len(res.Sources) == 0 {
		return res, errors.New("no retained recovery data to offload")
	}
	bin, err := exec.LookPath(heyditto)
	if err != nil {
		return res, errors.New("heyditto CLI not found; install it with `npm i -g @heyditto/cli` and run `heyditto login`")
	}
	args := []string{"teleport", "archive", "--name", capsule, "--json"}
	for _, s := range res.Sources {
		args = append(args, "--source", s.Name+"="+s.Dir)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout, cmd.Stderr = stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("heyditto teleport archive failed: %w", err)
	}
	if !json.Valid(stdout.Bytes()) {
		return res, errors.New("heyditto returned an unreadable report")
	}
	res.Ditto = json.RawMessage(stdout.Bytes())
	b, err := jsonBytes(res)
	if err != nil {
		return res, err
	}
	return res, atomicPrivate(filepath.Join(e.Config.StateDir, coverageDir, "offload-"+res.At.Format("20060102T150405Z")+".json"), append(b, '\n'))
}

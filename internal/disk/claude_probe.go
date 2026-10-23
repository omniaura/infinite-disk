package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The reviewed version manifest only lags behind Claude releases; it does not
// define the contract. When the installed Claude is not listed, the exact
// handoff command runs against a disposable synthetic conversation and must
// show the same behavior real cleanup requires: zero-turn native init in the
// destination, the stale worktree binding cleared on disk, original history
// kept as an unchanged prefix, hooks not executed, and both transcript-path and
// session-ID resume working. Nothing here touches user sessions or credentials.
var claudeProbeMu sync.Mutex
var claudeProbeResults = map[string]error{}

const claudeProbeTimeout = 20 * time.Second

func probeClaudeContract(ctx context.Context, bin, identity string) error {
	claudeProbeMu.Lock()
	defer claudeProbeMu.Unlock()
	if err, ok := claudeProbeResults[identity]; ok {
		return err
	}
	err := runClaudeProbe(ctx, bin)
	if ctx.Err() == nil {
		claudeProbeResults[identity] = err
	}
	return err
}

func runClaudeProbe(ctx context.Context, bin string) error {
	root, err := os.MkdirTemp("", "infinite-disk-claude-probe-")
	if err != nil {
		return errors.New("could not create a self-test workspace")
	}
	defer os.RemoveAll(root)
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return errors.New("could not create a self-test workspace")
	}
	config := filepath.Join(root, "config")
	destination := filepath.Join(root, "survivor")
	gone := filepath.Join(root, "removed-worktree")
	marker := filepath.Join(root, "hook-must-not-run")
	id := "5c1f2f0e-7a3b-4c52-9d1e-0a6b8f3d2c41"
	store := filepath.Join(config, "projects", claudeProjectKey(gone), id+".jsonl")
	hook, _ := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "touch " + marker}}}}}})
	rows := []any{
		map[string]any{"type": "user", "uuid": "0b8d6e0a-1f1c-4f43-9a55-3d2f2a7c1e01", "parentUuid": nil, "isSidechain": false, "cwd": gone, "sessionId": id, "version": "2.1.302", "timestamp": "2026-01-01T00:00:00.000Z", "message": map[string]any{"role": "user", "content": "Infinite Disk self-test. Do not execute tools."}},
		map[string]any{"type": "assistant", "uuid": "0b8d6e0a-1f1c-4f43-9a55-3d2f2a7c1e02", "parentUuid": "0b8d6e0a-1f1c-4f43-9a55-3d2f2a7c1e01", "isSidechain": false, "cwd": gone, "sessionId": id, "version": "2.1.302", "timestamp": "2026-01-01T00:00:01.000Z", "message": map[string]any{"id": "msg_selftest", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{map[string]any{"type": "text", "text": "Acknowledged."}}, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}},
		map[string]any{"type": "worktree-state", "sessionId": id, "worktreeSession": map[string]any{"worktreePath": gone, "originalCwd": destination, "branch": "self-test", "name": "self-test"}},
	}
	var original bytes.Buffer
	for _, row := range rows {
		b, _ := json.Marshal(row)
		original.Write(b)
		original.WriteByte('\n')
	}
	for path, data := range map[string][]byte{
		store:                                  original.Bytes(),
		filepath.Join(config, "settings.json"): hook,
		filepath.Join(destination, ".claude", "settings.json"): hook,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return errors.New("could not create a self-test workspace")
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return errors.New("could not create a self-test workspace")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, claudeProbeTimeout)
	defer cancel()
	session := Session{Provider: "Claude Code", ID: id, Checkout: gone, Store: store}
	for _, byID := range []bool{false, true} {
		h := ClaudeHandoff{Session: session, Destination: destination, byID: byID}
		cmd := claudeLocalCommand(ctx, bin, h)
		out := &cappedBuffer{limit: 1 << 20}
		cmd.Stdout = out
		if err := cmd.Run(); err != nil || out.overflow {
			return errors.New("the native local handoff command failed")
		}
		if err := verifyClaudeOutput(out.Bytes(), h); err != nil {
			return err
		}
		m, err := readClaudeMetadataContext(ctx, store, id, false)
		if err != nil || m.Binding != "" {
			return errors.New("native Claude did not clear the worktree binding")
		}
		live, err := os.ReadFile(store)
		if err != nil || !bytes.HasPrefix(live, original.Bytes()) {
			return errors.New("native Claude changed original conversation history")
		}
		if _, err := os.Lstat(marker); err == nil {
			return errors.New("native Claude ran a hook despite hooks being disabled")
		}
	}
	return nil
}

// verifiedClaudeExecutable accepts a Claude build if the reviewed manifest
// lists it or the behavioral self-test passes for this exact executable.
func verifiedClaudeExecutable(ctx context.Context, bin, identity, version string) error {
	if testedClaudeVersion(version + " (Claude Code)") {
		return nil
	}
	if err := probeClaudeContract(ctx, bin, identity); err != nil {
		return &untestedClaudeVersion{version: version, reason: err.Error()}
	}
	return nil
}

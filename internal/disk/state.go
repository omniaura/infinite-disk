package disk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
func itoa(i int) string                 { return strconv.Itoa(i) }

var validPlanID = regexp.MustCompile(`^[0-9a-f]{64}$`)

var ErrOperationInProgress = errors.New("another Infinite Disk operation is running; use status --json and retry after the audit finishes")

const stateMarker = "Infinite Disk private state v1\n"

func EnsureState(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	for p := filepath.Clean(c.StateDir); p != filepath.Dir(p); p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("state directory has a symlink ancestor")
		}
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	marker := filepath.Join(c.StateDir, ".infinite-disk-state")
	if info, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(c.StateDir)
		if readErr != nil {
			return readErr
		}
		if len(entries) > 0 {
			return errors.New("state directory contains unowned data; choose a new empty private directory")
		}
		if err = atomicPrivate(marker, []byte(stateMarker)); err != nil {
			return err
		}
	} else if err != nil || !info.Mode().IsRegular() {
		return errors.New("invalid state ownership marker")
	} else {
		b, readErr := os.ReadFile(marker)
		if readErr != nil || string(b) != stateMarker {
			return errors.New("invalid state ownership marker")
		}
	}
	return os.Chmod(c.StateDir, 0700)
}

func SavePlan(c Config, p Plan) (Plan, error) {
	if err := EnsureState(c); err != nil {
		return p, err
	}
	p.Mode = "report-only"
	p.ID = planID(p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return p, err
	}
	if len(b) > 16<<20 {
		return p, errors.New("report exceeds the private plan size limit; narrow discovery roots or project batch size")
	}
	if err = atomicPrivate(filepath.Join(c.StateDir, "plans", p.ID+".json"), b); err != nil {
		return p, err
	}
	if err = atomicPrivate(filepath.Join(c.StateDir, "latest"), []byte(p.ID)); err != nil {
		return p, err
	}
	// Only this tool's old recommendation artifacts are bounded. Recovery receipts
	// and Git bundles are never silently purged.
	entries, _ := os.ReadDir(filepath.Join(c.StateDir, "plans"))
	type old struct {
		path string
		t    time.Time
	}
	var plans []old
	for _, entry := range entries {
		if !entry.IsDir() && validPlanID.MatchString(entry.Name()[:max(0, len(entry.Name())-5)]) && filepath.Ext(entry.Name()) == ".json" {
			if s, e := entry.Info(); e == nil {
				plans = append(plans, old{filepath.Join(c.StateDir, "plans", entry.Name()), s.ModTime()})
			}
		}
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].t.After(plans[j].t) })
	for i := 50; i < len(plans); i++ {
		if plans[i].path != filepath.Join(c.StateDir, "plans", p.ID+".json") {
			_ = os.Remove(plans[i].path)
		}
	}
	return p, nil
}

func LoadPlan(c Config, id string) (Plan, error) {
	var p Plan
	if id == "latest" || id == "" {
		b, e := os.ReadFile(filepath.Join(c.StateDir, "latest"))
		if e != nil {
			return p, errors.New("no saved plan")
		}
		id = string(b)
	}
	if !validPlanID.MatchString(id) {
		return p, errors.New("invalid plan ID")
	}
	file := filepath.Join(c.StateDir, "plans", id+".json")
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 || info.Mode().Perm()&0077 != 0 {
		return p, errors.New("plan is missing, oversized, or not private")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return p, errors.New("invalid plan")
	}
	if p.Version != ProtocolVersion || p.Mode != "report-only" || p.ID != id || planID(p) != id || p.ConfigDigest != ConfigDigest(c) {
		return p, errors.New("plan or configuration changed; rescan")
	}
	return p, nil
}

type Event struct {
	Time        time.Time `json:"time"`
	Type        string    `json:"type"`
	PlanID      string    `json:"plan_id,omitempty"`
	CandidateID string    `json:"candidate_id,omitempty"`
	Outcome     string    `json:"outcome,omitempty"`
}

func AppendEvent(c Config, event Event) error {
	if err := EnsureState(c); err != nil {
		return err
	}
	dir := filepath.Join(c.StateDir, "logs")
	if err := noSymlinkAncestors(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file := filepath.Join(dir, "events.jsonl")
	if info, err := os.Lstat(file); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("refusing non-regular audit log")
		}
		if info.Size() > 10<<20 {
			_ = os.Remove(file + ".4")
			for i := 3; i >= 1; i-- {
				_ = os.Rename(file+"."+itoa(i), file+"."+itoa(i+1))
			}
			if err = os.Rename(file, file+".1"); err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	event.Time = time.Now().UTC()
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (e *Engine) RunReport(ctx context.Context) (Plan, error) {
	release, err := acquireLock(e.Config)
	if err != nil {
		return Plan{}, err
	}
	defer release()
	cursorFile := filepath.Join(e.Config.StateDir, "discovery-cursor.json")
	var cursor struct {
		Version      int    `json:"version"`
		ConfigDigest string `json:"config_digest"`
		LastPath     string `json:"last_path"`
	}
	if b, readErr := os.ReadFile(cursorFile); readErr == nil {
		if json.Unmarshal(b, &cursor) != nil {
			return Plan{}, errors.New("invalid discovery cursor")
		}
		if cursor.Version == ProtocolVersion && cursor.ConfigDigest == ConfigDigest(e.Config) {
			e.scanAfter = cursor.LastPath
		} else {
			e.scanAfter = ""
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Plan{}, readErr
	}
	p, err := e.Scan(ctx)
	if err != nil {
		return p, err
	}
	p, err = SavePlan(e.Config, p)
	if err != nil {
		return p, err
	}
	cursor.Version = ProtocolVersion
	cursor.ConfigDigest = ConfigDigest(e.Config)
	cursor.LastPath = e.scanAfter
	cursorBytes, _ := json.Marshal(cursor)
	if err = atomicPrivate(cursorFile, cursorBytes); err != nil {
		return p, err
	}
	err = AppendEvent(e.Config, Event{Type: "scan_completed", PlanID: p.ID, Outcome: "report-only"})
	return p, err
}

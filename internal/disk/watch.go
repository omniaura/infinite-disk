package disk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type WatchState struct {
	Version    int       `json:"version"`
	Mode       string    `json:"mode"`
	CheckedAt  time.Time `json:"checked_at"`
	LastAudit  time.Time `json:"last_audit"`
	LastTriage time.Time `json:"last_triage,omitempty"`
	LastPlanID string    `json:"last_plan_id,omitempty"`
	Pressure   bool      `json:"pressure"`
	Space      Space     `json:"space"`
	Warning    string    `json:"warning,omitempty"`
}

func ReadWatchState(c Config) (WatchState, error) {
	var s WatchState
	b, err := os.ReadFile(filepath.Join(c.StateDir, "watch.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func UnderPressure(c Config, s Space) bool {
	return s.FreeBytes < c.MinFreeGiB*1024*1024*1024 || s.FreePercent < c.MinFreePercent
}

func (e *Engine) Tick(ctx context.Context) (WatchState, error) {
	if err := EnsureState(e.Config); err != nil {
		return WatchState{}, err
	}
	s, err := ReadWatchState(e.Config)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, errors.New("invalid watcher state")
	}
	home, _ := os.UserHomeDir()
	space, err := DiskSpace(home)
	if err != nil {
		return s, err
	}
	now := e.Now().UTC()
	s.Version = ProtocolVersion
	s.Mode = "report-only"
	s.CheckedAt = now
	s.Space = space
	s.Pressure = UnderPressure(e.Config, space)
	s.Warning = ""
	due := s.LastAudit.IsZero() || now.Sub(s.LastAudit) >= time.Duration(e.Config.PeriodicHours)*time.Hour ||
		(s.Pressure && now.Sub(s.LastAudit) >= time.Duration(e.Config.AuditMinutes)*time.Minute)
	if due {
		p, err := e.RunReport(ctx)
		if err != nil {
			s.Warning = "Audit could not complete; no projects changed"
		} else {
			s.LastAudit = now
			s.LastPlanID = p.ID
			// Paid triage is opt-in, never necessary for safe reporting, and at most once daily.
			if e.Config.AgentEnabled && (s.LastTriage.IsZero() || now.Sub(s.LastTriage) >= 24*time.Hour) {
				s.LastTriage = now
				triaged, err := e.Triage(ctx, p)
				if err != nil {
					s.Warning = "Pi triage unavailable; deterministic report retained"
				} else {
					s.LastPlanID = triaged.ID
				}
			}
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return s, err
	}
	if err = atomicPrivate(filepath.Join(e.Config.StateDir, "watch.json"), b); err != nil {
		return s, err
	}
	if err = AppendEvent(e.Config, Event{Type: "pressure_check", PlanID: s.LastPlanID, Outcome: "report-only"}); err != nil {
		return s, err
	}
	return s, nil
}

func (e *Engine) Watch(ctx context.Context, once bool, observe func(WatchState)) error {
	for {
		s, err := e.Tick(ctx)
		if err != nil {
			return err
		}
		if observe != nil {
			observe(s)
		}
		if once {
			return nil
		}
		timer := time.NewTimer(time.Duration(e.Config.PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

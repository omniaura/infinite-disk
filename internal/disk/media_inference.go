package disk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"time"
)

type MediaAdvice struct {
	Version         int              `json:"version"`
	ID              string           `json:"id"`
	SourcePlan      string           `json:"source_plan"`
	Mode            string           `json:"mode"`
	Recommendations []Recommendation `json:"recommendations"`
	Run             *AgentRun        `json:"agent_run"`
	Steps           []Evidence       `json:"agent_steps"`
	Approved        bool             `json:"approved"`
	Executed        bool             `json:"executed"`
}

func (e *Engine) mediaProjection(p MediaPlan) (Plan, error) {
	if p.Version != 1 || p.ID != mediaID(p) || p.Mode != "report-only" || p.ConfigDigest != ConfigDigest(e.Config) || p.Approved || p.Executed {
		return Plan{}, errors.New("invalid media report")
	}
	q := Plan{Version: 1, Mode: "report-only", CreatedAt: p.CreatedAt, ConfigDigest: p.ConfigDigest}
	if len(p.Files) > 20000 || len(p.Roots) < 1 {
		return q, errors.New("invalid media report scope")
	}
	roots, err := e.mediaRoots(p.Roots)
	if err != nil || len(roots) != len(p.Roots) {
		return q, errors.New("media report roots changed")
	}
	seen := map[string]bool{}
	for _, f := range p.Files {
		if seen[f.ID] || f.ID != pathID(f.Path) || !slices.ContainsFunc(roots, func(r string) bool { return within(r, f.Path) }) || !slices.Contains([]string{"partial_download", "screenshot", "image", "video", "audio", "installer", "archive", "document", "other"}, f.Category) {
			return q, errors.New("invalid media file identity/category")
		}
		seen[f.ID] = true
		for _, blocker := range f.Blockers {
			if !slices.Contains([]string{"private_or_hidden_name", "metadata_unavailable", "unsafe_file_type_links_or_owner", "offline_or_sparse_not_read", "unqualified_filesystem_not_read", "activity_unknown", "file_in_use", "partial_download", "recent_file", "duplicate_hash_budget_or_size_limit", "file_changed_or_hash_unavailable", "incomplete_inventory"}, blocker) {
				return q, errors.New("unknown media hold reason")
			}
		}
		facts := &MediaFacts{Category: f.Category, ExactDuplicate: f.DuplicateOf != "", HashChecked: f.SHA256 != "", OldForReview: !f.Modified.After(e.Now().Add(-time.Duration(e.Config.IdleDays) * 24 * time.Hour))}
		actions := []SuggestedAction{{Kind: "keep", Summary: "Retain this file; no disposal authorized."}}
		if p.Complete && len(f.Blockers) == 0 {
			actions = append(actions, SuggestedAction{Kind: "review_media", Summary: "Human review of a media/download file; no approval, upload or deletion."})
			if f.DuplicateOf != "" {
				actions = append(actions, SuggestedAction{Kind: "review_duplicate", Summary: "Review this exact duplicate while retaining its designated peer; no removal authorized."})
			}
		}
		q.Candidates = append(q.Candidates, Candidate{ID: f.ID, Path: f.Path, Kind: "media-file", SizeBytes: f.Bytes, SizeComplete: true, LastModified: f.Modified, Eligible: p.Complete && len(f.Blockers) == 0, Blockers: f.Blockers, Analysis: &ProjectAnalysis{Actions: actions, Media: facts}})
	}
	q.ID = planID(q)
	return q, nil
}
func (e *Engine) TriageMedia(ctx context.Context, p MediaPlan) (MediaAdvice, error) {
	q, err := e.mediaProjection(p)
	if err != nil {
		return MediaAdvice{}, err
	}
	q, err = e.triageInventory(ctx, q, nil, "media", false)
	if err != nil {
		return MediaAdvice{}, err
	}
	a := MediaAdvice{Version: 1, SourcePlan: p.ID, Mode: "suggestions-only", Recommendations: q.Recommendations, Run: q.AgentRun, Steps: q.AgentSteps}
	b, _ := json.Marshal(a)
	a.ID = PlanDigest(b)
	b, _ = json.MarshalIndent(a, "", "  ")
	err = atomicPrivate(filepath.Join(e.Config.StateDir, "media-advice", a.ID+".json"), b)
	return a, err
}
func makeMediaDecisionRequest(e *Engine, p MediaPlan) (decisionRequest, error) {
	q, err := e.mediaProjection(p)
	if err != nil {
		return decisionRequest{}, err
	}
	r := decisionRequest{Model: decisionModel, Questions: map[string]decisionQuestion{}}
	for _, c := range q.Candidates {
		if len(r.State) == 20 {
			break
		}
		facts := c.Analysis.Media
		criteria := map[string]string{"keep": "Retain the original file. Age and category do not establish disposability.", "escalate": "Evidence is incomplete or personal value cannot be judged from metadata. Ask the user; retain the file."}
		if c.Eligible {
			criteria["review_media"] = "Suggest human review for possible archival, never disposal. Unique screenshots or downloads may be valuable."
			if facts.ExactDuplicate {
				criteria["review_duplicate"] = "Suggest human review of an exact byte-identical extra copy; the designated peer must remain. This is not deletion permission."
			}
		}
		r.State = append(r.State, decisionProject{ID: c.ID, AgeDays: max(0, int(e.Now().Sub(c.LastModified).Hours()/24)), Bytes: c.SizeBytes, SizeComplete: true, Eligible: c.Eligible, Blockers: c.Blockers, Media: facts})
		r.Questions["project_"+c.ID] = decisionQuestion{Type: "choice", Instructions: "Choose only the next review suggestion for this local file's precomputed metadata. Do not decide whether private contents are valuable: you cannot see them. Native Go code owns safety and no action may run. Keep held/incomplete files; uncertainty means escalate. Never treat age alone as disposal evidence.", Criteria: criteria}
	}
	if len(r.State) == 0 {
		return r, errors.New("no media metadata to triage")
	}
	return r, nil
}
func (e *Engine) DecideMedia(ctx context.Context, p MediaPlan, keyFile string) (DecisionReport, error) {
	r, err := makeMediaDecisionRequest(e, p)
	if err != nil {
		return DecisionReport{}, err
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e.executeDecisionRequest(ctx, r, p.ID, keyFile, client)
}

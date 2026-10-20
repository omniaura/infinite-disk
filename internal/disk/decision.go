package disk

// The System One spike is deliberately separate from Pi and approval plans.
// It ranks offered actions from metadata; it cannot inspect or change projects.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

const decisionURL = "https://openrouter.ai/api/alpha/decisions"
const decisionModel = "typesafe/jev-1.14"

type DecisionAnswer struct {
	ProjectID     string             `json:"project_id"`
	Choice        string             `json:"model_choice"`
	Suggested     string             `json:"suggested_action"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Escalated     bool               `json:"escalated"`
}
type DecisionReport struct {
	Version      int              `json:"version"`
	ID           string           `json:"id"`
	SourcePlan   string           `json:"source_plan"`
	Mode         string           `json:"mode"`
	InputKind    string           `json:"input_kind,omitempty"`
	Model        string           `json:"requested_model"`
	ServedModel  string           `json:"served_model"`
	StartedAt    time.Time        `json:"started_at"`
	DurationMS   int64            `json:"duration_ms"`
	Requests     int              `json:"requests"`
	InputTokens  uint64           `json:"input_tokens"`
	OutputTokens uint64           `json:"output_tokens"`
	CostUSD      *float64         `json:"reported_cost_usd,omitempty"`
	Answers      []DecisionAnswer `json:"answers"`
	Approved     bool             `json:"approved"`
	Executed     bool             `json:"executed"`
}
type decisionProject struct {
	ID              string      `json:"id"`
	AgeDays         int         `json:"age_days"`
	Bytes           uint64      `json:"size_bytes"`
	SizeComplete    bool        `json:"size_complete"`
	Eligible        bool        `json:"eligible"`
	SourceChecked   bool        `json:"source_checked"`
	ChangedFiles    int         `json:"changed_files"`
	SessionsChecked bool        `json:"sessions_checked"`
	Blockers        []string    `json:"blockers"`
	Commits         *int        `json:"commits_absent_local_origin_tracking,omitempty"`
	Media           *MediaFacts `json:"media,omitempty"`
}
type decisionQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}
type decisionRequest struct {
	Model     string                      `json:"model"`
	State     []decisionProject           `json:"state"`
	Questions map[string]decisionQuestion `json:"questions"`
}

func makeDecisionRequest(p Plan, selected map[string]bool, now time.Time) (decisionRequest, error) {
	out := decisionRequest{Model: decisionModel, Questions: map[string]decisionQuestion{}}
	for id := range selected {
		if !slices.ContainsFunc(p.Candidates, func(c Candidate) bool { return c.ID == id }) {
			return out, errors.New("project is not in the saved report")
		}
	}
	if len(selected) > 20 {
		return out, errors.New("select at most 20 projects")
	}
	cs := append([]Candidate{}, p.Candidates...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].SizeBytes > cs[j].SizeBytes })
	for _, c := range cs {
		if selected != nil && !selected[c.ID] {
			continue
		}
		if len(out.State) == 20 {
			break
		}
		if !validCandidateID.MatchString(c.ID) || c.ID != pathID(c.Path) {
			return out, errors.New("invalid project identity")
		}
		age := 0
		if !c.LastModified.IsZero() {
			age = max(0, int(now.Sub(c.LastModified).Hours()/24))
		}
		item := decisionProject{c.ID, age, c.SizeBytes, c.SizeComplete, c.Eligible, c.Fingerprint != "", c.DirtyFiles, c.SessionsChecked, c.Blockers, nil, nil}
		if c.Analysis != nil {
			item.Commits = c.Analysis.CommitsNotInOriginTracking
		}
		criteria := map[string]string{"keep": "Keep this checkout; active or recent work is not disposable.", "escalate": "Evidence is incomplete, unsupported or ambiguous; retain it for human or deeper analysis."}
		if c.Analysis != nil {
			for _, a := range c.Analysis.Actions {
				if !a.Executable {
					continue
				}
				switch a.Kind {
				case "preserve_source":
					if CanPreserveSource(c) {
						criteria[a.Kind] = "Suggest preserving source to verified recovery first; this is not permission to push or delete."
					}
				case "private_backup":
					if CanArchivePrivate(c) {
						criteria[a.Kind] = "Suggest an exact local private archive followed by fresh analysis. Never upload private contents."
					}
				case "remove_checkout":
					if c.Eligible && c.SessionsChecked {
						criteria[a.Kind] = "Suggest the trusted CLI's separate approval/preservation/session verification/removal workflow, not direct deletion."
					}
				case "inspect_artifacts":
					criteria[a.Kind] = "Suggest read-only Kondo artifact inspection, not artifact deletion."
				}
			}
		}
		out.State = append(out.State, item)
		out.Questions["project_"+c.ID] = decisionQuestion{"choice", "Select the next useful advisory action for state project " + c.ID + " only. Code owns safety and approvals. Never interpret local tracking refs as proof of a remote push. Prefer preserving source/private data before removal, and keep active/recent work. No action is executed by this answer.", criteria}
	}
	if len(out.State) == 0 {
		return out, errors.New("no project metadata to classify")
	}
	return out, nil
}

func readDecisionKey(name string) (string, error) {
	bad := errors.New("require an absolute owner-only, non-symlink, single-link OpenRouter key file")
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || noSymlinkAncestors(name) != nil {
		return "", bad
	}
	s, err := os.Lstat(name)
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 4096 {
		return "", bad
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() || st.Nlink != 1 {
		return "", bad
	}
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", bad
	}
	defer f.Close()
	a, err := f.Stat()
	if err != nil || !os.SameFile(s, a) || a.Mode().Perm()&0077 != 0 {
		return "", bad
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", bad
	}
	key := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^sk-or-v1-[A-Za-z0-9_-]{16,}$`).MatchString(key) {
		return "", bad
	}
	return key, nil
}
func (e *Engine) Decide(ctx context.Context, p Plan, selected map[string]bool, keyFile string) (DecisionReport, error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e.decide(ctx, p, selected, keyFile, client)
}
func (e *Engine) decide(ctx context.Context, p Plan, selected map[string]bool, keyFile string, client *http.Client) (DecisionReport, error) {
	out := DecisionReport{}
	if p.Version != ProtocolVersion || p.ID != planID(p) || p.ConfigDigest != ConfigDigest(e.Config) || p.Mode != "report-only" {
		return out, errors.New("invalid source report")
	}
	input, err := makeDecisionRequest(p, selected, e.Now())
	if err != nil {
		return out, err
	}
	return e.executeDecisionRequest(ctx, input, p.ID, keyFile, client)
}
func (e *Engine) executeDecisionRequest(ctx context.Context, input decisionRequest, sourceID, keyFile string, client *http.Client) (DecisionReport, error) {
	b, _ := json.Marshal(input)
	return e.executeDecisionPayload(ctx, input, sourceID, keyFile, client, b, 32768, "metadata")
}
func (e *Engine) executeDecisionPayload(ctx context.Context, input decisionRequest, sourceID, keyFile string, client *http.Client, b []byte, limit int, inputKind string) (DecisionReport, error) {
	out := DecisionReport{}
	if len(b) > limit {
		return out, errors.New("decision request exceeds its native byte budget; narrow the selection")
	}
	key, err := readDecisionKey(keyFile)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, decisionURL, bytes.NewReader(b))
	if err != nil {
		return out, errors.New("cannot construct decision request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "Infinite Disk report-only decision experiment")
	started := time.Now().UTC()
	res, err := client.Do(req)
	if err != nil {
		return out, errors.New("OpenRouter decision request failed or was cancelled; original report retained")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("OpenRouter decision request returned HTTP %d; no retry or raw response retained", res.StatusCode)
	}
	if !strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "application/json") {
		return out, errors.New("non-JSON decision response")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (512<<10)+1))
	if err != nil || len(raw) > 512<<10 {
		return out, errors.New("decision response incomplete or oversized")
	}
	var reply struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage *struct {
			Input  *uint64  `json:"input_tokens"`
			Output *uint64  `json:"output_tokens"`
			Cost   *float64 `json:"cost"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &reply) != nil || !agentModelID.MatchString(reply.Model) || !(reply.Model == decisionModel || strings.HasPrefix(reply.Model, decisionModel+"-") || strings.HasPrefix(reply.Model, decisionModel+".")) || reply.Usage == nil || reply.Usage.Input == nil || reply.Usage.Output == nil || *reply.Usage.Input > 1000000 || *reply.Usage.Output > 1000000 || len(reply.Answers) != len(input.Questions) {
		return out, errors.New("invalid decision response; original report retained")
	}
	if reply.Usage.Cost != nil && (!finite(*reply.Usage.Cost) || *reply.Usage.Cost < 0 || *reply.Usage.Cost > 1) {
		return out, errors.New("invalid decision cost evidence")
	}
	out = DecisionReport{Version: 1, SourcePlan: sourceID, Mode: "suggestions-only", InputKind: inputKind, Model: decisionModel, ServedModel: reply.Model, StartedAt: started, DurationMS: time.Since(started).Milliseconds(), Requests: 1, InputTokens: *reply.Usage.Input, OutputTokens: *reply.Usage.Output, CostUSD: reply.Usage.Cost}
	for _, item := range input.State {
		q := input.Questions["project_"+item.ID]
		a, ok := reply.Answers["project_"+item.ID]
		if !ok || a.Type != "choice" || a.Confidence == nil || !unit(*a.Confidence) || len(a.Probabilities) != len(q.Criteria) {
			return DecisionReport{}, errors.New("invalid decision answer or coverage")
		}
		if _, ok := q.Criteria[a.Choice]; !ok {
			return DecisionReport{}, errors.New("decision invented an action")
		}
		sum, largest := 0.0, 0.0
		for label, prob := range a.Probabilities {
			if _, ok := q.Criteria[label]; !ok || !unit(prob) {
				return DecisionReport{}, errors.New("invalid decision probability")
			}
			sum += prob
			largest = max(largest, prob)
		}
		if math.Abs(sum-1) > 0.03 || a.Probabilities[a.Choice]+0.000001 < largest {
			return DecisionReport{}, errors.New("invalid decision distribution")
		}
		suggested := a.Choice
		escalated := *a.Confidence < 0.7 || a.Probabilities[a.Choice] < 0.8
		if escalated {
			suggested = "escalate"
		}
		out.Answers = append(out.Answers, DecisionAnswer{item.ID, a.Choice, suggested, *a.Confidence, a.Probabilities, escalated})
	}
	// Sidecar only: never replaces a Go approval plan or its latest pointer.
	encoded, _ := json.Marshal(out)
	out.ID = PlanDigest(encoded)
	encoded, _ = json.MarshalIndent(out, "", "  ")
	if err = atomicPrivate(filepath.Join(e.Config.StateDir, "decision-reports", out.ID+".json"), encoded); err != nil {
		return DecisionReport{}, err
	}
	return out, nil
}
func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }
func unit(n float64) bool   { return finite(n) && n >= 0 && n <= 1 }

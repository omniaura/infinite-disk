package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type decisionRoundTrip func(*http.Request) (*http.Response, error)

func (f decisionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func decisionFixtureKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	p, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	p = filepath.Join(p, "key")
	if err = os.WriteFile(p, []byte("sk-or-v1-fixture_0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func fakeDecisionReply(req decisionRequest) map[string]any {
	a := map[string]any{}
	for id, q := range req.Questions {
		probs := map[string]float64{}
		for label := range q.Criteria {
			probs[label] = 0
		}
		probs["keep"] = 1
		a[id] = map[string]any{"type": "choice", "choice": "keep", "confidence": 0.95, "probabilities": probs}
	}
	return map[string]any{"model": decisionModel, "answers": a, "usage": map[string]any{"input_tokens": 100, "output_tokens": 0, "cost": 0.0000042}}
}
func TestDecisionMetadataOnlyOneCallAndSidecar(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	c := &p.Candidates[0]
	c.Name = "DO_NOT_SEND_NAME"
	c.Branch = "DO_NOT_SEND_BRANCH"
	c.CommonDir = "DO_NOT_SEND_GIT_PATH"
	c.Sessions = []Session{{Store: "DO_NOT_SEND_SESSION"}}
	p.ID = planID(p)
	p, err := SavePlan(f.e.Config, p)
	if err != nil {
		t.Fatal(err)
	}
	key := decisionFixtureKey(t)
	calls := 0
	client := &http.Client{Transport: decisionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != decisionURL || r.Method != "POST" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk-or-v1-fixture_") {
			t.Fatal("wrong endpoint or credential")
		}
		b, _ := io.ReadAll(r.Body)
		for _, canary := range []string{f.repo, "DO_NOT_SEND_NAME", "DO_NOT_SEND_BRANCH", "DO_NOT_SEND_GIT_PATH", "DO_NOT_SEND_SESSION", "source.txt"} {
			if bytes.Contains(b, []byte(canary)) {
				t.Fatal("private context crossed decision boundary", canary)
			}
		}
		var input decisionRequest
		if json.Unmarshal(b, &input) != nil || len(input.State) != 1 {
			t.Fatal("invalid request")
		}
		reply, _ := json.Marshal(fakeDecisionReply(input))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(reply))}, nil
	})}
	r, err := f.e.decide(context.Background(), p, nil, key, client)
	if err != nil || calls != 1 || r.Requests != 1 || len(r.Answers) != 1 || r.Approved || r.Executed {
		t.Fatal(r, err)
	}
	latest, err := LoadPlan(f.e.Config, "latest")
	if err != nil || latest.ID != p.ID {
		t.Fatal("decision changed latest approval plan", err)
	}
	if !latest.Candidates[0].Eligible || runGit(t, f.remote, "for-each-ref", "--format=%(refname)") != "refs/heads/main" {
		t.Fatal("decision changed safety or remote")
	}
	exists(t, f.repo)
	exists(t, filepath.Join(f.e.Config.StateDir, "decision-reports", r.ID+".json"))
}
func TestDecisionValidationEscalationAndFailures(t *testing.T) {
	for _, kind := range []string{"low confidence", "low probability", "invented action", "missing answer", "extra answer", "invalid probability", "bad sum", "missing confidence", "wrong model", "missing usage", "missing input", "provider error", "non-json", "oversize", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			key := decisionFixtureKey(t)
			calls := 0
			client := &http.Client{Transport: decisionRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				var input decisionRequest
				_ = json.NewDecoder(r.Body).Decode(&input)
				reply := fakeDecisionReply(input)
				answers := reply["answers"].(map[string]any)
				var first map[string]any
				for _, a := range answers {
					first = a.(map[string]any)
					break
				}
				switch kind {
				case "low confidence":
					first["confidence"] = 0.2
				case "low probability":
					probs := first["probabilities"].(map[string]float64)
					probs["keep"] = 0.6
					probs["escalate"] = 0.4
				case "invented action":
					first["choice"] = "run_shell_and_delete"
				case "missing answer":
					reply["answers"] = map[string]any{}
				case "extra answer":
					answers["invented_project"] = first
				case "invalid probability":
					first["probabilities"].(map[string]float64)["keep"] = -1
				case "bad sum":
					first["probabilities"].(map[string]float64)["keep"] = 0.3
				case "missing confidence":
					delete(first, "confidence")
				case "wrong model":
					reply["model"] = "typesafe/jev-1.140-other"
				case "missing usage":
					delete(reply, "usage")
				case "missing input":
					delete(reply["usage"].(map[string]any), "input_tokens")
				case "provider error":
					return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("PRIVATE_PROVIDER_ERROR"))}, nil
				case "cancelled":
					return nil, fmt.Errorf("PRIVATE_PROVIDER_ERROR")
				}
				b, _ := json.Marshal(reply)
				content := "application/json"
				if kind == "non-json" {
					content = "text/html"
				}
				if kind == "oversize" {
					b = []byte(strings.Repeat("x", (512<<10)+1))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{content}}, Body: io.NopCloser(bytes.NewReader(b))}, nil
			})}
			r, err := f.e.decide(context.Background(), p, nil, key, client)
			if kind == "low confidence" || kind == "low probability" {
				if err != nil || !r.Answers[0].Escalated || r.Answers[0].Suggested != "escalate" {
					t.Fatal(r, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "PRIVATE_PROVIDER_ERROR") {
				t.Fatal("invalid reply accepted or raw error leaked", err)
			}
			if calls != 1 {
				t.Fatal("retried paid request")
			}
			exists(t, f.repo)
		})
	}
}
func TestDecisionMetadataBoundAndNoBlockerOverride(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	original := p.Candidates[0]
	p.Candidates = nil
	for i := 0; i < 25; i++ {
		c := original
		c.Path = filepath.Join(f.root, fmt.Sprint(i))
		c.ID = pathID(c.Path)
		c.SizeBytes = uint64(i)
		c.Eligible = false
		c.Blockers = []string{"project_in_use"}
		c.Analysis = &ProjectAnalysis{Actions: []SuggestedAction{{Kind: "remove_checkout", Executable: true}, {Kind: "private_backup", Executable: true}}}
		p.Candidates = append(p.Candidates, c)
	}
	r, err := makeDecisionRequest(p, nil, f.e.Now())
	if err != nil || len(r.State) != 20 || r.State[0].Bytes != 24 {
		t.Fatal(r, err)
	}
	for _, q := range r.Questions {
		if len(q.Criteria) != 2 {
			t.Fatal("offered action bypassed Go blocker", q.Criteria)
		}
	}
	if _, err = makeDecisionRequest(p, map[string]bool{"not-a-project": true}, f.e.Now()); err == nil {
		t.Fatal("invented selection accepted")
	}
	all := map[string]bool{}
	for _, c := range p.Candidates {
		all[c.ID] = true
	}
	if _, err = makeDecisionRequest(p, all, f.e.Now()); err == nil {
		t.Fatal("selection budget ignored")
	}
}
func TestDecisionKeyUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "wrong key", "missing"} {
		t.Run(kind, func(t *testing.T) {
			p := decisionFixtureKey(t)
			switch kind {
			case "mode":
				_ = os.Chmod(p, 0644)
			case "symlink":
				_ = os.Symlink(p, p+"-link")
				p += "-link"
			case "hardlink":
				_ = os.Link(p, p+"-link")
			case "wrong key":
				_ = os.WriteFile(p, []byte("ditto_inf_fixture_not_selected"), 0600)
			case "missing":
				p += "-missing"
			}
			if _, err := readDecisionKey(p); err == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
}

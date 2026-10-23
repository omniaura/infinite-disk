package disk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mediaFixture(t *testing.T) (*Engine, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	root := filepath.Join(home, "Downloads")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	c.StateDir = filepath.Join(home, "state")
	c.Roots = []string{filepath.Join(home, "projects")}
	e := NewEngine(c)
	e.Activity = func(context.Context) Activity { return Activity{Known: true} }
	return e, root
}
func mediaWrite(t *testing.T, root, name, data string) string {
	t.Helper()
	p := filepath.Join(root, name)
	write(t, p, data)
	old(t, p)
	return p
}
func mediaScan(t *testing.T, e *Engine, root string) MediaPlan {
	t.Helper()
	p, err := e.MediaScan(context.Background(), MediaOptions{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMediaScanExactDuplicatesNotAgeAndSeparatePlans(t *testing.T) {
	e, root := mediaFixture(t)
	mediaWrite(t, root, "Screenshot old.png", "same bytes")
	mediaWrite(t, root, "copy.png", "same bytes")
	mediaWrite(t, root, "unique.png", "otherbytes")
	p := mediaScan(t, e, root)
	if !p.Complete || len(p.Files) != 3 || p.DuplicateCopyBytes != 10 || p.Approved || p.Executed {
		t.Fatal(p)
	}
	duplicates := 0
	for _, f := range p.Files {
		if f.DuplicateOf != "" {
			duplicates++
			if f.SHA256 == "" || f.Suggested != "review_duplicate" {
				t.Fatal(f)
			}
		}
	}
	if duplicates != 1 {
		t.Fatal(p.Files)
	}
	if _, err := os.Stat(filepath.Join(e.Config.StateDir, "latest")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("media overwrote project latest")
	}
	loaded, err := e.LoadMedia("latest")
	if err != nil || loaded.ID != p.ID {
		t.Fatal(err)
	}
	for _, f := range p.Files {
		if err = e.VerifyMediaFile(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
}
func TestMediaScanSafetyAndCancellation(t *testing.T) {
	for _, kind := range []string{"active", "unknown", "symlink", "hardlink", "partial", "recent", "private"} {
		t.Run(kind, func(t *testing.T) {
			e, root := mediaFixture(t)
			name := "old.png"
			if kind == "partial" {
				name = "video.part"
			}
			if kind == "private" {
				name = ".credential.png"
			}
			path := mediaWrite(t, root, name, "old")
			switch kind {
			case "active":
				e.Activity = func(context.Context) Activity { return Activity{Known: true, Paths: []string{path}} }
			case "unknown":
				e.Activity = func(context.Context) Activity { return Activity{} }
			case "symlink":
				os.Remove(path)
				if err := os.Symlink(filepath.Join(t.TempDir(), "external"), path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(root, "linked.png")); err != nil {
					t.Fatal(err)
				}
			case "recent":
				if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p := mediaScan(t, e, root)
			if len(p.Files) == 0 || len(p.Files[0].Blockers) == 0 {
				t.Fatal(p)
			}
			if err := e.VerifyMediaFile(context.Background(), p.Files[0]); err == nil {
				t.Fatal("held file accepted")
			}
		})
	}
	e, root := mediaFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.MediaScan(ctx, MediaOptions{Roots: []string{root}}); err == nil {
		t.Fatal("cancel ignored")
	}
}
func TestMediaIncompleteAndChangedPlan(t *testing.T) {
	e, root := mediaFixture(t)
	mediaWrite(t, root, "a.png", "same")
	mediaWrite(t, root, "b.png", "same")
	p, err := e.MediaScan(context.Background(), MediaOptions{Roots: []string{root}, MaxFiles: 1})
	if err != nil || p.Complete || p.DuplicateCopyBytes != 0 || !slices.Contains(p.Files[0].Blockers, "incomplete_inventory") {
		t.Fatal(p, err)
	}
	p = mediaScan(t, e, root)
	f := p.Files[0]
	write(t, f.Path, "changed")
	if e.VerifyMediaFile(context.Background(), f) == nil {
		t.Fatal("stale file accepted")
	}
	p.Files[0].Path = "/external/file"
	if _, err = e.mediaProjection(p); err == nil {
		t.Fatal("modified plan accepted")
	}
	if _, err = e.MediaScan(context.Background(), MediaOptions{Roots: []string{filepath.Dir(root)}}); err == nil {
		t.Fatal("broad home accepted")
	}
}
func TestMediaDecisionPrivacyAndValidation(t *testing.T) {
	e, root := mediaFixture(t)
	mediaWrite(t, root, "Screenshot SECRET_NAME.png", "SECRET_CONTENT")
	p := mediaScan(t, e, root)
	req, err := makeMediaDecisionRequest(e, p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(req)
	for _, private := range []string{root, "SECRET", "sha256", "path", "filename", "identity"} {
		if bytes.Contains(b, []byte(private)) {
			t.Fatal("private data in metadata request", private)
		}
	}
	calls := 0
	client := &http.Client{Transport: decisionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		reply := fakeDecisionReply(req)
		raw, _ := json.Marshal(reply)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}
	r, err := e.executeDecisionRequest(context.Background(), req, p.ID, decisionFixtureKey(t), client)
	if err != nil || calls != 1 || r.Approved || r.Executed {
		t.Fatal(r, err)
	}
	client.Transport = decisionRoundTrip(func(r *http.Request) (*http.Response, error) {
		reply := fakeDecisionReply(req)
		for k := range req.Questions {
			reply["answers"].(map[string]any)[k] = map[string]any{"type": "choice", "choice": "delete", "confidence": 1, "probabilities": map[string]float64{"delete": 1}}
		}
		raw, _ := json.Marshal(reply)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	if _, err = e.executeDecisionRequest(context.Background(), req, p.ID, decisionFixtureKey(t), client); err == nil {
		t.Fatal("unoffered destructive decision accepted")
	}
}
func TestMediaProtectedSiblingExcludedNotGlobalBlocker(t *testing.T) {
	e, root := mediaFixture(t)
	path := mediaWrite(t, root, "Screenshot fixture.png", "fixture")
	for _, name := range []string{"Installer.app", ".hidden", "Pictures.photoslibrary", "vendor"} {
		mediaWrite(t, filepath.Join(root, name), "private-content", "DO_NOT_READ_OR_HASH")
	}
	p := mediaScan(t, e, root)
	if !p.Complete || len(p.Files) != 1 || len(p.Excluded) != 4 || len(p.Files[0].Blockers) > 0 {
		t.Fatal(p)
	}
	d := newFixtureDrive()
	if _, err := e.PlanMediaArchive(context.Background(), p, []string{p.Files[0].ID}, DriveDestination{Account: d.account, FolderID: d.folder.ID, Visibility: "private"}, d); err != nil {
		t.Fatal("safe PNG blocked by excluded package", err)
	}
	exists(t, path)
	for _, x := range p.Excluded {
		exists(t, filepath.Join(x.Path, "private-content"))
	}
}

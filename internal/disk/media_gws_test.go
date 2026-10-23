package disk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureGWS(t *testing.T) (*GWSDrive, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(base, "gws")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$GWS_FIXTURE_LOG"
env > "$GWS_FIXTURE_ENV"
case "$1 $2 $3" in
  'drive permissions list')
    if [ "$GWS_FIXTURE_MODE" = incomplete ]; then
      printf '%s' '{"permissions":[{"id":"perm_123456","type":"user","role":"owner","emailAddress":"fixture@example.invalid"}],"nextPageToken":"same"}'
    elif [ "$GWS_FIXTURE_MODE" = public ]; then
      printf '%s' '{"permissions":[{"id":"perm_123456","type":"anyone","role":"reader"}]}'
    elif [ "$GWS_FIXTURE_MODE" = pages ]; then
      case "$*" in
        *pageToken*) printf '%s' '{"permissions":[{"id":"perm_second","type":"group","role":"reader","emailAddress":"group@example.invalid"}]}' ;;
        *) printf '%s' '{"permissions":[{"id":"perm_123456","type":"user","role":"owner","emailAddress":"fixture@example.invalid"}],"nextPageToken":"next"}' ;;
      esac
    else printf '%s' '{"permissions":[]}'
    fi ;;
  'drive about get') printf '%s' '{"user":{"emailAddress":"fixture@example.invalid"}}' ;;
  'drive files create') printf '%s' '{"id":"remote_123456"}' ;;
  'drive files list') printf '%s' '{"files":[{"id":"folder_123456","name":"Fixture"}]}' ;;
  '--help  ') printf '%s' 'GOOGLE_WORKSPACE_CLI_ACCOUNT' ;;
  *) printf '%s' '{}' ;;
esac
`
	if err = os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(base, "args")
	t.Setenv("GWS_FIXTURE_LOG", log)
	t.Setenv("GWS_FIXTURE_ENV", filepath.Join(base, "environment"))
	t.Setenv("GOOGLE_WORKSPACE_CLI_TOKEN", "not-an-actual-token")
	t.Setenv("OPENROUTER_API_KEY", "fixture-not-real")
	return &GWSDrive{Account: "fixture@example.invalid", binary: binary}, log
}
func TestGWSPermissionPaginationAndFailClosedAudience(t *testing.T) {
	d, log := fixtureGWS(t)
	for _, mode := range []string{"empty", "public", "incomplete", "pages"} {
		t.Setenv("GWS_FIXTURE_MODE", mode)
		perms, err := d.Permissions(context.Background(), "folder_123456")
		if mode == "pages" {
			if err != nil || len(perms) != 2 {
				t.Fatal(perms, err)
			}
			b, _ := os.ReadFile(log)
			if !strings.Contains(string(b), `"supportsAllDrives":true`) || !strings.Contains(string(b), `"pageToken":"next"`) {
				t.Fatal("incomplete shared permissions parameters")
			}
		} else if err == nil {
			t.Fatal("uncertain ACL accepted", mode)
		}
	}
}
func TestGWSExplicitIdentitySharedParametersNoConversionAndNoCredentialOverrides(t *testing.T) {
	d, log := fixtureGWS(t)
	if _, err := d.Identity(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(log), "environment"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "GOOGLE_WORKSPACE_CLI_TOKEN=") || strings.Contains(string(b), "OPENROUTER_API_KEY=") || !strings.Contains(string(b), "GOOGLE_WORKSPACE_CLI_ACCOUNT=fixture@example.invalid") {
		t.Fatal("credential override leaked to gws")
	}
	d.Account = "wrong@example.invalid"
	if _, err = d.Identity(context.Background()); err == nil {
		t.Fatal("wrong account accepted")
	}
	d.Account = "fixture@example.invalid"
	if _, err = d.Destinations(context.Background(), "shared_123456"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	for _, v := range []string{`"corpora":"drive"`, `"driveId":"shared_123456"`, `"includeItemsFromAllDrives":true`, `"supportsAllDrives":true`} {
		if !strings.Contains(string(b), v) {
			t.Fatal("missing shared drive parameter", v)
		}
	}
	stage := filepath.Join(filepath.Dir(log), "stage")
	raw := []byte("fixture")
	if err = os.WriteFile(stage, raw, 0400); err != nil {
		t.Fatal(err)
	}
	sum, _ := digestFile(stage)
	f := MediaFile{ID: "abcdef0123456789", Path: "/unused/private-name.png", Bytes: uint64(len(raw)), SHA256: sum}
	if err = d.Upload(context.Background(), DriveDestination{FolderID: "folder_123456"}, f, "archive_123456", "remote_123456", stage); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var params, body map[string]any
	for i, s := range lines {
		if s == "--params" {
			json.Unmarshal([]byte(lines[i+1]), &params)
		}
		if s == "--json" {
			json.Unmarshal([]byte(lines[i+1]), &body)
		}
	}
	if params["supportsAllDrives"] != true || params["ignoreDefaultVisibility"] != true || body["id"] != "remote_123456" || body["mimeType"] != "application/octet-stream" {
		t.Fatal("binary upload protocol unqualified")
	}
	if err = d.Download(context.Background(), "remote_123456", stage); err == nil {
		t.Fatal("existing restore target overwrite allowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = d.Identity(ctx); err == nil {
		t.Fatal("cancel ignored")
	}
}

package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoPiBoundaryRejectsInventedIDsAndTransmitsOnlyMetadata(t *testing.T) {
	for _, kind := range []string{"valid", "invented", "override", "extra tool"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			p := plan(t, f)
			p.Candidates[0].Eligible = false
			p.Candidates[0].Blockers = []string{"project_in_use"}
			p.ID = planID(p)
			id := p.Candidates[0].ID
			decision := "blocked"
			capabilities := `["inventory_summary","inspect_project"]`
			switch kind {
			case "invented":
				id = "ffffffffffffffff"
			case "override":
				decision = "review"
			case "extra tool":
				capabilities = `["inventory_summary","bash"]`
			}
			script := `let raw="";for await(const c of process.stdin)raw+=c;const input=JSON.parse(raw);for(const c of input.candidates){for(const key of Object.keys(c)){if(!["id","eligible","age_days","size_bytes","dirty_files","blockers"].includes(key))process.exit(2);}}process.stdout.write(JSON.stringify({version:1,recommendations:[{candidate_id:"` + id + `",priority:1,decision:"` + decision + `",reason:"fixture metadata",proposed_steps:[]}],steps:[{check:"inventory_summary",outcome:"performed"}],capabilities:` + capabilities + `}));`
			write(t, filepath.Join(f.e.Config.StateDir, "agent", "bridge.mjs"), script)
			result, err := f.e.Triage(context.Background(), p)
			if kind == "valid" {
				if err != nil || len(result.Recommendations) != 1 {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("untrusted worker response accepted")
			}
			exists(t, f.repo)
			if runGit(t, f.remote, "for-each-ref", "--format=%(refname)") != "refs/heads/main" {
				t.Fatal("triage pushed anything")
			}
		})
	}
}

func TestOwnedServiceLifecycleAndCrontabFailures(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.root)
	home := filepath.Join(base, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	config := filepath.Join(base, "config.json")
	if err := SaveConfig(config, f.e.Config); err != nil {
		t.Fatal(err)
	}
	fakebin := filepath.Join(base, "fakebin")
	write(t, filepath.Join(fakebin, "launchctl"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(fakebin, "launchctl"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))
	service, err := InstallService(context.Background(), f.e.Config, config, "darwin", false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.Stat(service)
	if err != nil || s.Mode().Perm() != 0600 {
		t.Fatal("service privacy", err)
	}
	if err := UninstallService(context.Background(), "darwin"); err != nil {
		t.Fatal(err)
	}
	exists(t, config)
	write(t, service, "unrelated service")
	if _, err := InstallService(context.Background(), f.e.Config, config, "darwin", false); err == nil {
		t.Fatal("unrelated service overwritten")
	}
	if UninstallService(context.Background(), "darwin") == nil {
		t.Fatal("unrelated service removed")
	}
	fake := filepath.Join(fakebin, "crontab")
	write(t, fake, "#!/bin/sh\nif [ \"$1\" = -l ]; then echo 'operational failure' >&2; exit 2; fi\nexit 99\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallService(context.Background(), f.e.Config, config, "cron", false); err == nil {
		t.Fatal("failed read could overwrite crontab")
	}
	state := filepath.Join(base, "crontab-fixture")
	write(t, state, "# unrelated job\n0 4 * * * /usr/bin/true\n")
	t.Setenv("CRONTAB_FIXTURE", state)
	write(t, fake, "#!/bin/sh\nif [ \"$1\" = -l ]; then /bin/cat \"$CRONTAB_FIXTURE\"; else /bin/cat > \"$CRONTAB_FIXTURE\"; fi\n")
	if _, err := InstallService(context.Background(), f.e.Config, config, "cron", false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(state)
	if !strings.Contains(string(b), "unrelated job") || !strings.Contains(string(b), serviceMarker) {
		t.Fatal("cron entry lost")
	}
	if err := UninstallService(context.Background(), "cron"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(state)
	if string(b) != "# unrelated job\n0 4 * * * /usr/bin/true\n" {
		t.Fatal("uninstall changed unrelated cron", string(b))
	}
}

func TestServiceEscapingAndOutputBudget(t *testing.T) {
	s, err := ServiceContent("linux", "/opt/$HOME%path", "/opt/config", "/opt/state")
	if err != nil || !strings.Contains(s, "$$HOME%%path") {
		t.Fatal(s, err)
	}
	s, err = ServiceContent("cron", "/opt/percent%path", "/opt/config", "/opt/state")
	if err != nil || !strings.Contains(s, `\%`) {
		t.Fatal(s, err)
	}
	b := &cappedBuffer{limit: 2}
	if _, err := b.Write([]byte("too long")); err == nil || !b.overflow {
		t.Fatal("command output overflow not detected")
	}
}

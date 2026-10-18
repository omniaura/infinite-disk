package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTriageBoundsLargeRealInventoryWithoutChangingSafety(t *testing.T) {
	f := newFixture(t)
	p := plan(t, f)
	original := p.Candidates[0]
	p.Candidates = nil
	for i := 0; i < 255; i++ {
		c := original
		c.ID = fmt.Sprintf("%016x", i+1)
		c.SizeBytes = uint64(i + 1)
		c.Eligible = false
		c.Blockers = []string{"coding_sessions_require_native_handoff"}
		p.Candidates = append(p.Candidates, c)
	}
	write(t, filepath.Join(f.e.Config.StateDir, "agent/bridge.mjs"), `let raw="";for await(const c of process.stdin)raw+=c;const input=JSON.parse(raw);if(input.candidates.length!==20||input.candidates[0].size_bytes!==255||input.candidates[19].size_bytes!==236)process.exit(1);process.stdout.write(JSON.stringify({version:1,requests:2,recommendations:[],steps:[{check:"inventory_summary",outcome:"performed"}],capabilities:["inventory_summary","inspect_project","suggest_action"]}));`)
	result, err := f.e.Triage(context.Background(), p)
	if err != nil || len(result.Candidates) != 255 {
		t.Fatal(err)
	}
	for _, c := range result.Candidates {
		if c.Eligible {
			t.Fatal("advice bypassed blocker")
		}
	}
	for _, code := range []int{65, 69, 70, 75} {
		write(t, filepath.Join(f.e.Config.StateDir, "agent/bridge.mjs"), fmt.Sprintf(`process.stderr.write("private provider error must not leak");process.exit(%d);`, code))
		result, err = f.e.Triage(context.Background(), p)
		if err == nil || strings.Contains(err.Error(), "private provider") || len(result.Candidates) != 255 {
			t.Fatal("lost report or leaked error", err)
		}
	}
}

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
			capabilities := `["inventory_summary","inspect_project","suggest_action"]`
			switch kind {
			case "invented":
				id = "ffffffffffffffff"
			case "override":
				decision = "review"
			case "extra tool":
				capabilities = `["inventory_summary","bash"]`
			}
			script := `let raw="";for await(const c of process.stdin)raw+=c;const input=JSON.parse(raw);for(const key of Object.keys(input.candidates[0])){if(!["id","eligible","age_days","size_bytes","size_complete","dirty_files","blockers","analysis"].includes(key))process.exit(2);}process.stdout.write(JSON.stringify({version:1,requests:2,recommendations:[{candidate_id:"` + id + `",priority:1,decision:"` + decision + `",reason:"fixture metadata",proposed_steps:[]}],steps:[{check:"inventory_summary",outcome:"performed"}],capabilities:` + capabilities + `}));`
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
	if _, err := InstallService(context.Background(), f.e.Config, config, "darwin", false); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fakebin, "launchctl"), "#!/bin/sh\nif [ \"$1\" = print ]; then exit 0; fi\nexit 42\n")
	if UninstallService(context.Background(), "darwin") == nil {
		t.Fatal("failed stop reported as successful uninstall")
	}
	exists(t, service)
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

package cli

import (
	"bytes"
	"github.com/omniaura/infinite-disk/internal/disk"
	"testing"
)

func TestDecisionCLIRequiresExplicitCredentialAndOffersNoApprovalFlag(t *testing.T) {
	for _, args := range [][]string{{}, {"--approve-plan", "never"}, {"--apply"}} {
		called := false
		asJSON := true
		cmd := decisionCommand(func() (*disk.Engine, error) { called = true; return nil, nil }, &asJSON)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if cmd.Execute() == nil || called {
			t.Fatal("missing consent for explicit paid experiment or approval flag accepted", args)
		}
	}
}

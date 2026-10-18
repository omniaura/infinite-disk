package cli

import (
	"encoding/json"
	"fmt"

	"github.com/omniaura/infinite-disk/internal/disk"
	"github.com/spf13/cobra"
)

func agentConfigCommand(load func() (*disk.Engine, error), configPath *string, asJSON *bool) *cobra.Command {
	var provider, model, baseURL, keyFile string
	cmd := &cobra.Command{Use: "configure", Short: "Configure inference using a credential-file reference; never enables scheduled paid runs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load()
			if err != nil {
				return err
			}
			c := e.Config
			c.AgentProvider, c.AgentModel, c.AgentBaseURL, c.AgentKeyFile = provider, model, baseURL, keyFile
			if _, err := disk.AgentEnvironment(c); err != nil {
				return err
			}
			if err := disk.SaveConfig(*configPath, c); err != nil {
				return err
			}
			if *asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"version": 1, "provider": provider, "model": model, "credential": "file-reference-only", "scheduled_paid_inference": c.AgentEnabled})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Inference configured: %s / %s. Key reference only; scheduled paid inference remains %t.\n", provider, model, c.AgentEnabled)
			return err
		}}
	cmd.Flags().StringVar(&provider, "provider", "ditto", "Inference provider: ditto or openrouter")
	cmd.Flags().StringVar(&model, "model", "", "Explicit model ID (required)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Official Ditto Router base URL from heyditto endpoints")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "Private endpoint-key file created by heyditto; never the key itself")
	_ = cmd.MarkFlagRequired("model")
	return cmd
}

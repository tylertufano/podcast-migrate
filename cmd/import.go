package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

func importCmd() *cobra.Command {
	var (
		to               string
		in               string
		dryRun           bool
		onlySubs         bool
		overcastOut      string
		conflictStrategy string
	)

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a JSON library export into a provider",
		Example: `  podcast-migrate import --to overcast --in ~/Desktop/my-podcasts.json \
    --overcast-out ~/Desktop/import-to-overcast.opml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(in)
			if err != nil {
				return fmt.Errorf("import: read %s: %w", in, err)
			}

			var lib model.Library
			if err := json.Unmarshal(data, &lib); err != nil {
				return fmt.Errorf("import: parse JSON: %w", err)
			}

			dst, err := buildProvider(to, "", "", "", overcastOut)
			if err != nil {
				return err
			}

			opts := provider.WriteOptions{
				DryRun:            dryRun,
				OnlySubscriptions: onlySubs,
				ConflictStrategy:  parseConflictStrategy(conflictStrategy),
			}

			if err := dst.SetLibrary(context.Background(), &lib, opts); err != nil {
				return fmt.Errorf("import: %w", err)
			}

			if !dryRun {
				fmt.Printf("imported %d podcasts to %s\n", len(lib.Podcasts), to)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "destination app: overcast (required)")
	cmd.Flags().StringVar(&in, "in", "", "path to JSON library file (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview without writing")
	cmd.Flags().BoolVar(&onlySubs, "only-subscriptions", false, "import subscriptions only")
	cmd.Flags().StringVar(&overcastOut, "overcast-out", "", "path for generated Overcast OPML import file")
	cmd.Flags().StringVar(&conflictStrategy, "conflict", "furthest", "conflict resolution: furthest | source | target")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.MarkFlagRequired("in")

	return cmd
}

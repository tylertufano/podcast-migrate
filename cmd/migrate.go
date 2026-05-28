package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tyler/podcast-migrate/internal/apple"
	"github.com/tyler/podcast-migrate/internal/overcast"
	"github.com/tyler/podcast-migrate/internal/provider"
	"github.com/tyler/podcast-migrate/internal/sync"
)

func migrateCmd() *cobra.Command {
	var (
		from             string
		to               string
		dryRun           bool
		onlySubs         bool
		sqlitePath       string
		opmlFallback     string
		overcastExport   string
		overcastOut      string
		conflictStrategy string
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate subscriptions and play state from one app to another",
		Example: `  # Podcasts → Overcast (subscriptions only, dry-run)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-out ~/Desktop/import-to-overcast.opml --dry-run

  # Podcasts → Overcast (full migration)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-out ~/Desktop/import-to-overcast.opml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := buildProvider(from, sqlitePath, opmlFallback, overcastExport, "")
			if err != nil {
				return fmt.Errorf("source: %w", err)
			}
			dst, err := buildProvider(to, "", "", "", overcastOut)
			if err != nil {
				return fmt.Errorf("destination: %w", err)
			}

			opts := provider.WriteOptions{
				DryRun:            dryRun,
				OnlySubscriptions: onlySubs,
				ConflictStrategy:  parseConflictStrategy(conflictStrategy),
			}

			engine := sync.New(src, dst)
			result, err := engine.Run(context.Background(), opts)
			if err != nil {
				return err
			}

			fmt.Println(result)

			if !dryRun && to == "overcast" && overcastOut != "" {
				fmt.Printf("\nNext step: open Overcast > Settings > Import OPML and select:\n  %s\n", overcastOut)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source app: podcasts (required)")
	cmd.Flags().StringVar(&to, "to", "", "destination app: overcast (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without writing anything")
	cmd.Flags().BoolVar(&onlySubs, "only-subscriptions", false, "migrate subscriptions only, skip play state")
	cmd.Flags().StringVar(&sqlitePath, "sqlite", "", "path to MTLibrary.sqlite (default: auto-detect)")
	cmd.Flags().StringVar(&opmlFallback, "opml-fallback", "", "path to Apple Podcasts OPML export (fallback if SQLite unavailable)")
	cmd.Flags().StringVar(&overcastExport, "overcast-export", "", "path to Overcast OPML export (for reading Overcast data)")
	cmd.Flags().StringVar(&overcastOut, "overcast-out", "", "path for the generated Overcast import OPML file")
	cmd.Flags().StringVar(&conflictStrategy, "conflict", "furthest", "conflict resolution: furthest | source | target")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

func buildProvider(name, sqlitePath, opmlFallback, overcastImport, overcastOut string) (provider.Provider, error) {
	switch name {
	case "podcasts", "apple":
		return apple.NewProvider(sqlitePath, opmlFallback), nil
	case "overcast":
		if overcastImport == "" && overcastOut == "" {
			return nil, fmt.Errorf("overcast requires --overcast-export (read) or --overcast-out (write)")
		}
		return overcast.NewProvider(overcastImport, overcastOut), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (supported: podcasts, overcast)", name)
	}
}

func parseConflictStrategy(s string) provider.ConflictStrategy {
	switch s {
	case "source":
		return provider.SourceWins
	case "target":
		return provider.TargetWins
	default:
		return provider.FurthestWins
	}
}


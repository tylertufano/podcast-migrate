package cmd

import (
	"context"
	"fmt"
	"os"

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
		playState        bool
		sqlitePath       string
		opmlFallback     string
		overcastExport   string
		overcastOut      string
		overcastEmail    string
		overcastPassword string
		conflictStrategy string
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate subscriptions and play state from one app to another",
		Example: `  # Podcasts → Overcast (subscriptions only, dry-run)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-out ~/Desktop/import-to-overcast.opml --dry-run

  # Podcasts → Overcast (subscriptions + play state via unofficial web API)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-out ~/Desktop/import-to-overcast.opml \
    --overcast-export ~/Downloads/overcast.opml \
    --play-state
  # Credentials: set OVERCAST_EMAIL and OVERCAST_PASSWORD environment variables,
  # or pass --overcast-email and --overcast-password flags.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve Overcast credentials from flags → env vars.
			if overcastEmail == "" {
				overcastEmail = os.Getenv("OVERCAST_EMAIL")
			}
			if overcastPassword == "" {
				overcastPassword = os.Getenv("OVERCAST_PASSWORD")
			}

			if playState && overcastEmail == "" {
				return fmt.Errorf("--play-state requires Overcast credentials: set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email / --overcast-password")
			}
			if playState && overcastExport == "" {
				return fmt.Errorf("--play-state requires --overcast-export (path to your extended OPML from overcast.fm/account/export_opml/extended) for episode matching")
			}

			src, err := buildProvider(from, sqlitePath, opmlFallback, overcastExport, "", "", "")
			if err != nil {
				return fmt.Errorf("source: %w", err)
			}
			dst, err := buildProvider(to, "", "", overcastExport, overcastOut, overcastEmail, overcastPassword)
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
				if playState {
					fmt.Println("Play state has been written directly via the Overcast web API.")
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source app: podcasts (required)")
	cmd.Flags().StringVar(&to, "to", "", "destination app: overcast (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without writing anything")
	cmd.Flags().BoolVar(&onlySubs, "only-subscriptions", false, "migrate subscriptions only, skip play state")
	cmd.Flags().BoolVar(&playState, "play-state", false, "also write episode play state via the unofficial Overcast web API (requires credentials)")
	cmd.Flags().StringVar(&sqlitePath, "sqlite", "", "path to MTLibrary.sqlite (default: auto-detect)")
	cmd.Flags().StringVar(&opmlFallback, "opml-fallback", "", "path to Apple Podcasts OPML export (fallback if SQLite unavailable)")
	cmd.Flags().StringVar(&overcastExport, "overcast-export", "", "path to Overcast OPML export (for reading Overcast data or play state matching)")
	cmd.Flags().StringVar(&overcastOut, "overcast-out", "", "path for the generated Overcast import OPML file")
	cmd.Flags().StringVar(&overcastEmail, "overcast-email", "", "Overcast account email (or set OVERCAST_EMAIL env var)")
	cmd.Flags().StringVar(&overcastPassword, "overcast-password", "", "Overcast account password (or set OVERCAST_PASSWORD env var)")
	cmd.Flags().StringVar(&conflictStrategy, "conflict", "furthest", "conflict resolution: furthest | source | target")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

func buildProvider(name, sqlitePath, opmlFallback, overcastImport, overcastOut, overcastEmail, overcastPassword string) (provider.Provider, error) {
	switch name {
	case "podcasts", "apple":
		return apple.NewProvider(sqlitePath, opmlFallback), nil
	case "overcast":
		if overcastImport == "" && overcastOut == "" {
			return nil, fmt.Errorf("overcast requires --overcast-export (read) or --overcast-out (write)")
		}
		if overcastEmail != "" {
			return overcast.NewProviderWithCredentials(overcastImport, overcastOut, overcastEmail, overcastPassword), nil
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

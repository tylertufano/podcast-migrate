package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

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
		requestDelay     time.Duration
		podcastFilter    []string // --podcast (repeatable)
		podcastListFile  string   // --podcast-list (file path)
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
  # or pass --overcast-email and --overcast-password flags.

  # Sync play state for a single podcast (word match, case-insensitive)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-export ~/Downloads/overcast.opml --play-state \
    --podcast "sistersinlaw"

  # Sync play state for podcasts listed in a file (one title/word per line)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-export ~/Downloads/overcast.opml --play-state \
    --podcast-list ~/my-podcasts.txt

  # Overcast → Podcasts (reverse sync: write Overcast play state back to Apple Podcasts)
  # Quit Apple Podcasts first, then run:
  podcast-migrate migrate --from overcast --to podcasts \
    --overcast-export ~/Downloads/overcast.opml --play-state

  # Reverse sync — single podcast, dry-run first
  podcast-migrate migrate --from overcast --to podcasts \
    --overcast-export ~/Downloads/overcast.opml --play-state \
    --podcast "sistersinlaw" --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve Overcast credentials from flags → env vars.
			if overcastEmail == "" {
				overcastEmail = os.Getenv("OVERCAST_EMAIL")
			}
			if overcastPassword == "" {
				overcastPassword = os.Getenv("OVERCAST_PASSWORD")
			}

			// Direction-aware validation for --play-state:
			//   Podcasts → Overcast: requires credentials + overcast-export (for episode matching)
			//   Overcast → Podcasts: requires overcast-export (the OPML is the source); no credentials needed
			if playState {
				switch {
				case (from == "podcasts" || from == "apple") && (to == "overcast"):
					if overcastEmail == "" {
						return fmt.Errorf("--play-state requires Overcast credentials: set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email / --overcast-password")
					}
					if overcastExport == "" {
						return fmt.Errorf("--play-state requires --overcast-export (path to your extended OPML from overcast.fm/account/export_opml/extended) for episode matching")
					}
				case (from == "overcast") && (to == "podcasts" || to == "apple"):
					if overcastExport == "" {
						return fmt.Errorf("--play-state requires --overcast-export (path to your Overcast OPML export from overcast.fm/account/export_opml/extended)")
					}
				}
			}

			// Merge podcast filter patterns from --podcast flags and --podcast-list file.
			allFilters, err := buildPodcastFilter(podcastFilter, podcastListFile)
			if err != nil {
				return err
			}

			src, err := buildProvider(from, sqlitePath, opmlFallback, overcastExport, "", "", "")
			if err != nil {
				return fmt.Errorf("source: %w", err)
			}
			// Pass sqlitePath and opmlFallback for the destination too — needed when
			// the destination is Apple Podcasts (reverse sync: overcast → podcasts).
			dst, err := buildProvider(to, sqlitePath, opmlFallback, overcastExport, overcastOut, overcastEmail, overcastPassword)
			if err != nil {
				return fmt.Errorf("destination: %w", err)
			}

			opts := provider.WriteOptions{
				DryRun:            dryRun,
				OnlySubscriptions: onlySubs,
				// When --play-state is set and destination is Apple Podcasts, restrict
				// writes to play state only (Apple has no subscription write API).
				OnlyPlayState:    playState && (to == "podcasts" || to == "apple"),
				ConflictStrategy: parseConflictStrategy(conflictStrategy),
				RequestDelay:     requestDelay,
				PodcastFilter:    allFilters,
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
			if !dryRun && (to == "podcasts" || to == "apple") && playState {
				fmt.Println("\nPlay state has been written to Apple Podcasts' local database.")
				fmt.Println("Open Apple Podcasts to see the updated episode states.")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source app: podcasts (required)")
	cmd.Flags().StringVar(&to, "to", "", "destination app: overcast (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without writing anything")
	cmd.Flags().BoolVar(&onlySubs, "only-subscriptions", false, "migrate subscriptions only, skip play state")
	cmd.Flags().BoolVar(&playState, "play-state", false, "also write episode play state (Podcasts→Overcast: uses unofficial web API, requires credentials; Overcast→Podcasts: writes to local SQLite database)")
	cmd.Flags().StringVar(&sqlitePath, "sqlite", "", "path to MTLibrary.sqlite (default: auto-detect)")
	cmd.Flags().StringVar(&opmlFallback, "opml-fallback", "", "path to Apple Podcasts OPML export (fallback if SQLite unavailable)")
	cmd.Flags().StringVar(&overcastExport, "overcast-export", "", "path to Overcast OPML export (for reading Overcast data or play state matching)")
	cmd.Flags().StringVar(&overcastOut, "overcast-out", "", "path for the generated Overcast import OPML file")
	cmd.Flags().StringVar(&overcastEmail, "overcast-email", "", "Overcast account email (or set OVERCAST_EMAIL env var)")
	cmd.Flags().StringVar(&overcastPassword, "overcast-password", "", "Overcast account password (or set OVERCAST_PASSWORD env var)")
	cmd.Flags().StringVar(&conflictStrategy, "conflict", "furthest", "conflict resolution: furthest | source | target")
	cmd.Flags().DurationVar(&requestDelay, "request-delay", overcast.DefaultRequestDelay, "delay between consecutive Overcast API requests (increase if you hit 429 rate limits)")
	cmd.Flags().StringArrayVar(&podcastFilter, "podcast", nil, "limit play-state sync to podcasts whose title contains this word/phrase (case-insensitive, repeatable)")
	cmd.Flags().StringVar(&podcastListFile, "podcast-list", "", "path to a file with one podcast title/word per line; combined with --podcast")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

// buildPodcastFilter merges CLI patterns and a list file into a single deduplicated
// slice of lowercase filter strings. Returns an error if the file cannot be read.
func buildPodcastFilter(cliPatterns []string, listFile string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string

	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	for _, p := range cliPatterns {
		add(p)
	}

	if listFile != "" {
		f, err := os.Open(listFile)
		if err != nil {
			return nil, fmt.Errorf("--podcast-list: open %s: %w", listFile, err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			add(sc.Text())
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("--podcast-list: read %s: %w", listFile, err)
		}
	}

	return out, nil
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

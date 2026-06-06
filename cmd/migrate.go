package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
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
		overcastSourceOPML string
		overcastMatchOPML  string
		overcastOut      string
		overcastEmail    string
		overcastPassword string
		conflictStrategy        string
		requestDelay            time.Duration
		titleMatchTolerance     time.Duration
		podcastFilter           []string // --podcast (repeatable)
		podcastListFile         string   // --podcast-list (file path)
		logFile                 string   // --log-file (per-episode CSV log)
		appleBearerToken    string       // --apple-bearer-token / APPLE_BEARER_TOKEN
		appleMediaUserToken string       // --apple-media-user-token / APPLE_MEDIA_USER_TOKEN
		strictFeedMatch     bool          // --strict-feed-match
		forceUpdate         bool          // --force-update
		subscribedOnly      bool          // --subscribed-only
		episodeCacheMaxAge  time.Duration // --episode-cache-max-age
		clearEpisodeCache   bool          // --clear-episode-cache
		sinceStr            string        // --since (delta sync cutoff)
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
    --overcast-source-opml ~/Downloads/overcast.opml \
    --play-state
  # Credentials: set OVERCAST_EMAIL and OVERCAST_PASSWORD environment variables,
  # or pass --overcast-email and --overcast-password flags.

  # Sync play state for a single podcast (word match, case-insensitive)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-source-opml ~/Downloads/overcast.opml --play-state \
    --podcast "sistersinlaw"

  # Sync play state for podcasts listed in a file (one title/word per line)
  podcast-migrate migrate --from podcasts --to overcast \
    --overcast-source-opml ~/Downloads/overcast.opml --play-state \
    --podcast-list ~/my-podcasts.txt

  # Overcast → Podcasts (reverse sync via Apple web API — syncs to iPhone automatically)
  # Get tokens from podcasts.apple.com DevTools (mark any episode played → network tab)
  export APPLE_BEARER_TOKEN="eyJ..."
  export APPLE_MEDIA_USER_TOKEN="0.Apg..."
  podcast-migrate migrate --from overcast --to podcasts \
    --overcast-source-opml ~/Downloads/overcast.opml --play-state

  # Reverse sync — single podcast, dry-run first
  podcast-migrate migrate --from overcast --to podcasts \
    --overcast-source-opml ~/Downloads/overcast.opml --play-state \
    --podcast "sistersinlaw" --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve Overcast credentials from flags → env vars.
			if overcastEmail == "" {
				overcastEmail = os.Getenv("OVERCAST_EMAIL")
			}
			if overcastPassword == "" {
				overcastPassword = os.Getenv("OVERCAST_PASSWORD")
			}

			// Resolve Apple web API tokens from flags → env vars.
			if appleBearerToken == "" {
				appleBearerToken = os.Getenv("APPLE_BEARER_TOKEN")
			}
			if appleMediaUserToken == "" {
				appleMediaUserToken = os.Getenv("APPLE_MEDIA_USER_TOKEN")
			}

			// Direction-aware validation for --play-state:
			//   * --from overcast: always requires --overcast-source-opml (the source data)
			//   * --to overcast:   requires credentials; destination matching OPML is
			//                      either --overcast-match-opml or auto-fetched after login
			if playState {
				if from == "overcast" && overcastSourceOPML == "" {
					return fmt.Errorf("--play-state requires --overcast-source-opml when --from overcast " +
						"(path to your extended OPML from overcast.fm/account/export_opml/extended)")
				}
				if to == "overcast" && overcastEmail == "" {
					return fmt.Errorf("--play-state requires Overcast credentials when --to overcast: " +
						"set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email / --overcast-password")
				}
			}

			// Merge podcast filter patterns from --podcast flags and --podcast-list file.
			allFilters, err := buildPodcastFilter(podcastFilter, podcastListFile)
			if err != nil {
				return err
			}

			src, err := buildProvider(from, sqlitePath, opmlFallback, overcastSourceOPML, "", "", "")
			if err != nil {
				return fmt.Errorf("source: %w", err)
			}

			// --since: limit the Apple source to episodes modified after the cutoff.
			// Only applicable when the source is Apple Podcasts (SQLite); ignored otherwise.
			if sinceStr != "" {
				t, err := parseSince(sinceStr)
				if err != nil {
					return err
				}
				if ap, ok := src.(*apple.Provider); ok {
					ap.SetSinceTime(t)
				} else {
					fmt.Fprintf(os.Stderr, "warning: --since is only supported when --from podcasts; flag ignored\n")
				}
			}
			// Pass sqlitePath and opmlFallback for the destination too — needed when
			// the destination is Apple Podcasts (reverse sync: overcast → podcasts).
			dst, err := buildProvider(to, sqlitePath, opmlFallback, overcastSourceOPML, overcastOut, overcastEmail, overcastPassword)
			if err != nil {
				return fmt.Errorf("destination: %w", err)
			}

			// If an explicit destination matching OPML was provided, wire it into the
			// Overcast destination provider. Without this, the provider auto-fetches
			// the live account library after login.
			if overcastMatchOPML != "" {
				if op, ok := dst.(*overcast.Provider); ok {
					op.SetMatchOPMLPath(overcastMatchOPML)
				}
			}

			// If Apple web API credentials are provided, configure the destination
			// provider (or source, in case someone pipes apple→apple) to use the
			// web API for play state writes instead of direct SQLite manipulation.
			if appleBearerToken != "" && appleMediaUserToken != "" {
				if ap, ok := dst.(*apple.Provider); ok {
					ap.SetWebAPICredentials(appleBearerToken, appleMediaUserToken)
				}
			}

			opts := provider.WriteOptions{
				DryRun:            dryRun,
				OnlySubscriptions: onlySubs,
				// Note: OnlyPlayState is intentionally NOT set here. Setting it causes
				// merge() to skip the subscription union, which empties merged.Podcasts
				// and breaks feedToTitle (the episode→podcast title lookup used by
				// --podcast filters and cross-feed-URL episode matching).
				// The Apple Podcasts writer ignores subscriptions internally regardless.
				ConflictStrategy:        parseConflictStrategy(conflictStrategy),
				RequestDelay:            requestDelay,
				PodcastFilter:           allFilters,
				TitleMatchDateTolerance: titleMatchTolerance,
				StrictFeedMatch:         strictFeedMatch,
				ForceUpdate:             forceUpdate,
				SubscribedOnly:          subscribedOnly,
				EpisodeCacheMaxAge:      episodeCacheMaxAge,
				ClearEpisodeCache:       clearEpisodeCache,
			}

			if logFile != "" {
				f, err := os.Create(logFile)
				if err != nil {
					return fmt.Errorf("--log-file: %w", err)
				}
				defer f.Close()
				opts.LogWriter = f
			}

			engine := sync.New(src, dst)
			result, err := engine.Run(context.Background(), opts)
			if err != nil {
				return err
			}

			fmt.Println(result)

			if !dryRun && to == "overcast" {
				if overcastOut != "" {
					fmt.Printf("\nNext step: open Overcast > Settings > Import OPML and select:\n  %s\n", overcastOut)
				}
				if playState {
					fmt.Println("Play state has been written directly via the Overcast web API.")
				}
			}
			if !dryRun && (to == "podcasts" || to == "apple") && playState {
				fmt.Println("\nPlay state has been written via the Apple Podcasts web API.")
				fmt.Println("Episodes will sync to iPhone and all Apple devices automatically.")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source app: podcasts (required)")
	cmd.Flags().StringVar(&to, "to", "", "destination app: overcast (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without writing anything")
	cmd.Flags().BoolVar(&onlySubs, "only-subscriptions", false, "migrate subscriptions only, skip play state")
	cmd.Flags().BoolVar(&playState, "play-state", false, "also write episode play state (Podcasts→Overcast: uses unofficial web API, requires Overcast credentials; Overcast→Podcasts: uses Apple Podcasts web API when --apple-bearer-token/--apple-media-user-token are set, otherwise writes to local SQLite)")
	cmd.Flags().StringVar(&sqlitePath, "sqlite", "", "path to MTLibrary.sqlite (default: auto-detect)")
	cmd.Flags().StringVar(&opmlFallback, "opml-fallback", "", "path to Apple Podcasts OPML export (fallback if SQLite unavailable)")
	cmd.Flags().StringVar(&overcastSourceOPML, "overcast-source-opml", "", "path to Overcast extended OPML export used as the migration source (from overcast.fm/account/export_opml/extended)")
	cmd.Flags().StringVar(&overcastMatchOPML, "overcast-match-opml", "", "path to Overcast OPML used for destination episode matching when writing play state (optional; if omitted and credentials are set, the live account library is fetched automatically)")
	cmd.Flags().StringVar(&overcastOut, "overcast-out", "", "path for the generated Overcast import OPML file")
	cmd.Flags().StringVar(&overcastEmail, "overcast-email", "", "Overcast account email (or set OVERCAST_EMAIL env var)")
	cmd.Flags().StringVar(&overcastPassword, "overcast-password", "", "Overcast account password (or set OVERCAST_PASSWORD env var)")
	cmd.Flags().StringVar(&conflictStrategy, "conflict", "furthest", "conflict resolution: furthest | source | target")
	cmd.Flags().DurationVar(&requestDelay, "request-delay", 0, "delay between consecutive API requests to Overcast or Apple (default 500ms for both; increase if you hit 429 rate limits)")
	cmd.Flags().DurationVar(&titleMatchTolerance, "title-match-tolerance", 72*time.Hour,
		"max pub-date gap allowed when matching episodes by title (strategies 2 & 4 fallback);\n"+
			"prevents false matches between same-named episodes published years apart;\n"+
			"set to 0 to disable the guard and accept any date (legacy behaviour)")
	cmd.Flags().StringArrayVar(&podcastFilter, "podcast", nil, "limit play-state sync to podcasts whose title contains this word/phrase (case-insensitive, repeatable)")
	cmd.Flags().StringVar(&podcastListFile, "podcast-list", "", "path to a file with one podcast title/word per line; combined with --podcast")
	cmd.Flags().StringVar(&logFile, "log-file", "", "write per-episode detail to this CSV file (columns: status, podcast, episode, pub_date, source_state, target_state, note)")
	cmd.Flags().StringVar(&appleBearerToken, "apple-bearer-token", "", "Apple Podcasts web API Bearer token (or set APPLE_BEARER_TOKEN); obtain from podcasts.apple.com DevTools → mark episode played → Authorization header")
	cmd.Flags().StringVar(&appleMediaUserToken, "apple-media-user-token", "", "Apple Podcasts media-user-token (or set APPLE_MEDIA_USER_TOKEN); obtain from podcasts.apple.com DevTools → mark episode played → media-user-token header")
	cmd.Flags().BoolVar(&strictFeedMatch, "strict-feed-match", false, "only match episodes using feed-URL-anchored strategies (pub date or title + same feed URL); skips cross-feed title fallbacks (strategies 3 and 4)")
	cmd.Flags().BoolVar(&forceUpdate, "force-update", false, "write source play state even if the destination already shows the episode as played or further along; bypasses the server-state check")
	cmd.Flags().BoolVar(&subscribedOnly, "subscribed-only", false, "only sync play state for podcasts already subscribed to at the destination; skips search and subscribe for unsubscribed feeds")
	cmd.Flags().DurationVar(&episodeCacheMaxAge, "episode-cache-max-age", 0,
		"maximum age of cached Overcast episode numeric IDs; entries older than this are\n"+
			"re-fetched (e.g. 720h = 30 days); 0 means cached IDs are valid indefinitely")
	cmd.Flags().BoolVar(&clearEpisodeCache, "clear-episode-cache", false,
		"discard all cached Overcast episode numeric IDs before syncing and re-fetch\n"+
			"them from Overcast; the cache is repopulated with fresh data during the run")
	cmd.Flags().StringVar(&sinceStr, "since", "",
		"delta sync: only process Apple Podcasts episodes whose play state changed after\n"+
			"this cutoff. Accepts a duration (e.g. 24h, 7d) or a date (e.g. 2026-06-01).\n"+
			"Only effective when --from podcasts.")

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
		if overcastImport == "" && overcastOut == "" && overcastEmail == "" {
			return nil, fmt.Errorf("overcast requires at least one of: --overcast-source-opml (read), --overcast-out (write), or credentials (--overcast-email/--overcast-password for play-state write)")
		}
		if overcastEmail != "" {
			return overcast.NewProviderWithCredentials(overcastImport, overcastOut, overcastEmail, overcastPassword), nil
		}
		return overcast.NewProvider(overcastImport, overcastOut), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (supported: podcasts, overcast)", name)
	}
}

// parseSince parses a --since value into an absolute time.
// Accepted forms:
//
//	"7d", "30d"         — N days ago (d suffix, not a standard Go duration)
//	"24h", "1h30m"      — standard Go duration, subtracted from now
//	"2026-06-01"        — date (local midnight)
//	"2026-06-01T15:04"  — date + time (local)
//	"2026-06-01T15:04:05Z07:00" — RFC3339
func parseSince(s string) (time.Time, error) {
	// Day-suffix shorthand: "7d", "30d"
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err == nil && n > 0 {
			return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	// Standard Go duration: "24h", "1h30m", etc.
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--since: duration must be positive, got %q", s)
		}
		return time.Now().Add(-d), nil
	}
	// Date / datetime layouts (local timezone unless explicit offset).
	for _, layout := range []string{
		"2006-01-02",
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		time.RFC3339,
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"--since: cannot parse %q — use a duration (e.g. 24h, 7d) or a date (e.g. 2026-06-01)", s)
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

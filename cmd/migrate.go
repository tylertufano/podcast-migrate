package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tyler/podcast-migrate/internal/apple"
	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/opml"
	"github.com/tyler/podcast-migrate/internal/overcast"
	"github.com/tyler/podcast-migrate/internal/pocketcasts"
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
		overcastSourceOPML       string
		overcastMatchOPML        string
		overcastOut              string
		overcastEmail            string
		overcastPassword         string
		overcastClearSourceCache bool   // --clear-source-opml-cache
		overcastSaveSourceOPML   string // --overcast-save-source-opml
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
		pocketcastsEmail        string   // --pocketcasts-email / POCKETCASTS_EMAIL
		pocketcastsPassword     string   // --pocketcasts-password / POCKETCASTS_PASSWORD
		pcIncludeUnsubscribed   bool     // --pc-include-unsubscribed
		feedMapPairs            []string // --feed-map (repeatable, "SRC_URL=DST_URL")
		opmlFile            string        // --opml-file (source OPML path when --from opml)
		opmlOut             string        // --opml-out (output OPML path when --to opml)
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

			// Resolve Pocket Casts credentials from flags → env vars.
			if pocketcastsEmail == "" {
				pocketcastsEmail = os.Getenv("POCKETCASTS_EMAIL")
			}
			if pocketcastsPassword == "" {
				pocketcastsPassword = os.Getenv("POCKETCASTS_PASSWORD")
			}

			// Direction-aware validation for --play-state:
			//   * --from overcast: requires --overcast-source-opml OR credentials for auto-fetch
			//   * --to overcast:   requires credentials; destination matching OPML is
			//                      either --overcast-match-opml or auto-fetched after login
			//   * --from/to pocketcasts: requires Pocket Casts credentials
			if playState {
				if from == "overcast" && overcastSourceOPML == "" && overcastEmail == "" {
					return fmt.Errorf("--play-state with --from overcast requires either " +
						"--overcast-source-opml (path to your extended OPML export) " +
						"or Overcast credentials (OVERCAST_EMAIL + OVERCAST_PASSWORD / " +
						"--overcast-email / --overcast-password) for automatic fetch")
				}
				if to == "overcast" && overcastEmail == "" {
					return fmt.Errorf("--play-state requires Overcast credentials when --to overcast: " +
						"set OVERCAST_EMAIL and OVERCAST_PASSWORD, or use --overcast-email / --overcast-password")
				}
				if (from == "pocketcasts" || from == "pc") && pocketcastsEmail == "" {
					return fmt.Errorf("--play-state requires Pocket Casts credentials when --from pocketcasts: " +
						"set POCKETCASTS_EMAIL and POCKETCASTS_PASSWORD, or use --pocketcasts-email / --pocketcasts-password")
				}
				if (to == "pocketcasts" || to == "pc") && pocketcastsEmail == "" {
					return fmt.Errorf("--play-state requires Pocket Casts credentials when --to pocketcasts: " +
						"set POCKETCASTS_EMAIL and POCKETCASTS_PASSWORD, or use --pocketcasts-email / --pocketcasts-password")
				}
			}

			// Merge podcast filter patterns from --podcast flags and --podcast-list file.
			allFilters, err := buildPodcastFilter(podcastFilter, podcastListFile)
			if err != nil {
				return err
			}

			// Parse feed URL mappings from --feed-map flags.
			feedMap, err := buildFeedMap(feedMapPairs)
			if err != nil {
				return err
			}

			// Pass Overcast credentials to the source provider so it can auto-fetch
			// the extended OPML when --overcast-source-opml is not specified.
			src, err := buildProvider(from, sqlitePath, opmlFallback, overcastSourceOPML, "", overcastEmail, overcastPassword, pocketcastsEmail, pocketcastsPassword, opmlFile, "")
			if err != nil {
				return fmt.Errorf("source: %w", err)
			}

			// Wire auto-fetch options into the Overcast source provider.
			if op, ok := src.(*overcast.Provider); ok {
				if overcastClearSourceCache {
					op.SetClearSourceOPMLCache(true)
				}
				if overcastSaveSourceOPML != "" {
					op.SetSaveSourceOPMLPath(overcastSaveSourceOPML)
				}
			}

			// --pc-include-unsubscribed: include play history for podcasts the user is
			// no longer subscribed to in Pocket Casts (off by default).
			if pcIncludeUnsubscribed {
				if pcProv, ok := src.(*pocketcasts.Provider); ok {
					pcProv.IncludeUnsubscribed = true
				}
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
			dst, err := buildProvider(to, sqlitePath, opmlFallback, overcastSourceOPML, overcastOut, overcastEmail, overcastPassword, pocketcastsEmail, pocketcastsPassword, "", opmlOut)
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

			// Configure write credentials for Apple Podcasts destination.
			// Web API (bearer + media-user-token) is preferred: catalog episodes
			// resolve immediately without needing local indexing. If those tokens
			// are absent, fall back to KVS-only mode (all episodes via KVS),
			// which requires Apple Podcasts to index each feed first.
			if ap, ok := dst.(*apple.Provider); ok {
				if appleBearerToken != "" && appleMediaUserToken != "" {
					ap.SetWebAPICredentials(appleBearerToken, appleMediaUserToken)
				} else {
					// KVS-only mode: silently succeeds (prints its own banner) or
					// silently fails (error surfaces at write time if --play-state).
					_ = ap.SetKVSOnlyMode()
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
				FeedMap:                 feedMap,
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
			if !dryRun && (to == "pocketcasts" || to == "pc") && playState {
				fmt.Println("Play state has been written via the Pocket Casts web API.")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "source app: podcasts, overcast, pocketcasts (required)")
	cmd.Flags().StringVar(&to, "to", "", "destination app: overcast, pocketcasts, podcasts (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without writing anything")
	cmd.Flags().BoolVar(&onlySubs, "only-subscriptions", false, "migrate subscriptions only, skip play state")
	cmd.Flags().BoolVar(&playState, "play-state", false, "also write episode play state (Podcasts→Overcast: uses unofficial web API, requires Overcast credentials; Overcast→Podcasts: uses Apple Podcasts web API when --apple-bearer-token/--apple-media-user-token are set, otherwise writes to local SQLite)")
	cmd.Flags().StringVar(&sqlitePath, "sqlite", "", "path to MTLibrary.sqlite (default: auto-detect)")
	cmd.Flags().StringVar(&opmlFallback, "opml-fallback", "", "path to Apple Podcasts OPML export (fallback if SQLite unavailable)")
	cmd.Flags().StringVar(&overcastSourceOPML, "overcast-source-opml", "", "path to Overcast extended OPML export used as the migration source;\n"+
		"optional when Overcast credentials are set — the extended OPML is fetched automatically\n"+
		"and cached for 24 h (see --clear-source-opml-cache)")
	cmd.Flags().StringVar(&overcastMatchOPML, "overcast-match-opml", "", "path to Overcast OPML used for destination episode matching when writing play state (optional; if omitted and credentials are set, the live account library is fetched automatically)")
	cmd.Flags().StringVar(&overcastOut, "overcast-out", "", "path for the generated Overcast import OPML file")
	cmd.Flags().StringVar(&overcastEmail, "overcast-email", "", "Overcast account email (or set OVERCAST_EMAIL env var)")
	cmd.Flags().StringVar(&overcastPassword, "overcast-password", "", "Overcast account password (or set OVERCAST_PASSWORD env var)")
	cmd.Flags().BoolVar(&overcastClearSourceCache, "clear-source-opml-cache", false,
		"discard the cached Overcast source OPML and force a fresh download;\n"+
			"only effective when --from overcast without --overcast-source-opml")
	cmd.Flags().StringVar(&overcastSaveSourceOPML, "overcast-save-source-opml", "",
		"save a copy of the fetched Overcast source OPML to this path;\n"+
			"if the flag is given without a value, ~/Downloads/overcast.opml is used")
	cmd.Flags().Lookup("overcast-save-source-opml").NoOptDefVal = filepath.Join(os.Getenv("HOME"), "Downloads", "overcast.opml")
	cmd.Flags().StringVar(&conflictStrategy, "conflict", "furthest", "conflict resolution: furthest | source | target")
	cmd.Flags().DurationVar(&requestDelay, "request-delay", 0, "delay between consecutive API requests to Overcast or Apple (default 1s; increase if you hit 429 rate limits)")
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
	cmd.Flags().BoolVar(&forceUpdate, "force-update", false, "write source play state even if the destination already shows the episode as played or further along; bypasses the server-state check. Note: has no effect when --to pocketcasts — Pocket Casts does not expose a reliable per-episode read-back API, so the tool always writes unconditionally regardless of this flag")
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
	cmd.Flags().StringVar(&pocketcastsEmail, "pocketcasts-email", "",
		"Pocket Casts account email (or set POCKETCASTS_EMAIL env var)")
	cmd.Flags().StringVar(&pocketcastsPassword, "pocketcasts-password", "",
		"Pocket Casts account password (or set POCKETCASTS_PASSWORD env var)")
	cmd.Flags().BoolVar(&pcIncludeUnsubscribed, "pc-include-unsubscribed", false,
		"when --from pocketcasts: also export play history for podcasts no longer subscribed to;\n"+
			"the feed URL is recovered via the Pocket Casts CDN or iTunes Search API")
	cmd.Flags().StringArrayVar(&feedMapPairs, "feed-map", nil,
		"map a source subscriber feed URL to a destination analog feed URL\n"+
			"(format: SRC_URL=DST_URL; repeatable). Use when you have already\n"+
			"subscribed to a private/subscriber feed on the destination platform\n"+
			"that corresponds to an Apple subscriber feed in the source. Episodes\n"+
			"from SRC_URL are matched against DST_URL instead of the public feed,\n"+
			"without requiring --subscribed-only.\n"+
			"Example: --feed-map 'https://private.apple.feed/abc=https://private.target.feed/xyz'")
	cmd.Flags().StringVar(&opmlFile, "opml-file", "",
		"path to source OPML file (required when --from opml);\n"+
			"supports standard OPML (subscriptions) and extended OPML with episode play state\n"+
			"(compatible with Overcast extended export from overcast.fm/account/export_opml/extended)")
	cmd.Flags().StringVar(&opmlOut, "opml-out", "",
		"path for the generated OPML output file (required when --to opml);\n"+
			"writes extended OPML with episode play state when --play-state is set,\n"+
			"subscriptions only otherwise (compatible with any standard OPML importer)")

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

// buildFeedMap parses a slice of "SRC_URL=DST_URL" strings into the map
// expected by provider.WriteOptions.FeedMap. Both URLs are normalised via
// NormalizeFeedURL so that http/https differences and trailing-slash
// variations are treated as equivalent. Returns an error if any pair is
// malformed.
func buildFeedMap(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		idx := strings.Index(pair, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("--feed-map %q: expected format SRC_URL=DST_URL (missing '=')", pair)
		}
		src := strings.TrimSpace(pair[:idx])
		dst := strings.TrimSpace(pair[idx+1:])
		if src == "" || dst == "" {
			return nil, fmt.Errorf("--feed-map %q: both source and destination URLs must be non-empty", pair)
		}
		m[migrate.NormalizeFeedURL(src)] = migrate.NormalizeFeedURL(dst)
	}
	return m, nil
}

func buildProvider(name, sqlitePath, opmlFallback, overcastImport, overcastOut, overcastEmail, overcastPassword, pcEmail, pcPassword, opmlSourceFile, opmlOutputFile string) (provider.Provider, error) {
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
	case "pocketcasts", "pc":
		if pcEmail == "" {
			return nil, fmt.Errorf("pocketcasts requires credentials: set POCKETCASTS_EMAIL and POCKETCASTS_PASSWORD, or use --pocketcasts-email / --pocketcasts-password")
		}
		return pocketcasts.NewProvider(pcEmail, pcPassword), nil
	case "opml":
		if opmlSourceFile != "" {
			return opml.NewSourceProvider(opmlSourceFile), nil
		}
		if opmlOutputFile != "" {
			// Extended OPML (with play state) is always written; SetLibrary respects
			// opts.OnlySubscriptions to omit episode outlines when not migrating play state.
			return opml.NewOutputProvider(opmlOutputFile, true), nil
		}
		return nil, fmt.Errorf("opml requires --opml-file (source) or --opml-out (destination)")
	default:
		return nil, fmt.Errorf("unknown provider %q (supported: podcasts, apple, overcast, pocketcasts, opml)", name)
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

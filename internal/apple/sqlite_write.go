package apple

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
	_ "modernc.org/sqlite"
)

// SQLiteWriter writes episode play state back to the Apple Podcasts SQLite
// database (MTLibrary.sqlite). It matches incoming episodes by feed URL +
// publication date (primary) or feed URL + normalised title (fallback),
// then updates the four relevant columns: ZPLAYSTATE, ZPLAYHEAD, ZPLAYCOUNT,
// and ZLASTDATEPLAYED.
//
// Safety constraints:
//   - Only existing ZMTEPISODE rows are updated; no new rows are inserted.
//   - The database is opened in WAL mode to reduce lock contention.
//   - Closing Apple Podcasts before running is strongly recommended.
type SQLiteWriter struct {
	dbPath string
}

// NewSQLiteWriter returns a writer targeting the given SQLite database path.
func NewSQLiteWriter(dbPath string) *SQLiteWriter {
	return &SQLiteWriter{dbPath: dbPath}
}

// appleEpisodeRecord holds the data needed to match and conditionally update
// a single ZMTEPISODE row.
type appleEpisodeRecord struct {
	pk           int64
	feedURL      string
	guid         string
	title        string
	pubDate      time.Time
	playState    model.PlayState
	playPosition time.Duration
}

// Write applies episode play state from lib to the Apple Podcasts SQLite database,
// respecting opts.ConflictStrategy and opts.PodcastFilter.
// Returns the number of rows actually updated.
func (w *SQLiteWriter) Write(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	if opts.DryRun {
		return w.dryRun(ctx, lib, opts)
	}

	// Warn the user: writing to Apple Podcasts' live database while the app is
	// open can cause CoreData conflicts. It is not unsafe under WAL mode, but
	// changes may not be visible until Apple Podcasts re-reads the database.
	fmt.Println("apple: writing play state to local SQLite database")
	fmt.Println("  note: for best results, quit Apple Podcasts before running this command")

	db, err := sql.Open("sqlite", "file:"+w.dbPath+"?_journal=wal")
	if err != nil {
		return 0, fmt.Errorf("apple/sqlite-write: open %s: %w", w.dbPath, err)
	}
	defer db.Close()

	// Build a feed-keyed index of all episodes currently in Apple's DB.
	index, err := w.buildAppleIndex(ctx, db)
	if err != nil {
		return 0, err
	}

	// Filter episodes to the requested podcasts.
	feedToTitle := buildFeedToTitleFromLib(lib)
	episodes := filterLibraryEpisodes(lib.Episodes, feedToTitle, opts.PodcastFilter)
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("apple: podcast filter active — %q — %d/%d episode(s) in scope\n",
			opts.PodcastFilter, len(episodes), len(lib.Episodes))
	}

	updated := 0
	skipped := 0
	notFound := 0

	for _, ep := range episodes {
		if ep.PlayState == model.PlayStateUnplayed {
			continue
		}
		if ep.FeedURL == "" {
			continue
		}

		appleRec, ok := findInAppleIndex(index, ep)
		if !ok {
			notFound++
			continue
		}

		if appleSatisfied(ep, appleRec, opts.ConflictStrategy) {
			skipped++
			continue
		}

		if err := w.updateEpisodePlayState(ctx, db, appleRec.pk, ep); err != nil {
			fmt.Printf("  warning: could not update %q (pk=%d): %v\n", ep.Title, appleRec.pk, err)
			continue
		}
		updated++
	}

	if skipped > 0 {
		fmt.Printf("apple: skipping %d episode(s) already at desired state\n", skipped)
	}
	if notFound > 0 {
		fmt.Printf("apple: %d episode(s) not found in Apple Podcasts database (not downloaded or streamed)\n", notFound)
	}

	return updated, nil
}

// dryRun reports what Write would do without touching the database.
func (w *SQLiteWriter) dryRun(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	db, err := sql.Open("sqlite", "file:"+w.dbPath+"?mode=ro&_journal=off")
	if err != nil {
		return 0, fmt.Errorf("apple/sqlite-write: open (dry-run) %s: %w", w.dbPath, err)
	}
	defer db.Close()

	index, err := w.buildAppleIndex(ctx, db)
	if err != nil {
		return 0, err
	}

	feedToTitle := buildFeedToTitleFromLib(lib)
	episodes := filterLibraryEpisodes(lib.Episodes, feedToTitle, opts.PodcastFilter)

	n := 0
	for _, ep := range episodes {
		if ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		appleRec, ok := findInAppleIndex(index, ep)
		if !ok {
			continue
		}
		if !appleSatisfied(ep, appleRec, opts.ConflictStrategy) {
			n++
		}
	}
	return n, nil
}

// buildAppleIndex queries ZMTEPISODE (joined to ZMTPODCAST) and returns
// a map keyed by both feeddate and feedtitle match keys.
// Only episodes on subscribed, publicly-accessible feeds are included.
func (w *SQLiteWriter) buildAppleIndex(ctx context.Context, db *sql.DB) (map[string]appleEpisodeRecord, error) {
	const q = `
		SELECT
			e.Z_PK,
			e.ZGUID,
			p.ZFEEDURL,
			e.ZTITLE,
			e.ZPUBDATE,
			e.ZPLAYHEAD,
			e.ZPLAYSTATE
		FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE p.ZSUBSCRIBED = 1
		  AND p.ZFEEDURL IS NOT NULL
		  AND (p.ZFEEDURL LIKE 'http://%' OR p.ZFEEDURL LIKE 'https://%')`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("apple/sqlite-write: query episodes: %w", err)
	}
	defer rows.Close()

	index := make(map[string]appleEpisodeRecord)

	for rows.Next() {
		var (
			pk          int64
			guid        sql.NullString
			feedURL     string
			title       string
			pubDateRaw  sql.NullFloat64
			playHeadSec sql.NullFloat64
			playStateSq sql.NullInt64
		)
		if err := rows.Scan(&pk, &guid, &feedURL, &title, &pubDateRaw, &playHeadSec, &playStateSq); err != nil {
			return nil, fmt.Errorf("apple/sqlite-write: scan: %w", err)
		}

		rec := appleEpisodeRecord{
			pk:      pk,
			feedURL: feedURL,
			title:   title,
		}
		if guid.Valid {
			rec.guid = guid.String
		}
		if pubDateRaw.Valid {
			rec.pubDate = coreDataEpoch.Add(time.Duration(pubDateRaw.Float64 * float64(time.Second)))
		}
		if playHeadSec.Valid && playHeadSec.Float64 > 0 {
			rec.playPosition = time.Duration(playHeadSec.Float64 * float64(time.Second))
			rec.playState = model.PlayStateInProgress
		} else if (playStateSq.Valid && playStateSq.Int64 != 0) || (playHeadSec.Valid && playHeadSec.Float64 == 0) {
			// Non-zero ZPLAYSTATE with no playhead means explicitly played.
			// We can't distinguish played from unplayed purely from ZPLAYSTATE without
			// also checking ZPLAYCOUNT/ZLASTDATEPLAYED, but for conflict resolution
			// we are conservative: if ZPLAYSTATE=0 and ZPLAYHEAD=0, treat as unplayed.
			if playStateSq.Valid && playStateSq.Int64 != 0 {
				rec.playState = model.PlayStatePlayed
			}
		}

		normFeed := normalizeWriteFeedURL(feedURL)

		// Primary key: feedURL + pubDate (RFC3339 UTC, second precision).
		if !rec.pubDate.IsZero() {
			key := feedDateKey(normFeed, rec.pubDate)
			if _, exists := index[key]; !exists {
				index[key] = rec
			}
		}

		// Fallback key: feedURL + normalised title.
		if title != "" {
			key := feedTitleKey(normFeed, title)
			if _, exists := index[key]; !exists {
				index[key] = rec
			}
		}
	}
	return index, rows.Err()
}

// updateEpisodePlayState issues a single UPDATE on the ZMTEPISODE row identified
// by pk, writing the ZPLAYSTATE, ZPLAYHEAD, ZPLAYCOUNT, and ZLASTDATEPLAYED columns.
func (w *SQLiteWriter) updateEpisodePlayState(ctx context.Context, db *sql.DB, pk int64, ep model.EpisodeState) error {
	now := time.Now().UTC()
	nowCD := toCoreData(now)

	var playStateVal int64
	var playHeadVal float64
	var playCountExpr string

	switch ep.PlayState {
	case model.PlayStatePlayed:
		playStateVal = applePlayStatePlayed
		playHeadVal = 0
		playCountExpr = "MAX(COALESCE(ZPLAYCOUNT, 0), 1)"
	case model.PlayStateInProgress:
		playStateVal = applePlayStateInProgress
		playHeadVal = ep.PlayPosition.Seconds()
		playCountExpr = "COALESCE(ZPLAYCOUNT, 0)"
	default:
		return nil // nothing to write for unplayed
	}

	query := fmt.Sprintf(`
		UPDATE ZMTEPISODE
		SET ZPLAYSTATE      = ?,
		    ZPLAYHEAD       = ?,
		    ZPLAYCOUNT      = %s,
		    ZLASTDATEPLAYED = ?
		WHERE Z_PK = ?`, playCountExpr)

	_, err := db.ExecContext(ctx, query, playStateVal, playHeadVal, nowCD, pk)
	if err != nil {
		return fmt.Errorf("UPDATE Z_PK=%d: %w", pk, err)
	}
	return nil
}

// Apple Podcasts ZPLAYSTATE column values observed in the MTLibrary.sqlite schema:
//   0 = unplayed (default)
//   1 = in-progress (ZPLAYHEAD > 0 confirms partial listening)
//   2 = fully played (ZPLAYHEAD = 0)
//
// We write the same values Apple Podcasts itself uses so the UI reflects the
// correct state immediately after the database is reloaded.
const (
	applePlayStateUnplayed   = 0
	applePlayStateInProgress = 1
	applePlayStatePlayed     = 2
)

// findInAppleIndex looks up an episode in the Apple index by feed+pubDate then
// feed+title. Returns the record and whether a match was found.
func findInAppleIndex(index map[string]appleEpisodeRecord, ep model.EpisodeState) (appleEpisodeRecord, bool) {
	normFeed := normalizeWriteFeedURL(ep.FeedURL)
	if !ep.PubDate.IsZero() {
		if rec, ok := index[feedDateKey(normFeed, ep.PubDate)]; ok {
			return rec, true
		}
	}
	if ep.Title != "" {
		if rec, ok := index[feedTitleKey(normFeed, ep.Title)]; ok {
			return rec, true
		}
	}
	return appleEpisodeRecord{}, false
}

// appleSatisfied reports whether the Apple episode's current state already meets
// or exceeds the desired state, making an update unnecessary.
func appleSatisfied(desired model.EpisodeState, current appleEpisodeRecord, strategy provider.ConflictStrategy) bool {
	switch strategy {
	case provider.SourceWins:
		return false // always overwrite
	case provider.TargetWins:
		return true // never overwrite
	default: // FurthestWins
		switch desired.PlayState {
		case model.PlayStatePlayed:
			return current.playState == model.PlayStatePlayed
		case model.PlayStateInProgress:
			if current.playState == model.PlayStatePlayed {
				return true // Apple already further
			}
			if current.playState == model.PlayStateInProgress {
				return current.playPosition >= desired.PlayPosition
			}
		}
		return false
	}
}

// buildFeedToTitleFromLib returns a map from feed URL to lowercased podcast
// title from the library's Podcasts slice.
func buildFeedToTitleFromLib(lib *model.Library) map[string]string {
	m := make(map[string]string, len(lib.Podcasts))
	for _, pod := range lib.Podcasts {
		if pod.FeedURL != "" {
			m[pod.FeedURL] = strings.ToLower(strings.TrimSpace(pod.Title))
		}
	}
	return m
}

// filterLibraryEpisodes is the Apple-side equivalent of filterEpisodesByPodcast.
// Returns only episodes from podcasts matching at least one filter pattern.
// If filters is empty, all episodes are returned.
func filterLibraryEpisodes(episodes []model.EpisodeState, feedToTitle map[string]string, filters []string) []model.EpisodeState {
	if len(filters) == 0 {
		return episodes
	}
	lower := make([]string, len(filters))
	for i, f := range filters {
		lower[i] = strings.ToLower(strings.TrimSpace(f))
	}
	var out []model.EpisodeState
	for _, ep := range episodes {
		title := feedToTitle[ep.FeedURL]
		for _, f := range lower {
			if f != "" && strings.Contains(title, f) {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

// toCoreData converts a Go time.Time to Apple's CoreData epoch offset (seconds
// since 2001-01-01 UTC).
func toCoreData(t time.Time) float64 {
	return t.UTC().Sub(coreDataEpoch).Seconds()
}

// normalizeWriteFeedURL produces a canonical feed URL key for matching,
// identical to the normalization applied on the Overcast side:
//   - lowercase scheme and host
//   - http → https
//   - strip trailing slash
//   - drop fragment
func normalizeWriteFeedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	u.Fragment = ""
	return u.String()
}

func feedDateKey(normFeedURL string, pubDate time.Time) string {
	return "feeddate:" + normFeedURL + "|" + pubDate.UTC().Format(time.RFC3339)
}

func feedTitleKey(normFeedURL, title string) string {
	return "feedtitle:" + normFeedURL + "|" + strings.ToLower(strings.TrimSpace(title))
}

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
// database (MTLibrary.sqlite).
//
// Episode matching uses a two-tier approach to handle the common case where
// Overcast and Apple Podcasts have subscribed to the same show via different
// feed URLs (different CDNs, http vs https redirect, etc.):
//
//  1. Primary (same feed URL): feedURL+pubDate, then feedURL+episodeTitle
//  2. Secondary (same podcast title, any feed URL): podcastTitle+pubDate,
//     then podcastTitle+episodeTitle
//
// The secondary tier lets the writer match episodes even when the two apps
// diverged on the canonical feed URL for the same show.
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
	podcastTitle string // normalised podcast title from ZMTPODCAST.ZTITLE
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
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("apple: podcast filter active — limiting to podcasts matching %q\n", opts.PodcastFilter)
	}
	episodes := filterLibraryEpisodes(lib.Episodes, feedToTitle, opts.PodcastFilter)

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

		appleRec, ok := findInAppleIndex(index, ep, feedToTitle)
		if !ok {
			notFound++
			continue
		}

		if applySatisfied(ep, appleRec, opts.ConflictStrategy) {
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
		fmt.Printf("apple: %d episode(s) already at desired state — skipped\n", skipped)
	}
	if notFound > 0 {
		fmt.Printf("apple: %d episode(s) not found in Apple Podcasts database (may not be downloaded/subscribed)\n", notFound)
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
	if len(opts.PodcastFilter) > 0 {
		fmt.Printf("apple: podcast filter active — limiting to podcasts matching %q\n", opts.PodcastFilter)
	}
	episodes := filterLibraryEpisodes(lib.Episodes, feedToTitle, opts.PodcastFilter)

	n := 0
	notFound := 0
	skipped := 0
	for _, ep := range episodes {
		if ep.PlayState == model.PlayStateUnplayed || ep.FeedURL == "" {
			continue
		}
		appleRec, ok := findInAppleIndex(index, ep, feedToTitle)
		if !ok {
			notFound++
			continue
		}
		if applySatisfied(ep, appleRec, opts.ConflictStrategy) {
			skipped++
			continue
		}
		n++
	}

	if notFound > 0 {
		fmt.Printf("apple: %d episode(s) not found in Apple Podcasts database (may not be downloaded/subscribed)\n", notFound)
	}
	if skipped > 0 {
		fmt.Printf("apple: %d episode(s) already at desired state — skipped\n", skipped)
	}

	return n, nil
}

// buildAppleIndex queries ZMTEPISODE (joined to ZMTPODCAST) and returns a map
// keyed by multiple match strategies. Four key types are built for each episode:
//
//   feeddate:<normFeedURL>|<RFC3339>       — primary: same feed URL, same pubDate
//   feedtitle:<normFeedURL>|<normTitle>    — fallback: same feed URL, same episode title
//   poddate:<normPodTitle>|<RFC3339>       — secondary: same podcast title, same pubDate
//   podtitle:<normPodTitle>|<normEpTitle>  — secondary fallback: same podcast + episode title
//
// The "pod" keys allow matching when Overcast and Apple Podcasts subscribed to
// the same show via different feed URLs.
func (w *SQLiteWriter) buildAppleIndex(ctx context.Context, db *sql.DB) (map[string]appleEpisodeRecord, error) {
	// Note: both p.ZTITLE and e.ZTITLE exist; alias them to avoid ambiguity.
	const q = `
		SELECT
			e.Z_PK,
			e.ZGUID,
			p.ZFEEDURL,
			p.ZTITLE AS PODCAST_TITLE,
			e.ZTITLE AS EPISODE_TITLE,
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
			pk             int64
			guid           sql.NullString
			feedURL        string
			podcastTitle   sql.NullString
			episodeTitle   sql.NullString
			pubDateRaw     sql.NullFloat64
			playHeadSec    sql.NullFloat64
			playStateSq    sql.NullInt64
		)
		if err := rows.Scan(&pk, &guid, &feedURL, &podcastTitle, &episodeTitle,
			&pubDateRaw, &playHeadSec, &playStateSq); err != nil {
			return nil, fmt.Errorf("apple/sqlite-write: scan: %w", err)
		}

		epTitle := ""
		if episodeTitle.Valid {
			epTitle = episodeTitle.String
		}
		podTitle := ""
		if podcastTitle.Valid {
			podTitle = podcastTitle.String
		}

		rec := appleEpisodeRecord{
			pk:           pk,
			feedURL:      feedURL,
			podcastTitle: strings.ToLower(strings.TrimSpace(podTitle)),
			title:        epTitle,
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
		} else if playStateSq.Valid && playStateSq.Int64 != 0 {
			// Non-zero ZPLAYSTATE with no playhead means explicitly played.
			rec.playState = model.PlayStatePlayed
		}

		normFeed := normalizeWriteFeedURL(feedURL)
		normPodTitle := rec.podcastTitle

		// --- Feed URL-based keys (primary) ---
		if !rec.pubDate.IsZero() {
			setIfAbsent(index, feedDateKey(normFeed, rec.pubDate), rec)
		}
		if epTitle != "" {
			setIfAbsent(index, feedTitleKey(normFeed, epTitle), rec)
		}

		// --- Podcast title-based keys (secondary, cross-feed-URL matching) ---
		if normPodTitle != "" {
			if !rec.pubDate.IsZero() {
				setIfAbsent(index, podDateKey(normPodTitle, rec.pubDate), rec)
			}
			if epTitle != "" {
				setIfAbsent(index, podTitleKey(normPodTitle, epTitle), rec)
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

// findInAppleIndex looks up an episode in the Apple index using a cascade of
// four match strategies:
//  1. feedURL + pubDate  (same feed, exact date)
//  2. feedURL + title    (same feed, same episode title)
//  3. podcastTitle + pubDate  (any feed for same podcast, exact date)
//  4. podcastTitle + title    (any feed for same podcast, same episode title)
//
// feedToTitle maps the incoming episode's feed URL to its lowercased podcast
// title (used for strategies 3 and 4 when the feed URL differs between apps).
func findInAppleIndex(index map[string]appleEpisodeRecord, ep model.EpisodeState, feedToTitle map[string]string) (appleEpisodeRecord, bool) {
	normFeed := normalizeWriteFeedURL(ep.FeedURL)

	// Strategy 1: same feed URL, same pubDate
	if !ep.PubDate.IsZero() {
		if rec, ok := index[feedDateKey(normFeed, ep.PubDate)]; ok {
			return rec, true
		}
	}
	// Strategy 2: same feed URL, same episode title
	if ep.Title != "" {
		if rec, ok := index[feedTitleKey(normFeed, ep.Title)]; ok {
			return rec, true
		}
	}

	// Strategies 3 & 4: cross-feed-URL matching via podcast title.
	// feedToTitle is keyed by the episode's feed URL and returns the lowercased
	// podcast title. The Apple index has the same podcast title (from ZMTPODCAST)
	// keyed as "poddate:" and "podtitle:" entries.
	podTitle := feedToTitle[ep.FeedURL] // already lowercased
	if podTitle == "" {
		return appleEpisodeRecord{}, false
	}

	// Strategy 3: same podcast title, same pubDate
	if !ep.PubDate.IsZero() {
		if rec, ok := index[podDateKey(podTitle, ep.PubDate)]; ok {
			return rec, true
		}
	}
	// Strategy 4: same podcast title, same episode title
	if ep.Title != "" {
		if rec, ok := index[podTitleKey(podTitle, ep.Title)]; ok {
			return rec, true
		}
	}

	return appleEpisodeRecord{}, false
}

// applySatisfied reports whether the Apple episode's current state already meets
// or exceeds the desired state, making an update unnecessary.
func applySatisfied(desired model.EpisodeState, current appleEpisodeRecord, strategy provider.ConflictStrategy) bool {
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

// filterLibraryEpisodes returns only episodes from podcasts whose title
// contains at least one filter pattern (case-insensitive substring match).
// If filters is empty, all episodes are returned unchanged.
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

// normalizeWriteFeedURL produces a canonical feed URL key for matching:
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

// setIfAbsent inserts rec into m under key only if key is not already present.
func setIfAbsent(m map[string]appleEpisodeRecord, key string, rec appleEpisodeRecord) {
	if _, exists := m[key]; !exists {
		m[key] = rec
	}
}

// Index key constructors.

func feedDateKey(normFeedURL string, pubDate time.Time) string {
	return "feeddate:" + normFeedURL + "|" + pubDate.UTC().Format(time.RFC3339)
}

func feedTitleKey(normFeedURL, title string) string {
	return "feedtitle:" + normFeedURL + "|" + strings.ToLower(strings.TrimSpace(title))
}

func podDateKey(normPodTitle string, pubDate time.Time) string {
	return "poddate:" + normPodTitle + "|" + pubDate.UTC().Format(time.RFC3339)
}

func podTitleKey(normPodTitle, epTitle string) string {
	return "podtitle:" + normPodTitle + "|" + strings.ToLower(strings.TrimSpace(epTitle))
}

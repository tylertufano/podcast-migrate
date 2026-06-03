package apple

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	_ "modernc.org/sqlite"
)

// CoreData stores timestamps as seconds since Jan 1, 2001.
var coreDataEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

const defaultDBPath = "243LU875E5.groups.com.apple.podcasts/Documents/MTLibrary.sqlite"

// DefaultSQLitePath returns the standard Apple Podcasts database location.
func DefaultSQLitePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Group Containers", defaultDBPath)
}

// SQLiteReader reads subscriptions and episode states from the Apple Podcasts
// local SQLite database (MTLibrary.sqlite).
type SQLiteReader struct {
	dbPath string
}

func NewSQLiteReader(dbPath string) *SQLiteReader {
	return &SQLiteReader{dbPath: dbPath}
}

func (r *SQLiteReader) Read(ctx context.Context) (*model.Library, error) {
	db, err := sql.Open("sqlite", "file:"+r.dbPath+"?mode=ro&_journal=off")
	if err != nil {
		return nil, fmt.Errorf("apple/sqlite: open: %w", err)
	}
	defer db.Close()

	podcasts, skippedPodcasts, err := r.readPodcasts(ctx, db)
	if err != nil {
		return nil, err
	}

	episodes, skippedEpisodes, err := r.readEpisodes(ctx, db)
	if err != nil {
		return nil, err
	}

	return &model.Library{
		Podcasts:                podcasts,
		Episodes:                episodes,
		ExportedAt:              time.Now(),
		SourceProvider:          "Apple Podcasts (SQLite)",
		PaywalledEpisodesIncluded: skippedEpisodes,
		SkippedInternalPodcasts: skippedPodcasts,
	}, nil
}

func (r *SQLiteReader) readPodcasts(ctx context.Context, db *sql.DB) ([]model.Podcast, int, error) {
	// Restrict to publicly subscribable http/https feeds.
	//
	// Excluded feed patterns (counted in skippedPodcasts):
	//   "internal://" — Apple-exclusive shows with no public RSS feed.
	//   "%/eyJ%" — Feeds whose URL path contains a JWT authentication token
	//       (base64-encoded JSON; "eyJ" decodes to '{"'). Several podcast
	//       subscription platforms embed a user-specific signed token directly
	//       in the feed URL (NPR Plus, Slate Plus via supportingcast.fm, etc.).
	//       These URLs are Apple-account-specific and fail to validate in any
	//       other app — Overcast's OPML importer aborts the entire import batch
	//       when it encounters them. Search for these shows by name in Overcast
	//       to subscribe via their public feeds.
	const q = `
		SELECT ZFEEDURL, ZTITLE, ZAUTHOR, ZIMAGEURL
		FROM ZMTPODCAST
		WHERE ZSUBSCRIBED = 1
		  AND (ZFEEDURL LIKE 'http://%' OR ZFEEDURL LIKE 'https://%')
		  AND ZFEEDURL NOT LIKE '%/eyJ%'
		ORDER BY ZTITLE`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("apple/sqlite: query podcasts: %w", err)
	}
	defer rows.Close()

	var out []model.Podcast
	for rows.Next() {
		var p model.Podcast
		var imageURL sql.NullString
		if err := rows.Scan(&p.FeedURL, &p.Title, &p.Author, &imageURL); err != nil {
			return nil, 0, fmt.Errorf("apple/sqlite: scan podcast: %w", err)
		}
		if imageURL.Valid {
			p.ImageURL = imageURL.String
		}
		// Strip Apple-added cache-buster parameters (e.g. ?t=1234567890) that
		// are appended to feed URLs to force a refresh. They are not part of the
		// canonical feed URL and can break feed resolution in other apps.
		p.FeedURL = cleanFeedURL(p.FeedURL)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	const countSkipped = `
		SELECT COUNT(*) FROM ZMTPODCAST
		WHERE ZSUBSCRIBED = 1
		  AND ZFEEDURL IS NOT NULL
		  AND (
		        ZFEEDURL NOT LIKE 'http://%' AND ZFEEDURL NOT LIKE 'https://%'
		        OR ZFEEDURL LIKE '%/eyJ%'
		      )`

	var skipped int
	_ = db.QueryRowContext(ctx, countSkipped).Scan(&skipped)

	return out, skipped, nil
}

// cleanFeedURL removes transient query parameters that Apple Podcasts appends to
// feed URLs (currently "t", a millisecond timestamp used as a cache-buster).
// These parameters are not part of the canonical feed URL and cause failures when
// other apps try to subscribe.
func cleanFeedURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.RawQuery == "" {
		return rawURL
	}
	q := u.Query()
	dirty := false
	for _, key := range []string{"t", "_t"} {
		if q.Has(key) {
			q.Del(key)
			dirty = true
		}
	}
	if !dirty {
		return rawURL
	}
	u.RawQuery = q.Encode()
	// If stripping left an empty query string, remove the trailing "?".
	result := u.String()
	return strings.TrimSuffix(result, "?")
}

func (r *SQLiteReader) readEpisodes(ctx context.Context, db *sql.DB) ([]model.EpisodeState, int, error) {
	// Fetch all episodes with any evidence of having been played. Apple Podcasts
	// uses several fields to track play history — no single column is authoritative:
	//
	//   ZPLAYSTATE != 0  — explicit state set by the app (1=in-progress, 2=played)
	//   ZPLAYHEAD > 0    — non-zero playback position (in-progress)
	//   ZPLAYCOUNT > 0   — episode has been played at least once (e.g. played on
	//                      another device or app; ZPLAYSTATE may still be 0)
	//   ZLASTDATEPLAYED  — timestamp of last play (set when play completes or syncs
	//                      from another device; also set when ZPLAYSTATE is 0)
	//
	// The Mac Podcasts app shows an episode as "played" when any of these indicate
	// prior listening, regardless of ZPLAYSTATE. Relying on ZPLAYSTATE alone misses
	// episodes played on other devices or via iCloud sync.
	//
	// PSUB (Apple Podcasts Subscription) and PLUS episodes are excluded:
	// their GUIDs are Apple-internal hex IDs (not RSS <guid> values) and their
	// enclosure URLs are Apple DRM streams — neither will match or play in any
	// other app. The parent podcast subscription is still exported.
	const q = `
		SELECT
			e.ZGUID,
			p.ZFEEDURL,
			e.ZTITLE,
			e.ZPUBDATE,
			e.ZDURATION,
			e.ZPLAYHEAD,
			e.ZLASTDATEPLAYED
		FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE (e.ZPLAYSTATE != 0 OR e.ZPLAYHEAD > 0 OR e.ZPLAYCOUNT > 0 OR e.ZLASTDATEPLAYED IS NOT NULL)
		  AND e.ZGUID IS NOT NULL
		  AND p.ZFEEDURL IS NOT NULL
		  AND p.ZFEEDURL NOT LIKE '%/eyJ%'
		  AND p.ZFEEDURL NOT LIKE 'internal://%'
		ORDER BY e.ZPUBDATE DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("apple/sqlite: query episodes: %w", err)
	}
	defer rows.Close()

	var out []model.EpisodeState
	for rows.Next() {
		var (
			guid          string
			feedURL       string
			title         string
			pubDateRaw    sql.NullFloat64
			durationSec   sql.NullFloat64
			playHeadSec   sql.NullFloat64
			lastPlayedRaw sql.NullFloat64
		)
		if err := rows.Scan(
			&guid, &feedURL, &title,
			&pubDateRaw, &durationSec,
			&playHeadSec, &lastPlayedRaw,
		); err != nil {
			return nil, 0, fmt.Errorf("apple/sqlite: scan episode: %w", err)
		}

		ep := model.EpisodeState{
			GUID:    guid,
			FeedURL: feedURL,
			Title:   title,
		}

		if pubDateRaw.Valid {
			ep.PubDate = coreDataEpoch.Add(time.Duration(pubDateRaw.Float64 * float64(time.Second)))
		}
		if durationSec.Valid && durationSec.Float64 > 0 {
			ep.Duration = time.Duration(durationSec.Float64 * float64(time.Second))
		}
		if playHeadSec.Valid && playHeadSec.Float64 > 0 {
			// Non-zero playhead means the episode is partially listened to.
			ep.PlayPosition = time.Duration(playHeadSec.Float64 * float64(time.Second))
			ep.PlayState = model.PlayStateInProgress
		} else {
			// No active playhead — episode was played to completion (or marked played).
			// The WHERE clause guarantees at least one play indicator is set, so this
			// row represents a listened-to episode even if ZPLAYSTATE happens to be 0.
			ep.PlayState = model.PlayStatePlayed
		}
		if lastPlayedRaw.Valid {
			ep.LastPlayed = coreDataEpoch.Add(time.Duration(lastPlayedRaw.Float64 * float64(time.Second)))
		}

		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Count PSUB/PLUS episodes that are included in the main query (i.e. on
	// non-internal, non-JWT feeds). These have Apple-proprietary GUIDs and DRM
	// enclosure URLs but are included for fuzzy matching by podcast title + pub date.
	const countPaywalled = `
		SELECT COUNT(*) FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE (e.ZPLAYSTATE != 0 OR e.ZPLAYHEAD > 0 OR e.ZPLAYCOUNT > 0 OR e.ZLASTDATEPLAYED IS NOT NULL)
		  AND e.ZGUID IS NOT NULL
		  AND p.ZFEEDURL IS NOT NULL
		  AND p.ZFEEDURL NOT LIKE '%/eyJ%'
		  AND p.ZFEEDURL NOT LIKE 'internal://%'
		  AND e.ZPRICETYPE IN ('PSUB', 'PLUS')`

	var included int
	_ = db.QueryRowContext(ctx, countPaywalled).Scan(&included)

	return out, included, nil
}

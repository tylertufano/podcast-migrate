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
	dbPath    string
	sinceTime time.Time // when set, only episodes modified after this time are returned
}

func NewSQLiteReader(dbPath string) *SQLiteReader {
	return &SQLiteReader{dbPath: dbPath}
}

// SetSinceTime restricts the episode read to episodes whose
// ZPLAYSTATELASTMODIFIEDDATE is after t. Episodes with no recorded modification
// date are excluded. A zero t disables the filter (default).
func (r *SQLiteReader) SetSinceTime(t time.Time) { r.sinceTime = t }

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
	// When a since-time is set, only episodes whose ZPLAYSTATELASTMODIFIEDDATE
	// is after the cutoff are returned. The cutoff is converted to CoreData
	// epoch seconds (seconds since 2001-01-01 UTC).
	var sinceArgs []any
	sinceClause := ""
	if !r.sinceTime.IsZero() {
		sinceSecs := r.sinceTime.Sub(coreDataEpoch).Seconds()
		sinceClause = "\n\t\t  AND e.ZPLAYSTATELASTMODIFIEDDATE > ?"
		sinceArgs = append(sinceArgs, sinceSecs)
	}
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
	// ZGUID is selected as nullable: some podcast RSS feeds omit <guid> for
	// individual episodes and Apple stores NULL in that case. Episodes without a
	// GUID are matched downstream by feed URL + pub date or feed URL + title.
	q := `
		SELECT
			e.ZGUID,
			p.ZFEEDURL,
			e.ZTITLE,
			e.ZPUBDATE,
			e.ZDURATION,
			e.ZPLAYHEAD,
			e.ZLASTDATEPLAYED,
			e.ZPLAYSTATE,
			e.ZPLAYCOUNT,
			e.ZPLAYSTATESOURCE
		FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE (e.ZPLAYSTATE != 0 OR e.ZPLAYHEAD > 0 OR e.ZPLAYCOUNT > 0 OR e.ZLASTDATEPLAYED IS NOT NULL)
		  AND (e.ZGUID IS NOT NULL OR (e.ZTITLE IS NOT NULL AND e.ZPUBDATE IS NOT NULL))
		  AND p.ZSUBSCRIBED = 1
		  AND p.ZFEEDURL IS NOT NULL
		  AND p.ZFEEDURL NOT LIKE '%/eyJ%'
		  AND p.ZFEEDURL NOT LIKE 'internal://%'` +
		sinceClause + `
		ORDER BY e.ZPUBDATE DESC`

	rows, err := db.QueryContext(ctx, q, sinceArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("apple/sqlite: query episodes: %w", err)
	}
	defer rows.Close()

	var out []model.EpisodeState
	for rows.Next() {
		var (
			guid            sql.NullString
			feedURL         string
			title           string
			pubDateRaw      sql.NullFloat64
			durationSec     sql.NullFloat64
			playHeadSec     sql.NullFloat64
			lastPlayedRaw   sql.NullFloat64
			playState       sql.NullInt64
			playCount       sql.NullInt64
			playStateSource sql.NullInt64
		)
		if err := rows.Scan(
			&guid, &feedURL, &title,
			&pubDateRaw, &durationSec,
			&playHeadSec, &lastPlayedRaw,
			&playState, &playCount,
			&playStateSource,
		); err != nil {
			return nil, 0, fmt.Errorf("apple/sqlite: scan episode: %w", err)
		}

		ep := model.EpisodeState{
			GUID:    guid.String, // empty string when NULL; matching falls back to title+date
			FeedURL: feedURL,
			Title:   title,
		}

		if pubDateRaw.Valid {
			ep.PubDate = coreDataEpoch.Add(time.Duration(pubDateRaw.Float64 * float64(time.Second)))
		}
		if durationSec.Valid && durationSec.Float64 > 0 {
			ep.Duration = time.Duration(durationSec.Float64 * float64(time.Second))
		}
		// trustedPlayed reports whether a ZPLAYSTATE=2 row represents genuine
		// listening rather than Apple's automatic "mark as played" behaviour.
		//
		// Apple sets ZPLAYSTATESOURCE to indicate why an episode was marked played:
		//   1 — manually marked by the user in the UI
		//   2 — auto-marked when a newer episode arrived (daily/news shows, not listened)
		//   3 — listened to completion on this device
		//   4 — synced from another device that had it as played
		//   6 — default/initial state; also used for auto-marks with no play record
		//
		// We exclude only the two known auto-mark sources (2 and 6) when they lack
		// a ZLASTDATEPLAYED timestamp — those rows were never actually listened to.
		// All other ZPLAYSTATE=2 rows are trusted, including:
		//   ZPLAYSTATESOURCE=1 (user-initiated "Mark as Played")
		//   ZPLAYSTATESOURCE=3 (listened to completion)
		//   ZPLAYSTATESOURCE=4 (synced from another device)
		//   Any unknown/future source value
		//
		// A ZLASTDATEPLAYED timestamp is also accepted as corroborating evidence for
		// auto-marked rows (2/6) — it indicates the episode was actually played at
		// some point even if the source was auto.
		autoMarked := (playStateSource.Int64 == 2 || playStateSource.Int64 == 6) && !lastPlayedRaw.Valid
		trustedPlayed := playState.Valid && playState.Int64 == 2 && !autoMarked

		switch {
		case playHeadSec.Valid && playHeadSec.Float64 > 0:
			// Non-zero playhead: episode is partially listened to.
			ep.PlayPosition = time.Duration(playHeadSec.Float64 * float64(time.Second))
			ep.PlayState = model.PlayStateInProgress
		case trustedPlayed:
			ep.PlayState = model.PlayStatePlayed
		case playState.Valid && playState.Int64 == 1:
			// ZPLAYSTATE=1 (in-progress) without a stored playhead — app explicitly
			// set the episode as started; treat as played rather than silently dropping.
			ep.PlayState = model.PlayStatePlayed
		case playCount.Valid && playCount.Int64 > 0:
			// Played at least once (possibly on another device before iCloud sync
			// back-filled ZPLAYSTATE). Rare with modern sync but kept as a fallback.
			ep.PlayState = model.PlayStatePlayed
		case lastPlayedRaw.Valid &&
			playStateSource.Int64 != 0 &&
			playStateSource.Int64 != 2 &&
			playStateSource.Int64 != 6:
			// ZLASTDATEPLAYED is set and ZPLAYSTATESOURCE carries a non-auto value
			// (1=manual, 3=completion, 4=device-sync, or similar).
			// This pattern occurs when an episode was played on another device (e.g.
			// iPhone) and iCloud synced the play timestamp and source, but not
			// ZPLAYSTATE or ZPLAYCOUNT.
			// ZPLAYSTATESOURCE=0 (unset/default) is excluded: Apple also sets
			// ZLASTDATEPLAYED for non-user events such as background downloads and
			// iCloud metadata refreshes, leaving ZPLAYSTATESOURCE at 0.
			ep.PlayState = model.PlayStatePlayed
		default:
			// No reliable evidence of genuine playback; skip this episode.
			continue
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
		  AND p.ZSUBSCRIBED = 1
		  AND p.ZFEEDURL IS NOT NULL
		  AND p.ZFEEDURL NOT LIKE '%/eyJ%'
		  AND p.ZFEEDURL NOT LIKE 'internal://%'
		  AND e.ZPRICETYPE IN ('PSUB', 'PLUS')`

	var included int
	_ = db.QueryRowContext(ctx, countPaywalled).Scan(&included)

	return out, included, nil
}

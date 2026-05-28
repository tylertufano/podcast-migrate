package apple

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
		SkippedPaywalledEpisodes: skippedEpisodes,
		SkippedInternalPodcasts: skippedPodcasts,
	}, nil
}

func (r *SQLiteReader) readPodcasts(ctx context.Context, db *sql.DB) ([]model.Podcast, int, error) {
	// Restrict to http/https feeds. The "internal://" scheme is used by
	// Apple-exclusive shows that have no public RSS feed — no other app can
	// subscribe to them, so including them in an export would produce invalid
	// entries. Any future non-HTTP schemes are excluded by the same filter.
	const q = `
		SELECT ZFEEDURL, ZTITLE, ZAUTHOR, ZIMAGEURL
		FROM ZMTPODCAST
		WHERE ZSUBSCRIBED = 1
		  AND (ZFEEDURL LIKE 'http://%' OR ZFEEDURL LIKE 'https://%')
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
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	const countInternal = `
		SELECT COUNT(*) FROM ZMTPODCAST
		WHERE ZSUBSCRIBED = 1
		  AND ZFEEDURL IS NOT NULL
		  AND ZFEEDURL NOT LIKE 'http://%'
		  AND ZFEEDURL NOT LIKE 'https://%'`

	var skipped int
	_ = db.QueryRowContext(ctx, countInternal).Scan(&skipped)

	return out, skipped, nil
}

func (r *SQLiteReader) readEpisodes(ctx context.Context, db *sql.DB) ([]model.EpisodeState, int, error) {
	// Only fetch episodes that have some user interaction (played, in-progress,
	// or with a non-zero play position). Unplayed-and-untouched episodes
	// don't carry meaningful state to migrate.
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
			e.ZPLAYSTATE,
			e.ZPLAYHEAD,
			e.ZLASTDATEPLAYED
		FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE (e.ZPLAYSTATE != 0 OR e.ZPLAYHEAD > 0)
		  AND e.ZGUID IS NOT NULL
		  AND p.ZFEEDURL IS NOT NULL
		  AND e.ZPRICETYPE NOT IN ('PSUB', 'PLUS')
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
			playStateRaw  int
			playHeadSec   sql.NullFloat64
			lastPlayedRaw sql.NullFloat64
		)
		if err := rows.Scan(
			&guid, &feedURL, &title,
			&pubDateRaw, &durationSec,
			&playStateRaw, &playHeadSec,
			&lastPlayedRaw,
		); err != nil {
			return nil, 0, fmt.Errorf("apple/sqlite: scan episode: %w", err)
		}

		ep := model.EpisodeState{
			GUID:      guid,
			FeedURL:   feedURL,
			Title:     title,
			PlayState: model.PlayState(playStateRaw),
		}

		if pubDateRaw.Valid {
			ep.PubDate = coreDataEpoch.Add(time.Duration(pubDateRaw.Float64 * float64(time.Second)))
		}
		if durationSec.Valid && durationSec.Float64 > 0 {
			ep.Duration = time.Duration(durationSec.Float64 * float64(time.Second))
		}
		if playHeadSec.Valid && playHeadSec.Float64 > 0 {
			ep.PlayPosition = time.Duration(playHeadSec.Float64 * float64(time.Second))
		}
		if lastPlayedRaw.Valid {
			ep.LastPlayed = coreDataEpoch.Add(time.Duration(lastPlayedRaw.Float64 * float64(time.Second)))
		}

		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Count paywalled episodes separately so callers can report them.
	const countPaywalled = `
		SELECT COUNT(*) FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE (e.ZPLAYSTATE != 0 OR e.ZPLAYHEAD > 0)
		  AND e.ZGUID IS NOT NULL
		  AND p.ZFEEDURL IS NOT NULL
		  AND e.ZPRICETYPE IN ('PSUB', 'PLUS')`

	var skipped int
	_ = db.QueryRowContext(ctx, countPaywalled).Scan(&skipped)

	return out, skipped, nil
}

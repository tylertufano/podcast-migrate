package apple

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	_ "modernc.org/sqlite"
)

// appleEpisodeRecord holds the data needed to match a ZMTEPISODE row and look
// up its Apple catalog ID for use with the web API.
type appleEpisodeRecord struct {
	pk           int64
	feedURL      string
	podcastTitle string // normalised podcast title from ZMTPODCAST.ZTITLE
	guid         string
	title        string
	pubDate      time.Time
	playState    model.PlayState
	playPosition time.Duration
	// rawZPlayState is the raw ZPLAYSTATE column value (0/1/2).
	rawZPlayState int64
	// rawZPlayStateSource is the raw ZPLAYSTATESOURCE column value.
	//   1 = user explicitly marked as played/unplayed via the Mac UI
	//   3 = episode played to completion via audio on this Mac
	//   6 = play state written by PodcastContentService (cloud sync)
	rawZPlayStateSource int64
	// lastUserMarkedAsPlayed is ZLASTUSERMARKEDASPLAYEDDATE decoded from CoreData epoch.
	lastUserMarkedAsPlayed time.Time
	// lastDatePlayed is ZLASTDATEPLAYED decoded from CoreData epoch.
	lastDatePlayed time.Time
	// playCount is ZPLAYCOUNT.
	playCount int64
	// storeTrackID is the Apple catalog episode ID (ZSTORETRACKID).
	// Used by WebAPIWriter to target the amp-api.podcasts.apple.com playback
	// positions endpoint. Zero means the episode is not in the Apple catalog
	// (private feed, not yet indexed, etc.) and the web API cannot be used.
	storeTrackID int64
}

// Apple Podcasts ZPLAYSTATE column values.
const (
	applePlayStatePlayed = 2
)

// Apple Podcasts ZPLAYSTATESOURCE column values observed in MTLibrary.sqlite.
const (
	applePlayStateSourceUserMarked = 1 // user explicitly set state via UI
	applePlayStateSourceListened   = 3 // episode played to completion via audio
	applePlayStateSourceExternal   = 6 // play state written by PodcastContentService (cloud sync)
)

// buildAppleIndex queries ZMTEPISODE (joined to ZMTPODCAST) and returns a map
// keyed by multiple match strategies. Four key types are built for each episode:
//
//	feeddate:<normFeedURL>|<RFC3339>       — primary: same feed URL, same pubDate
//	feedtitle:<normFeedURL>|<normTitle>    — fallback: same feed URL, same episode title
//	poddate:<normPodTitle>|<RFC3339>       — secondary: same podcast title, same pubDate
//	podtitle:<normPodTitle>|<normEpTitle>  — secondary fallback: same podcast + episode title
//
// The "pod" keys allow matching when Overcast and Apple Podcasts subscribed to
// the same show via different feed URLs.
func buildAppleIndex(ctx context.Context, db *sql.DB) (map[string]appleEpisodeRecord, error) {
	const q = `
		SELECT
			e.Z_PK,
			e.ZGUID,
			p.ZFEEDURL,
			p.ZTITLE AS PODCAST_TITLE,
			e.ZTITLE AS EPISODE_TITLE,
			e.ZPUBDATE,
			e.ZPLAYHEAD,
			e.ZPLAYSTATE,
			e.ZPLAYCOUNT,
			e.ZLASTDATEPLAYED,
			e.ZPLAYSTATESOURCE,
			e.ZLASTUSERMARKEDASPLAYEDDATE,
			e.ZSTORETRACKID
		FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE p.ZSUBSCRIBED = 1
		  AND p.ZFEEDURL IS NOT NULL
		  AND (p.ZFEEDURL LIKE 'http://%' OR p.ZFEEDURL LIKE 'https://%')`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("apple/sqlite: query episodes: %w", err)
	}
	defer rows.Close()

	index := make(map[string]appleEpisodeRecord)

	for rows.Next() {
		var (
			pk                int64
			guid              sql.NullString
			feedURL           string
			podcastTitle      sql.NullString
			episodeTitle      sql.NullString
			pubDateRaw        sql.NullFloat64
			playHeadSec       sql.NullFloat64
			playStateSq       sql.NullInt64
			playCountSq       sql.NullInt64
			lastPlayedRaw     sql.NullFloat64
			playStateSourceSq sql.NullInt64
			lastUserMarkedRaw sql.NullFloat64
			storeTrackIDSq    sql.NullInt64
		)
		if err := rows.Scan(&pk, &guid, &feedURL, &podcastTitle, &episodeTitle,
			&pubDateRaw, &playHeadSec, &playStateSq, &playCountSq, &lastPlayedRaw,
			&playStateSourceSq, &lastUserMarkedRaw, &storeTrackIDSq); err != nil {
			return nil, fmt.Errorf("apple/sqlite: scan: %w", err)
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
		if playStateSq.Valid {
			rec.rawZPlayState = playStateSq.Int64
		}
		if playStateSourceSq.Valid {
			rec.rawZPlayStateSource = playStateSourceSq.Int64
		}
		if lastUserMarkedRaw.Valid {
			rec.lastUserMarkedAsPlayed = coreDataEpoch.Add(
				time.Duration(lastUserMarkedRaw.Float64 * float64(time.Second)))
		}
		if lastPlayedRaw.Valid {
			rec.lastDatePlayed = coreDataEpoch.Add(
				time.Duration(lastPlayedRaw.Float64 * float64(time.Second)))
		}
		if playCountSq.Valid {
			rec.playCount = playCountSq.Int64
		}
		if storeTrackIDSq.Valid {
			rec.storeTrackID = storeTrackIDSq.Int64
		}

		// Determine play state using the same four-column logic as the SQLite reader.
		switch {
		case playHeadSec.Valid && playHeadSec.Float64 > 0:
			rec.playPosition = time.Duration(playHeadSec.Float64 * float64(time.Second))
			rec.playState = model.PlayStateInProgress
		case (playStateSq.Valid && playStateSq.Int64 != 0) ||
			(playCountSq.Valid && playCountSq.Int64 > 0) ||
			lastPlayedRaw.Valid:
			rec.playState = model.PlayStatePlayed
		}

		normFeed := normalizeWriteFeedURL(feedURL)
		normPodTitle := rec.podcastTitle

		// Feed URL-based keys (primary).
		if !rec.pubDate.IsZero() {
			setIfAbsent(index, feedDateKey(normFeed, rec.pubDate), rec)
		}
		if epTitle != "" {
			setPreferUnplayed(index, feedTitleKey(normFeed, epTitle), rec)
		}

		// Podcast title-based keys (secondary, cross-feed-URL matching).
		if normPodTitle != "" {
			if !rec.pubDate.IsZero() {
				setIfAbsent(index, podDateKey(normPodTitle, rec.pubDate), rec)
			}
			if epTitle != "" {
				setPreferUnplayed(index, podTitleKey(normPodTitle, epTitle), rec)
			}
		}
	}
	return index, rows.Err()
}

// findInAppleIndex looks up an episode in the Apple index using a cascade of
// four match strategies:
//  1. feedURL + pubDate  (same feed, exact date)
//  2. feedURL + title    (same feed, same episode title)
//  3. podcastTitle + pubDate  (any feed for same podcast, exact date)
//  4. podcastTitle + title    (any feed for same podcast, same episode title)
func findInAppleIndex(
	index map[string]appleEpisodeRecord,
	ep model.EpisodeState,
	feedToTitle map[string]string,
	tolerance time.Duration,
) (appleEpisodeRecord, bool) {
	normFeed := normalizeWriteFeedURL(ep.FeedURL)

	if !ep.PubDate.IsZero() {
		if rec, ok := index[feedDateKey(normFeed, ep.PubDate)]; ok {
			return rec, true
		}
	}
	if ep.Title != "" {
		if rec, ok := index[feedTitleKey(normFeed, ep.Title)]; ok {
			if withinDateTolerance(ep.PubDate, rec.pubDate, tolerance) {
				return rec, true
			}
		}
	}

	podTitle := feedToTitle[ep.FeedURL]
	if podTitle == "" {
		return appleEpisodeRecord{}, false
	}

	if !ep.PubDate.IsZero() {
		if rec, ok := index[podDateKey(podTitle, ep.PubDate)]; ok {
			return rec, true
		}
	}
	if ep.Title != "" {
		if rec, ok := index[podTitleKey(podTitle, ep.Title)]; ok {
			if withinDateTolerance(ep.PubDate, rec.pubDate, tolerance) {
				return rec, true
			}
		}
	}

	return appleEpisodeRecord{}, false
}

// withinDateTolerance reports whether two pub dates are close enough to allow
// a title-based match. A tolerance ≤ 0 disables the guard.
func withinDateTolerance(a, b time.Time, tolerance time.Duration) bool {
	if tolerance <= 0 {
		return true
	}
	if a.IsZero() || b.IsZero() {
		return true
	}
	diff := a.Sub(b)
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

// buildFeedToTitleFromLib returns a map from feed URL to lowercased podcast title.
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

// normalizeWriteFeedURL produces a canonical feed URL key for matching.
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

// setIfAbsent inserts rec into m under key only if the key is not already present.
func setIfAbsent(m map[string]appleEpisodeRecord, key string, rec appleEpisodeRecord) {
	if _, exists := m[key]; !exists {
		m[key] = rec
	}
}

// setPreferUnplayed inserts rec into m under key, preferring records that are
// not already effectively played. Used for title-keyed entries where the same
// title may appear as both a played re-release and an unplayed original.
func setPreferUnplayed(m map[string]appleEpisodeRecord, key string, rec appleEpisodeRecord) {
	existing, exists := m[key]
	if !exists {
		m[key] = rec
		return
	}
	if isEffectivelyPlayed(existing) && !isEffectivelyPlayed(rec) {
		m[key] = rec
	}
}

// isEffectivelyPlayed reports whether an Apple episode record is already played,
// used to rank duplicates in the index.
func isEffectivelyPlayed(r appleEpisodeRecord) bool {
	userMarked := !r.lastUserMarkedAsPlayed.IsZero() &&
		r.rawZPlayStateSource == applePlayStateSourceUserMarked
	listenedNaturally := r.rawZPlayState == applePlayStatePlayed &&
		r.rawZPlayStateSource == applePlayStateSourceListened
	cloudPlayed := r.rawZPlayState == applePlayStatePlayed &&
		r.rawZPlayStateSource == applePlayStateSourceExternal
	return userMarked || listenedNaturally || cloudPlayed
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

// ---------------------------------------------------------------------------
// Per-episode log helpers
// ---------------------------------------------------------------------------

func writeLogHeader(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, "status,podcast,episode,pub_date,source_state,target_state,note")
}

func writeLogLine(w io.Writer, status, podcast, episode string, pubDate time.Time, srcState, tgtState, note string) {
	if w == nil {
		return
	}
	dateStr := ""
	if !pubDate.IsZero() {
		dateStr = pubDate.UTC().Format("2006-01-02")
	}
	fmt.Fprintf(w, "%s,%s,%s,%s,%s,%s,%s\n",
		csvField(status), csvField(podcast), csvField(episode),
		dateStr,
		csvField(srcState), csvField(tgtState), csvField(note))
}

func playStateLabel(ps model.PlayState, pos time.Duration) string {
	switch ps {
	case model.PlayStatePlayed:
		return "played"
	case model.PlayStateInProgress:
		if pos > 0 {
			return "in_progress(" + pos.Round(time.Second).String() + ")"
		}
		return "in_progress"
	default:
		return "unplayed"
	}
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

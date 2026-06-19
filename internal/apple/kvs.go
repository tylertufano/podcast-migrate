package apple

import (
	"bytes"
	"compress/flate"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

const (
	kvsEndpoint    = "https://bookkeeper.itunes.apple.com/WebObjects/MZBookkeeper.woa/wa/putAll"
	kvsGetEndpoint = "https://bookkeeper.itunes.apple.com/WebObjects/MZBookkeeper.woa/wa/getAll"
	kvsDomain      = "com.apple.upp"
)

// KVSWriter pushes episode play state to Apple's UPP key-value store via
// bookkeeper.itunes.apple.com/putAll. This is the sync path for private and
// subscriber-feed episodes (ZSTORETRACKID=0) that are not indexed in the Apple
// catalog and cannot use the amp-api path.
//
// Auth uses iTunes Store session cookies. These are sourced from the
// APPLE_KVS_COOKIES env var (preferred) or scanned from known binarycookies
// files on disk. The APPLE_KVS_DSID env var supplies the DSID when it cannot
// be extracted from the cookie string.
type KVSWriter struct {
	sqlitePath     string
	cookieHdr      string // full Cookie: header value
	dsid           string // iTunes Store account DSID
	httpClient     *http.Client
	sessionReady   bool           // true after getAll has been called
	serverVersions map[string]int // metadataIdentifier → current server version, populated by getAll
}

// binaryCookiePaths is the list of paths tried when APPLE_KVS_COOKIES is unset.
// Checked in order; first one that yields a valid DSID wins.
var binaryCookiePaths = []string{
	// Sandboxed Podcasts container — present on older macOS builds.
	"Library/Containers/com.apple.podcasts/Data/Library/Cookies/Cookies.binarycookies",
	// System-level HTTPStorages — TV and Music share the iTunes Store session.
	"Library/HTTPStorages/com.apple.TV.binarycookies",
	"Library/HTTPStorages/com.apple.Music.binarycookies",
}

// NewKVSWriter constructs a KVSWriter ready to call putAll.
//
// Cookie auth is resolved in this order:
//  1. APPLE_KVS_COOKIES env var  (set from a Proxyman capture)
//  2. binarycookies files at known paths
//
// APPLE_KVS_DSID overrides the DSID when it cannot be parsed from the cookies.
//
// The HTTP client uses a cookie jar pre-populated with the initial session
// cookies. Apple's KVS server rotates tokens via Set-Cookie on each response;
// the jar handles this automatically so subsequent putAll calls in the same
// session use the updated tokens rather than the stale originals.
func NewKVSWriter(sqlitePath string) (*KVSWriter, error) {
	dsid, cookieHdr, err := resolveCookies()
	if err != nil {
		return nil, err
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("kvs: cookie jar: %w", err)
	}
	kvsURL, _ := url.Parse("https://bookkeeper.itunes.apple.com")
	jar.SetCookies(kvsURL, parseCookieHeader(cookieHdr))

	if sqlitePath == "" {
		sqlitePath = DefaultSQLitePath()
	}
	return &KVSWriter{
		sqlitePath: sqlitePath,
		cookieHdr:  cookieHdr,
		dsid:       dsid,
		httpClient: &http.Client{Timeout: 30 * time.Second, Jar: jar},
	}, nil
}

// parseCookieHeader splits a Cookie: header value into individual http.Cookie
// values suitable for seeding a cookiejar.
func parseCookieHeader(header string) []*http.Cookie {
	var cookies []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{
			Name:  strings.TrimSpace(kv[0]),
			Value: strings.TrimSpace(kv[1]),
		})
	}
	return cookies
}

// resolveCookies finds the Cookie header and DSID for KVS requests.
func resolveCookies() (dsid, cookieHdr string, err error) {
	// Env var takes precedence — populate from a Proxyman capture.
	if envCookies := os.Getenv("APPLE_KVS_COOKIES"); envCookies != "" {
		cookieHdr = envCookies
		dsid = os.Getenv("APPLE_KVS_DSID")
		if dsid == "" {
			dsid = extractDSIDFromCookies(cookieHdr)
		}
		if dsid == "" {
			return "", "", fmt.Errorf("kvs: APPLE_KVS_COOKIES is set but no DSID found — also set APPLE_KVS_DSID")
		}
		return dsid, cookieHdr, nil
	}

	// Fall back to known binarycookies paths.
	home, _ := os.UserHomeDir()
	for _, rel := range binaryCookiePaths {
		p := filepath.Join(home, rel)
		d, ch, readErr := parsePodcastCookies(p)
		if readErr == nil && d != "" {
			return d, ch, nil
		}
	}

	return "", "", fmt.Errorf("kvs: no iTunes Store session found\n" +
		"  Set APPLE_KVS_COOKIES to the Cookie: header value from a Proxyman capture of\n" +
		"  bookkeeper.itunes.apple.com while Apple Podcasts is open and signed in.\n" +
		"  Also set APPLE_KVS_DSID to your numeric DSID (visible as X-Dsid= in the cookie).")
}

// extractDSIDFromCookies parses the X-Dsid value from a Cookie header string.
func extractDSIDFromCookies(cookies string) string {
	for _, part := range strings.Split(cookies, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "X-Dsid") {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

// kvsItemWithMeta pairs a kvsItem with the source episode state for logging.
type kvsItemWithMeta struct {
	item     kvsItem
	ep       model.EpisodeState
	podTitle string
}

// applyPlayState fills item's play-state fields from ep.
func applyPlayState(ep model.EpisodeState, item *kvsItem, nowSec float64) {
	item.HasBeenPlayed = ep.PlayState == model.PlayStatePlayed
	item.BookmarkTimeSec = 0
	if ep.PlayState == model.PlayStateInProgress {
		item.BookmarkTimeSec = ep.PlayPosition.Seconds()
	}
	if ep.PlayState == model.PlayStatePlayed && item.PlayCount < 1 {
		item.PlayCount = 1
	}
	item.TimestampSec = nowSec
}

// Write is the provider.Writer interface implementation. It finds private-feed
// episode matches in the local Apple Podcasts DB and pushes all of them via a
// single putAll request. Catalog episodes (ZSTORETRACKID != 0) are skipped
// (they are handled by WebAPIWriter via amp-api).
//
// All matched episodes are batched into one HTTP call to work around the
// server's one-time-use session token: a second putAll with a spent token
// returns status 1198 regardless of cookie rotation.
func (w *KVSWriter) Write(ctx context.Context, lib *model.Library, opts provider.WriteOptions) (int, error) {
	db, err := sql.Open("sqlite", "file:"+w.sqlitePath+"?mode=ro&_journal=off")
	if err != nil {
		return 0, fmt.Errorf("kvs: open sqlite: %w", err)
	}
	defer db.Close()

	feedToTitle := buildFeedToTitleFromLib(lib)
	episodes := filterLibraryEpisodes(lib.Episodes, feedToTitle, opts.PodcastFilter)

	writeLogHeader(opts.LogWriter)

	now := time.Since(coreDataEpoch).Seconds()
	var pending []kvsItemWithMeta
	dryRunCount := 0

	for _, ep := range episodes {
		if ep.FromDestination {
			continue
		}
		if ep.PlayState != model.PlayStatePlayed && ep.PlayState != model.PlayStateInProgress {
			continue
		}

		item, found, err := lookupPrivateEpisode(ctx, db, ep)
		if err != nil {
			fmt.Printf("  kvs: lookup failed for %q: %v\n", ep.Title, err)
			writeLogLine(opts.LogWriter, "error", feedToTitle[ep.FeedURL], ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", err.Error())
			continue
		}
		if !found {
			continue
		}

		podTitle := feedToTitle[ep.FeedURL]
		applyPlayState(ep, &item, now)

		if opts.DryRun {
			fmt.Printf("  [dry-run] kvs: would putAll %q — %q (key=%s)\n",
				podTitle, ep.Title, item.MetadataIdentifier)
			writeLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", "kvs (private feed)")
			dryRunCount++
			continue
		}

		pending = append(pending, kvsItemWithMeta{item, ep, podTitle})
	}

	if opts.DryRun {
		return dryRunCount, nil
	}
	if len(pending) == 0 {
		return 0, nil
	}

	// Single putAll for all matched episodes — one token, one round-trip.
	items := make([]kvsItem, len(pending))
	for i, p := range pending {
		items[i] = p.item
	}
	if err := w.putAll(ctx, items); err != nil {
		for _, p := range pending {
			writeLogLine(opts.LogWriter, "error", p.podTitle, p.ep.Title, p.ep.PubDate,
				playStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", err.Error())
		}
		return 0, err
	}

	for _, p := range pending {
		fmt.Printf("  kvs: synced %q — %q\n", p.podTitle, p.ep.Title)
		writeLogLine(opts.LogWriter, "updated", p.podTitle, p.ep.Title, p.ep.PubDate,
			playStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", "kvs (private feed)")
	}
	return len(pending), nil
}

// WriteBatch resolves and syncs a slice of episodes via a single putAll request.
// Called by WebAPIWriter as the fallback for private-feed episodes that cannot
// be resolved via the Apple catalog (CatalogPodcastNotInCatalog). Batching is
// critical: the server's session token is single-use, so a second sequential
// putAll with the same token returns status 1198.
func (w *KVSWriter) WriteBatch(ctx context.Context, episodes []model.EpisodeState, feedToTitle map[string]string, opts provider.WriteOptions) (int, error) {
	db, err := sql.Open("sqlite", "file:"+w.sqlitePath+"?mode=ro&_journal=off")
	if err != nil {
		return 0, fmt.Errorf("kvs: open sqlite: %w", err)
	}
	defer db.Close()

	now := time.Since(coreDataEpoch).Seconds()
	var pending []kvsItemWithMeta
	dryRunCount := 0

	for _, ep := range episodes {
		podTitle := feedToTitle[ep.FeedURL]

		item, found, err := lookupPrivateEpisode(ctx, db, ep)
		if err != nil {
			fmt.Printf("  kvs: lookup failed for %q: %v\n", ep.Title, err)
			writeLogLine(opts.LogWriter, "error", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", err.Error())
			continue
		}
		if !found {
			writeLogLine(opts.LogWriter, "no_apple_id", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—",
				"not in catalog and not found in local Apple Podcasts DB")
			continue
		}

		applyPlayState(ep, &item, now)

		if opts.DryRun {
			fmt.Printf("  [dry-run] kvs: would putAll %q — %q (key=%s)\n",
				podTitle, ep.Title, item.MetadataIdentifier)
			writeLogLine(opts.LogWriter, "would_update", podTitle, ep.Title, ep.PubDate,
				playStateLabel(ep.PlayState, ep.PlayPosition), "—", "kvs (private feed)")
			dryRunCount++
			continue
		}

		pending = append(pending, kvsItemWithMeta{item, ep, podTitle})
	}

	if opts.DryRun {
		return dryRunCount, nil
	}
	if len(pending) == 0 {
		return 0, nil
	}

	items := make([]kvsItem, len(pending))
	for i, p := range pending {
		items[i] = p.item
	}
	if err := w.putAll(ctx, items); err != nil {
		for _, p := range pending {
			writeLogLine(opts.LogWriter, "error", p.podTitle, p.ep.Title, p.ep.PubDate,
				playStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", err.Error())
		}
		return 0, fmt.Errorf("kvs: putAll batch failed: %w", err)
	}

	for _, p := range pending {
		fmt.Printf("  kvs: synced %q — %q\n", p.podTitle, p.ep.Title)
		writeLogLine(opts.LogWriter, "updated", p.podTitle, p.ep.Title, p.ep.PubDate,
			playStateLabel(p.ep.PlayState, p.ep.PlayPosition), "—", "kvs (private feed)")
	}
	return len(pending), nil
}

// WriteEpisode looks up a single episode and pushes its play state to the KVS.
// Returns (true, nil) when the push succeeded, (false, nil) when the episode is
// not found in the local DB as a private-feed episode.
//
// Prefer WriteBatch when syncing multiple episodes — the session token is
// single-use so sequential WriteEpisode calls will fail after the first.
func (w *KVSWriter) WriteEpisode(ctx context.Context, ep model.EpisodeState) (bool, error) {
	db, err := sql.Open("sqlite", "file:"+w.sqlitePath+"?mode=ro&_journal=off")
	if err != nil {
		return false, fmt.Errorf("kvs: open sqlite: %w", err)
	}
	defer db.Close()

	item, found, err := lookupPrivateEpisode(ctx, db, ep)
	if err != nil || !found {
		return found, err
	}

	now := time.Since(coreDataEpoch).Seconds()
	applyPlayState(ep, &item, now)

	return true, w.putAll(ctx, []kvsItem{item})
}

// kvsItem holds the data needed to build one entry in a putAll request.
type kvsItem struct {
	MetadataIdentifier string  // ZMTEPISODE.ZMETADATAIDENTIFIER = the KVS key
	UPPVersion         int     // ZMTUPPMETADATA.Z_OPT = "base-version"
	BookmarkTimeSec    float64 // bookmark position (0 = fully played)
	HasBeenPlayed      bool
	PlayCount          int
	TimestampSec       float64 // CoreData epoch seconds
}

// lookupPrivateEpisode finds the local Apple Podcasts episode that matches ep
// and returns its KVS metadata. Only private-feed episodes (ZSTORETRACKID=0)
// with a non-empty ZMETADATAIDENTIFIER are returned.
//
// Matching priority: GUID → FeedURL+PubDate (within 1 day) → FeedURL+Title.
func lookupPrivateEpisode(ctx context.Context, db *sql.DB, ep model.EpisodeState) (kvsItem, bool, error) {
	// Attempt GUID match first (exact, fast).
	if ep.GUID != "" {
		item, found, err := scanKVSRow(db.QueryRowContext(ctx, `
			SELECT e.ZMETADATAIDENTIFIER,
			       COALESCE(u.Z_OPT, 1),
			       COALESCE(u.ZBOOKMARKTIME, 0.0),
			       COALESCE(u.ZHASBEENPLAYED, 0),
			       COALESCE(u.ZPLAYCOUNT, 0),
			       COALESCE(u.ZTIMESTAMP, 0.0)
			FROM ZMTEPISODE e
			JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
			LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
			WHERE e.ZMETADATAIDENTIFIER IS NOT NULL
			  AND (e.ZSTORETRACKID IS NULL OR e.ZSTORETRACKID = 0)
			  AND p.ZSUBSCRIBED = 1
			  AND e.ZGUID = ?
			LIMIT 1`, ep.GUID))
		if err != nil {
			return kvsItem{}, false, fmt.Errorf("lookup by GUID: %w", err)
		}
		if found {
			return item, true, nil
		}
	}

	// Fall back to FeedURL + PubDate (within 24 hours).
	if ep.FeedURL != "" && !ep.PubDate.IsZero() {
		pubDateSec := ep.PubDate.Sub(coreDataEpoch).Seconds()
		item, found, err := scanKVSRow(db.QueryRowContext(ctx, `
			SELECT e.ZMETADATAIDENTIFIER,
			       COALESCE(u.Z_OPT, 1),
			       COALESCE(u.ZBOOKMARKTIME, 0.0),
			       COALESCE(u.ZHASBEENPLAYED, 0),
			       COALESCE(u.ZPLAYCOUNT, 0),
			       COALESCE(u.ZTIMESTAMP, 0.0)
			FROM ZMTEPISODE e
			JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
			LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
			WHERE e.ZMETADATAIDENTIFIER IS NOT NULL
			  AND (e.ZSTORETRACKID IS NULL OR e.ZSTORETRACKID = 0)
			  AND p.ZSUBSCRIBED = 1
			  AND p.ZFEEDURL LIKE ? || '%'
			  AND ABS(COALESCE(e.ZPUBDATE, 0) - ?) < 86400
			LIMIT 1`, ep.FeedURL, pubDateSec))
		if err != nil {
			return kvsItem{}, false, fmt.Errorf("lookup by feed+date: %w", err)
		}
		if found {
			return item, true, nil
		}
	}

	// Final fallback: FeedURL + title (case-insensitive).
	if ep.FeedURL != "" && ep.Title != "" {
		item, found, err := scanKVSRow(db.QueryRowContext(ctx, `
			SELECT e.ZMETADATAIDENTIFIER,
			       COALESCE(u.Z_OPT, 1),
			       COALESCE(u.ZBOOKMARKTIME, 0.0),
			       COALESCE(u.ZHASBEENPLAYED, 0),
			       COALESCE(u.ZPLAYCOUNT, 0),
			       COALESCE(u.ZTIMESTAMP, 0.0)
			FROM ZMTEPISODE e
			JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
			LEFT JOIN ZMTUPPMETADATA u ON u.ZMETADATAIDENTIFIER = e.ZMETADATAIDENTIFIER
			WHERE e.ZMETADATAIDENTIFIER IS NOT NULL
			  AND (e.ZSTORETRACKID IS NULL OR e.ZSTORETRACKID = 0)
			  AND p.ZSUBSCRIBED = 1
			  AND p.ZFEEDURL LIKE ? || '%'
			  AND LOWER(TRIM(e.ZTITLE)) = LOWER(TRIM(?))
			LIMIT 1`, ep.FeedURL, ep.Title))
		if err != nil {
			return kvsItem{}, false, fmt.Errorf("lookup by feed+title: %w", err)
		}
		if found {
			return item, true, nil
		}
	}

	return kvsItem{}, false, nil
}

func scanKVSRow(row *sql.Row) (kvsItem, bool, error) {
	var (
		metaID  string
		zopt    int
		bktm    float64
		hbplInt int
		plct    int
		tstm    float64
	)
	err := row.Scan(&metaID, &zopt, &bktm, &hbplInt, &plct, &tstm)
	if errors.Is(err, sql.ErrNoRows) {
		return kvsItem{}, false, nil
	}
	if err != nil {
		return kvsItem{}, false, err
	}
	return kvsItem{
		MetadataIdentifier: metaID,
		UPPVersion:         zopt,
		BookmarkTimeSec:    bktm,
		HasBeenPlayed:      hbplInt != 0,
		PlayCount:          plct,
		TimestampSec:       tstm,
	}, true, nil
}

// setKVSHeaders applies the standard iTunes Store KVS headers to req.
func (w *KVSWriter) setKVSHeaders(req *http.Request) {
	req.Header.Set("iCloud-DSID", w.dsid)
	req.Header.Set("X-DSID", w.dsid)
	req.Header.Set("X-Apple-Store-Front", "143441-1,42 t:podcasts1")
	req.Header.Set("X-Apple-Client-Application", "com.apple.podcasts")
	req.Header.Set("X-Apple-I-Locale", "en_US")
	req.Header.Set("X-Apple-I-Client-Time", time.Now().UTC().Format(time.RFC3339))
	req.Header.Set("Content-Type", "application/x-plist")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("User-Agent", "Podcasts/1.1.0 (Macintosh; OS X 27.0; 26A5353q) AppleWebKit/2625.1.18.11.5 AMS/1 (dt:1)")
}

// initSession calls getAll to mirror the native Podcasts app behaviour and to
// fetch the current server-side version for every key in the domain. These
// versions are stored in w.serverVersions and used as base-version in putAll,
// replacing the stale local SQLite Z_OPT values. Without current versions,
// putAll returns status=1198 whenever another device (e.g. iPhone) has synced
// since the last Mac DB update.
func (w *KVSWriter) initSession(ctx context.Context) error {
	if w.sessionReady {
		return nil
	}

	bodyXML := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n" +
		"\t<key>domain</key>\n\t<string>" + kvsDomain + "</string>\n" +
		"</dict>\n</plist>\n"

	body, err := xmlToBinaryPlist(bodyXML)
	if err != nil {
		return fmt.Errorf("getAll body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kvsGetEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("getAll request: %w", err)
	}
	w.setKVSHeaders(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("getAll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("getAll HTTP %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	w.serverVersions = parseServerVersions(ctx, respBody)
	w.sessionReady = true
	return nil
}

// parseServerVersions extracts metadataIdentifier→version pairs from a getAll
// binary plist response body. Returns an empty map on any parse failure.
func parseServerVersions(ctx context.Context, body []byte) map[string]int {
	versions := make(map[string]int)
	if len(body) == 0 {
		return versions
	}
	cmd := exec.CommandContext(ctx, "plutil", "-convert", "xml1", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	xmlOut, err := cmd.Output()
	if err != nil {
		return versions
	}
	s := string(xmlOut)

	// Narrow to the values array to avoid false matches on top-level keys.
	const valuesKey = "<key>values</key>"
	vi := strings.Index(s, valuesKey)
	if vi == -1 {
		return versions
	}
	s = s[vi+len(valuesKey):]
	arrayStart := strings.Index(s, "<array>")
	arrayEnd := strings.Index(s, "</array>")
	if arrayStart == -1 || arrayEnd == -1 || arrayEnd <= arrayStart {
		return versions
	}
	s = s[arrayStart+len("<array>") : arrayEnd]

	// Parse each <dict> block: extract the "key" string and "version" string.
	for {
		dictStart := strings.Index(s, "<dict>")
		dictEnd := strings.Index(s, "</dict>")
		if dictStart == -1 || dictEnd == -1 || dictEnd <= dictStart {
			break
		}
		block := s[dictStart+len("<dict>") : dictEnd]
		s = s[dictEnd+len("</dict>"):]

		metaID := xmlStringAfter(block, "<key>key</key>")
		verStr := xmlStringAfter(block, "<key>version</key>")
		if metaID == "" || verStr == "" {
			continue
		}
		if v, err := strconv.Atoi(verStr); err == nil {
			versions[metaID] = v
		}
	}
	return versions
}

// xmlStringAfter returns the content of the first <string>…</string> element
// that follows the literal tag within s.
func xmlStringAfter(s, tag string) string {
	i := strings.Index(s, tag)
	if i == -1 {
		return ""
	}
	after := strings.TrimSpace(s[i+len(tag):])
	after = strings.TrimPrefix(after, "<string>")
	if after == s[i+len(tag):] { // no <string> prefix found
		return ""
	}
	return strings.SplitN(after, "<", 2)[0]
}

// kvsBatchSize is the maximum number of episodes per putAll request.
// The native app sends one episode per call; the protocol supports arrays but
// the server's payload limit is undocumented. 25 is a conservative default
// that keeps each request well under any reasonable limit while still being
// far more efficient than one-at-a-time for bulk migrations.
const kvsBatchSize = 25

// putAll sends all items to the KVS, chunked into groups of kvsBatchSize.
// It calls initSession (getAll) first to obtain current server-side versions,
// then overwrites each item's UPPVersion with the server value before sending.
func (w *KVSWriter) putAll(ctx context.Context, items []kvsItem) error {
	if err := w.initSession(ctx); err != nil {
		fmt.Printf("apple/kvs: session init (getAll) failed: %v\n", err)
	}
	// Use server-side versions as base-version to avoid 1198 conflicts.
	// The local SQLite Z_OPT is stale whenever another device has synced.
	for i := range items {
		if v, ok := w.serverVersions[items[i].MetadataIdentifier]; ok {
			items[i].UPPVersion = v
		}
	}
	for len(items) > 0 {
		n := kvsBatchSize
		if n > len(items) {
			n = len(items)
		}
		if err := w.sendChunk(ctx, items[:n]); err != nil {
			return err
		}
		items = items[n:]
	}
	return nil
}

// sendChunk sends a single putAll HTTP request for the given items (≤ kvsBatchSize).
func (w *KVSWriter) sendChunk(ctx context.Context, items []kvsItem) error {
	body, err := buildPutAllBody(items)
	if err != nil {
		return fmt.Errorf("build body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kvsEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	w.setKVSHeaders(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("putAll HTTP: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("putAll HTTP %d: %s", resp.StatusCode, respBody)
	}

	// The response is a binary plist; convert to XML with plutil to check status.
	cmd := exec.CommandContext(ctx, "plutil", "-convert", "xml1", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(respBody)
	xmlOut, err := cmd.Output()
	if err == nil && bytes.Contains(xmlOut, []byte("<key>status</key>")) {
		// Crude parse: check for <integer>-2</integer> adjacent to the status key.
		s := string(xmlOut)
		if idx := strings.Index(s, "<key>status</key>"); idx != -1 {
			after := s[idx+len("<key>status</key>"):]
			after = strings.TrimSpace(after)
			if strings.HasPrefix(after, "<integer>") {
				after = strings.TrimPrefix(after, "<integer>")
				statusStr := strings.SplitN(after, "<", 2)[0]
				if statusStr != "0" {
					return fmt.Errorf("putAll returned status=%s (session may have expired — reopen Apple Podcasts)", statusStr)
				}
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Plist body construction
// ---------------------------------------------------------------------------

// buildPutAllBody builds the binary plist body for a putAll request.
// Uses plutil to convert XML → binary (avoids a library dependency).
func buildPutAllBody(items []kvsItem) ([]byte, error) {
	var entries []string
	for _, item := range items {
		value, err := buildItemValue(item)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", item.MetadataIdentifier, err)
		}
		entries = append(entries, fmt.Sprintf(
			"\t\t<dict>\n"+
				"\t\t\t<key>base-version</key>\n"+
				"\t\t\t<string>%d</string>\n"+
				"\t\t\t<key>key</key>\n"+
				"\t\t\t<string>%s</string>\n"+
				"\t\t\t<key>value</key>\n"+
				"\t\t\t<data>%s</data>\n"+
				"\t\t</dict>",
			item.UPPVersion,
			item.MetadataIdentifier,
			base64.StdEncoding.EncodeToString(value),
		))
	}

	xmlPlist := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n" +
		"<dict>\n" +
		"\t<key>domain</key>\n" +
		"\t<string>" + kvsDomain + "</string>\n" +
		"\t<key>keys</key>\n" +
		"\t<array>\n" +
		strings.Join(entries, "\n") + "\n" +
		"\t</array>\n" +
		"</dict>\n" +
		"</plist>\n"

	return xmlToBinaryPlist(xmlPlist)
}

// buildItemValue builds the DEFLATE-compressed binary plist value for one item.
func buildItemValue(item kvsItem) ([]byte, error) {
	hbplTag := "<false/>"
	if item.HasBeenPlayed {
		hbplTag = "<true/>"
	}
	// Keys must be alphabetically sorted for a binary plist dict.
	xmlPlist := fmt.Sprintf(
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"+
			"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" "+
			"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"+
			"<plist version=\"1.0\">\n"+
			"<dict>\n"+
			"\t<key>bktm</key>\n"+
			"\t<real>%.17g</real>\n"+
			"\t<key>hbpl</key>\n"+
			"\t%s\n"+
			"\t<key>plct</key>\n"+
			"\t<integer>%d</integer>\n"+
			"\t<key>tstm</key>\n"+
			"\t<real>%.17g</real>\n"+
			"</dict>\n"+
			"</plist>\n",
		item.BookmarkTimeSec, hbplTag, item.PlayCount, item.TimestampSec,
	)

	innerBinary, err := xmlToBinaryPlist(xmlPlist)
	if err != nil {
		return nil, fmt.Errorf("inner plist: %w", err)
	}

	// Raw DEFLATE — no zlib header, matching the native Podcasts app's encoding.
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("deflate init: %w", err)
	}
	if _, err := fw.Write(innerBinary); err != nil {
		fw.Close()
		return nil, fmt.Errorf("deflate write: %w", err)
	}
	if err := fw.Close(); err != nil {
		return nil, fmt.Errorf("deflate close: %w", err)
	}
	return buf.Bytes(), nil
}

// xmlToBinaryPlist converts an XML property list string to binary plist format
// using plutil, which is always available on macOS.
func xmlToBinaryPlist(xmlContent string) ([]byte, error) {
	cmd := exec.Command("plutil", "-convert", "binary1", "-o", "-", "-")
	cmd.Stdin = strings.NewReader(xmlContent)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("plutil: %s", exitErr.Stderr)
		}
		return nil, fmt.Errorf("plutil: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Cookie store parsing
// ---------------------------------------------------------------------------

// parsePodcastCookies reads an NSHTTPCookieStorage binarycookies file and
// returns the iTunes Store DSID and a Cookie: header value for
// bookkeeper.itunes.apple.com requests.
//
// The binarycookies format is Apple's custom binary cookie storage:
//
//	"cook" magic (4 bytes)
//	num_pages uint32 BE
//	page_sizes []uint32 BE
//	pages (each contains cookie records)
func parsePodcastCookies(path string) (dsid, cookieHeader string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if len(data) < 8 || string(data[:4]) != "cook" {
		n := 4
		if len(data) < n {
			n = len(data)
		}
		return "", "", fmt.Errorf("not a binarycookies file (magic=%q)", data[:n])
	}

	numPages := int(binary.BigEndian.Uint32(data[4:8]))
	if len(data) < 8+numPages*4 {
		return "", "", fmt.Errorf("binarycookies truncated")
	}

	pageSizes := make([]int, numPages)
	for i := range pageSizes {
		pageSizes[i] = int(binary.BigEndian.Uint32(data[8+i*4 : 12+i*4]))
	}

	type cookie struct{ name, value, domain string }
	var cookies []cookie

	offset := 8 + numPages*4
	for _, pageSize := range pageSizes {
		if offset+pageSize > len(data) {
			break
		}
		page := data[offset : offset+pageSize]
		offset += pageSize

		if len(page) < 8 {
			continue
		}
		numCookies := int(binary.LittleEndian.Uint32(page[4:8]))

		for i := 0; i < numCookies; i++ {
			offIdx := 8 + i*4
			if offIdx+4 > len(page) {
				break
			}
			ckOffset := int(binary.LittleEndian.Uint32(page[offIdx : offIdx+4]))
			// Cookie record layout (little-endian):
			//   +0  size (4 bytes)
			//   +4  unknown (4 bytes)
			//   +8  flags (4 bytes)
			//   +12 unknown (4 bytes)
			//   +16 domain_offset from record start (4 bytes)
			//   +20 name_offset   from record start (4 bytes)
			//   +24 path_offset   from record start (4 bytes)
			//   +28 value_offset  from record start (4 bytes)
			//   +32 end marker    (8 bytes)
			//   +40 expiry        (float64 LE, CoreData epoch)
			//   +48 creation      (float64 LE, CoreData epoch)
			//   +56+ null-terminated strings
			if ckOffset+32 > len(page) {
				continue
			}
			ck := page[ckOffset:]

			domainOff := int(binary.LittleEndian.Uint32(ck[16:20]))
			nameOff := int(binary.LittleEndian.Uint32(ck[20:24]))
			valueOff := int(binary.LittleEndian.Uint32(ck[28:32]))

			domain := nullTermString(ck, domainOff)
			name := nullTermString(ck, nameOff)
			value := nullTermString(ck, valueOff)

			if strings.Contains(strings.ToLower(domain), "apple.com") {
				cookies = append(cookies, cookie{name: name, value: value, domain: domain})
			}
		}
	}

	var parts []string
	for _, c := range cookies {
		parts = append(parts, c.name+"="+c.value)
		if c.name == "X-Dsid" {
			dsid = c.value
		}
	}
	return dsid, strings.Join(parts, "; "), nil
}

func nullTermString(data []byte, offset int) string {
	if offset >= len(data) {
		return ""
	}
	end := bytes.IndexByte(data[offset:], 0)
	if end < 0 {
		return string(data[offset:])
	}
	return string(data[offset : offset+end])
}

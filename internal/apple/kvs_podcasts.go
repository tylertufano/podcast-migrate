package apple

// kvs_podcasts.go handles the com.apple.podcasts KVS domain, which stores:
//   - playState:<feedURL>   — per-feed episode play state (isNew, lastDatePlayed,
//                            metadataIdentifier, …)
//   - podcastSubscriptions-2012-09-04  — full subscription list
//
// Unlike com.apple.upp (one key per episode), this domain stores the entire
// episode list for a feed under a single key, keyed by the feed URL. This
// gives us the metadataIdentifier for all known episodes without reading the
// local SQLite database.

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"howett.net/plist"
)

const (
	kvsPodcastsDomain  = "com.apple.podcasts"
	kvsPlayStatePrefix = "playState:"
	kvsSubKey          = "podcastSubscriptions-2012-09-04"
)

// playStateEp is one episode entry within a playState:<feedURL> value.
type playStateEp struct {
	GUID                 string  `json:"guid"`
	IsNew                bool    `json:"isNew"`
	LastDatePlayed       float64 `json:"lastDatePlayed"`        // CoreData epoch seconds
	LastUserMarkedPlayed float64 `json:"lastUserMarkedAsPlayedDate"`
	MetadataIdentifier   string  `json:"metadataIdentifier"`
	PlayStateManuallySet bool    `json:"playStateManuallySet"`
}

// playStateFeed is one playState:<feedURL> entry parsed from getAll.
type playStateFeed struct {
	FeedURL     string
	Version     string
	Episodes    []playStateEp
	DataVersion int
	modified    bool
	guidIdx     map[string]int // guid → index in Episodes (lazy-built)
}

func (f *playStateFeed) buildIdx() {
	if f.guidIdx != nil {
		return
	}
	f.guidIdx = make(map[string]int, len(f.Episodes))
	for i, ep := range f.Episodes {
		f.guidIdx[ep.GUID] = i
	}
}

// findByGUID returns a pointer into f.Episodes for the given guid.
func (f *playStateFeed) findByGUID(guid string) *playStateEp {
	f.buildIdx()
	i, ok := f.guidIdx[guid]
	if !ok {
		return nil
	}
	return &f.Episodes[i]
}

// podcastSubscription is one entry in podcastSubscriptions-2012-09-04.
type podcastSubscription struct {
	UUID                   string    `json:"uuid"`
	Subscribed             int       `json:"subscribed"` // 1=subscribed, 0=unsubscribed
	FeedURL                string    `json:"feedURL"`
	Title                  string    `json:"title"`
	PodcastPID             int64     `json:"podcastPID,omitempty"`
	StoreCollectionID      int64     `json:"storeCollectionId,omitempty"`
	AddedDate              time.Time `json:"addedDate"`
	LastTouchDate          time.Time `json:"lastTouchDate"`
	UpdatedDate            time.Time `json:"updatedDate"`
	DarkCount              int       `json:"darkCount"`
	PlaybackNewestToOldest bool      `json:"playbackNewestToOldest"`
	SortAscending          bool      `json:"sortAscending"`
	ShowTypeSetting        int       `json:"showTypeSetting"`
}

// ---------------------------------------------------------------------------
// initPodcastsDomain — getAll for com.apple.podcasts
// ---------------------------------------------------------------------------

// initPodcastsDomain calls getAll for com.apple.podcasts and caches the
// results in w.podcastsFeeds and w.subscriptions. Idempotent.
func (w *KVSWriter) initPodcastsDomain(ctx context.Context) error {
	if w.podcastsDomainReady {
		return nil
	}

	bodyXML := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n" +
		"\t<key>domain</key>\n\t<string>" + kvsPodcastsDomain + "</string>\n" +
		"</dict>\n</plist>\n"

	body, err := xmlToBinaryPlist(bodyXML)
	if err != nil {
		return fmt.Errorf("kvs podcasts domain getAll body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kvsGetEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("kvs podcasts domain getAll request: %w", err)
	}
	w.setKVSHeaders(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kvs podcasts domain getAll: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kvs podcasts domain getAll HTTP %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	feeds, subs, subVer, err := parsePodcastsDomainGetAll(ctx, respBody)
	if err != nil {
		return fmt.Errorf("kvs podcasts domain parse: %w", err)
	}

	w.podcastsFeeds = feeds
	w.subscriptions = subs
	w.subVersion = subVer
	w.podcastsDomainReady = true
	return nil
}

// parsePodcastsDomainGetAll parses the binary plist response from
// getAll(com.apple.podcasts). Returns a map of feedURL→playStateFeed (for
// all playState: keys), the subscription list, and the subscription key version.
func parsePodcastsDomainGetAll(ctx context.Context, body []byte) (
	feeds map[string]*playStateFeed,
	subs []podcastSubscription,
	subVersion string,
	err error,
) {
	feeds = make(map[string]*playStateFeed)

	// Convert binary plist → XML once.
	xmlOut, err := bplistToXML(ctx, body)
	if err != nil {
		return nil, nil, "", fmt.Errorf("bplist to xml: %w", err)
	}
	s := xmlOut

	// Advance to the values array.
	const valuesKey = "<key>values</key>"
	vi := strings.Index(s, valuesKey)
	if vi == -1 {
		return feeds, subs, subVersion, nil
	}
	s = s[vi+len(valuesKey):]
	arrayStart := strings.Index(s, "<array>")
	arrayEnd := strings.LastIndex(s, "</array>")
	if arrayStart == -1 || arrayEnd == -1 || arrayEnd <= arrayStart {
		return feeds, subs, subVersion, nil
	}
	s = s[arrayStart+len("<array>") : arrayEnd]

	// Parse each <dict> in the values array.
	for {
		dictStart := strings.Index(s, "<dict>")
		dictEnd := strings.Index(s, "</dict>")
		if dictStart == -1 || dictEnd == -1 || dictEnd <= dictStart {
			break
		}
		block := s[dictStart+len("<dict>") : dictEnd]
		s = s[dictEnd+len("</dict>"):]

		key := xmlStringAfter(block, "<key>key</key>")
		version := xmlStringAfter(block, "<key>version</key>")
		rawData := xmlDataAfter(block, "<key>value</key>")
		if key == "" || len(rawData) == 0 {
			continue
		}

		// Decompress the raw value (raw DEFLATE, no zlib header).
		inner, decompErr := deflateDecompress(rawData)
		if decompErr != nil {
			continue // skip non-DEFLATE values (e.g., bk-ordinals-v1)
		}

		switch {
		case strings.HasPrefix(key, kvsPlayStatePrefix):
			feedURL := key[len(kvsPlayStatePrefix):]
			eps, dataVer, parseErr := parsePlayStateInner(ctx, inner)
			if parseErr != nil {
				continue
			}
			feeds[feedURL] = &playStateFeed{
				FeedURL:     feedURL,
				Version:     version,
				Episodes:    eps,
				DataVersion: dataVer,
			}

		case key == kvsSubKey:
			parsedSubs, parseErr := parseSubscriptionInner(ctx, inner)
			if parseErr == nil {
				subs = parsedSubs
				subVersion = version
			}
		}
	}

	return feeds, subs, subVersion, nil
}

// parsePlayStateInner decodes the DEFLATE-decompressed binary plist for a
// playState: value and returns its episodes and DataVersion.
func parsePlayStateInner(ctx context.Context, bplistData []byte) ([]playStateEp, int, error) {
	jsonData, err := bplistToJSON(ctx, bplistData)
	if err != nil {
		return nil, 0, err
	}
	var raw struct {
		Episodes    []playStateEp `json:"2"`
		DataVersion int           `json:"DataVersion"`
	}
	if err := json.Unmarshal(jsonData, &raw); err != nil {
		return nil, 0, err
	}
	return raw.Episodes, raw.DataVersion, nil
}

// ---------------------------------------------------------------------------
// Subscription parsing — XML-based
// ---------------------------------------------------------------------------
// plutil -convert json cannot represent NSDate values, so we use
// plutil -convert xml1 and parse the plist XML by hand instead of JSON.

func parseSubscriptionInner(ctx context.Context, bplistData []byte) ([]podcastSubscription, error) {
	xmlData, err := bplistToXML(ctx, bplistData)
	if err != nil {
		return nil, err
	}
	return parseSubscriptionXML(xmlData)
}

// parseSubscriptionXML parses the XML plist produced by bplistToXML for a
// podcastSubscriptions-2012-09-04 value. The outer structure is:
//
//	{ "2": [ {subscription dict}, ... ], "DataVersion": 2 }
func parseSubscriptionXML(xmlStr string) ([]podcastSubscription, error) {
	// Find the <array> under key "2".
	idx := strings.Index(xmlStr, "<key>2</key>")
	if idx == -1 {
		return nil, nil
	}
	after := xmlStr[idx+len("<key>2</key>"):]
	arrStart := strings.Index(after, "<array>")
	// Use LastIndex to grab the closing </array> of this specific array.
	arrEnd := strings.Index(after, "</array>")
	if arrStart == -1 || arrEnd == -1 || arrEnd <= arrStart {
		return nil, nil
	}
	content := after[arrStart+len("<array>") : arrEnd]

	var subs []podcastSubscription
	for {
		dStart := strings.Index(content, "<dict>")
		dEnd := strings.Index(content, "</dict>")
		if dStart == -1 || dEnd == -1 || dEnd <= dStart {
			break
		}
		block := content[dStart+len("<dict>") : dEnd]
		content = content[dEnd+len("</dict>"):]
		subs = append(subs, parseOneSubscriptionBlock(block))
	}
	return subs, nil
}

// parseOneSubscriptionBlock extracts all known fields from a single
// subscription <dict> block in the XML plist.
func parseOneSubscriptionBlock(block string) podcastSubscription {
	s := podcastSubscription{
		UUID:                   xmlStringAfter(block, "<key>uuid</key>"),
		FeedURL:                xmlStringAfter(block, "<key>feedURL</key>"),
		Title:                  xmlStringAfter(block, "<key>title</key>"),
		DarkCount:              xmlIntAfterKey(block, "darkCount"),
		ShowTypeSetting:        xmlIntAfterKey(block, "showTypeSetting"),
		PodcastPID:             xmlInt64AfterKey(block, "podcastPID"),
		StoreCollectionID:      xmlInt64AfterKey(block, "storeCollectionId"),
		PlaybackNewestToOldest: xmlBoolAfterKey(block, "playbackNewestToOldest"),
		SortAscending:          xmlBoolAfterKey(block, "sortAscending"),
	}
	// subscribed may be stored as <true/>/<false/> or <integer>1</integer>.
	if xmlBoolAfterKey(block, "subscribed") || xmlIntAfterKey(block, "subscribed") == 1 {
		s.Subscribed = 1
	}
	s.AddedDate, _ = parseKVSDate(xmlDateAfterKey(block, "addedDate"))
	s.LastTouchDate, _ = parseKVSDate(xmlDateAfterKey(block, "lastTouchDate"))
	s.UpdatedDate, _ = parseKVSDate(xmlDateAfterKey(block, "updatedDate"))
	return s
}

// parseKVSDate parses an ISO 8601 date string from plist XML.
func parseKVSDate(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Now().UTC(), fmt.Errorf("cannot parse KVS date %q", s)
}

// xmlIntAfterKey returns the integer value after <key>name</key><integer>N</integer>.
func xmlIntAfterKey(s, name string) int {
	tag := "<key>" + name + "</key>"
	i := strings.Index(s, tag)
	if i == -1 {
		return 0
	}
	after := strings.TrimSpace(s[i+len(tag):])
	if !strings.HasPrefix(after, "<integer>") {
		return 0
	}
	v := strings.TrimPrefix(after, "<integer>")
	v = strings.SplitN(v, "<", 2)[0]
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

// xmlInt64AfterKey returns the int64 value after <key>name</key><integer>N</integer>.
func xmlInt64AfterKey(s, name string) int64 {
	tag := "<key>" + name + "</key>"
	i := strings.Index(s, tag)
	if i == -1 {
		return 0
	}
	after := strings.TrimSpace(s[i+len(tag):])
	if !strings.HasPrefix(after, "<integer>") {
		return 0
	}
	v := strings.TrimPrefix(after, "<integer>")
	v = strings.SplitN(v, "<", 2)[0]
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

// xmlBoolAfterKey returns true if <key>name</key><true/> is present (false for <false/> or absent).
func xmlBoolAfterKey(s, name string) bool {
	tag := "<key>" + name + "</key>"
	i := strings.Index(s, tag)
	if i == -1 {
		return false
	}
	after := strings.TrimSpace(s[i+len(tag):])
	return strings.HasPrefix(after, "<true/>")
}

// xmlDateAfterKey returns the ISO 8601 string from <key>name</key><date>...</date>.
func xmlDateAfterKey(s, name string) string {
	tag := "<key>" + name + "</key>"
	i := strings.Index(s, tag)
	if i == -1 {
		return ""
	}
	after := strings.TrimSpace(s[i+len(tag):])
	if !strings.HasPrefix(after, "<date>") {
		return ""
	}
	v := strings.TrimPrefix(after, "<date>")
	v = strings.SplitN(v, "<", 2)[0]
	return strings.TrimSpace(v)
}

// ---------------------------------------------------------------------------
// Episode lookup via play state (no SQLite required)
// ---------------------------------------------------------------------------

// lookupEpisodeViaPlayState finds the metadataIdentifier for an episode using
// the cached com.apple.podcasts play state data. Returns ("", false) when the
// episode is not found (feed not subscribed or episode not yet indexed).
func (w *KVSWriter) lookupEpisodeViaPlayState(feedURL, guid string) (metadataIdentifier string, ok bool) {
	if !w.podcastsDomainReady || guid == "" || feedURL == "" {
		return "", false
	}
	feed := w.podcastsFeeds[feedURL]
	if feed == nil {
		return "", false
	}
	ep := feed.findByGUID(guid)
	if ep == nil || ep.MetadataIdentifier == "" {
		return "", false
	}
	return ep.MetadataIdentifier, true
}

// ---------------------------------------------------------------------------
// Subscription management
// ---------------------------------------------------------------------------

// IsSubscribed reports whether feedURL is currently in the subscription list
// with subscribed=1. Requires initPodcastsDomain to have been called first.
func (w *KVSWriter) IsSubscribed(feedURL string) bool {
	for _, s := range w.subscriptions {
		if s.FeedURL == feedURL && s.Subscribed == 1 {
			return true
		}
	}
	return false
}

// Subscribe adds feedURL to the subscription list. Returns true if a new
// subscription was created (or an unsubscribed entry was re-enabled), false
// if the feed was already subscribed. Writes the updated list to the KVS.
func (w *KVSWriter) Subscribe(ctx context.Context, feedURL, title string) (bool, error) {
	if err := w.initPodcastsDomain(ctx); err != nil {
		return false, err
	}
	// Already subscribed — no-op.
	for i, s := range w.subscriptions {
		if s.FeedURL == feedURL {
			if s.Subscribed == 1 {
				return false, nil
			}
			// Was unsubscribed — flip it back.
			w.subscriptions[i].Subscribed = 1
			w.subscriptions[i].UpdatedDate = time.Now().UTC()
			w.subscriptions[i].LastTouchDate = time.Now().UTC()
			return true, w.putSubscriptions(ctx)
		}
	}

	// New subscription.
	now := time.Now().UTC()
	w.subscriptions = append(w.subscriptions, podcastSubscription{
		UUID:                   newUUID(),
		Subscribed:             1,
		FeedURL:                feedURL,
		Title:                  title,
		DarkCount:              0,
		PlaybackNewestToOldest: true,
		SortAscending:          false,
		ShowTypeSetting:        1,
		AddedDate:              now,
		LastTouchDate:          now,
		UpdatedDate:            now,
	})
	if err := w.putSubscriptions(ctx); err != nil {
		return false, err
	}
	if w.newlySubscribed == nil {
		w.newlySubscribed = make(map[string]time.Time)
	}
	w.newlySubscribed[feedURL] = now
	return true, nil
}

// Unsubscribe sets subscribed=0 for feedURL in the subscription list.
func (w *KVSWriter) Unsubscribe(ctx context.Context, feedURL string) error {
	if err := w.initPodcastsDomain(ctx); err != nil {
		return err
	}
	found := false
	for i, s := range w.subscriptions {
		if s.FeedURL == feedURL && s.Subscribed == 1 {
			w.subscriptions[i].Subscribed = 0
			w.subscriptions[i].UpdatedDate = time.Now().UTC()
			w.subscriptions[i].LastTouchDate = time.Now().UTC()
			found = true
		}
	}
	if !found {
		return nil // already unsubscribed or not present
	}
	return w.putSubscriptions(ctx)
}

// putSubscriptions writes the current subscription list to the KVS.
func (w *KVSWriter) putSubscriptions(ctx context.Context) error {
	// Refuse to write without a valid base-version: we'd risk overwriting the
	// entire subscription list with a stale snapshot if parsing failed earlier.
	if w.subVersion == "" {
		return fmt.Errorf("kvs: cannot write subscriptions — server version unknown (subscription list may not have parsed correctly)")
	}

	value, err := buildSubscriptionValue(w.subscriptions)
	if err != nil {
		return fmt.Errorf("encode subscriptions: %w", err)
	}

	ver := w.subVersion
	xmlPlist := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n" +
		"\t<key>domain</key>\n\t<string>" + kvsPodcastsDomain + "</string>\n" +
		"\t<key>keys</key>\n\t<array>\n" +
		"\t\t<dict>\n" +
		"\t\t\t<key>base-version</key>\n\t\t\t<string>" + ver + "</string>\n" +
		"\t\t\t<key>key</key>\n\t\t\t<string>" + kvsSubKey + "</string>\n" +
		"\t\t\t<key>value</key>\n\t\t\t<data>" + base64.StdEncoding.EncodeToString(value) + "</data>\n" +
		"\t\t</dict>\n" +
		"\t</array>\n" +
		"</dict>\n</plist>\n"

	body, err := xmlToBinaryPlist(xmlPlist)
	if err != nil {
		return fmt.Errorf("encode putAll body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kvsEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	w.setKVSHeaders(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("putSubscriptions: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("putSubscriptions HTTP %d: %s", resp.StatusCode, respBody)
	}

	// Parse the binary plist response to check the status and get the new version.
	xmlOut, xmlErr := bplistToXML(ctx, respBody)
	if xmlErr != nil {
		return fmt.Errorf("putSubscriptions: cannot parse server response: %w", xmlErr)
	}
	status := xmlIntAfterKey(xmlOut, "status")
	if status != 0 {
		return fmt.Errorf("putSubscriptions: server returned status %d", status)
	}
	// Extract updated version from the values array in the response.
	if idx := strings.Index(xmlOut, "<key>version</key>"); idx != -1 {
		after := strings.TrimSpace(xmlOut[idx+len("<key>version</key>"):])
		if strings.HasPrefix(after, "<string>") {
			newVer := strings.TrimPrefix(after, "<string>")
			newVer = strings.SplitN(newVer, "<", 2)[0]
			if newVer != "" {
				w.subVersion = newVer
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Encoding helpers
// ---------------------------------------------------------------------------

// buildSubscriptionValue encodes a subscription list as DEFLATE-compressed
// binary plist matching the podcastSubscriptions-2012-09-04 format.
func buildSubscriptionValue(subs []podcastSubscription) ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n" +
		"\t<key>2</key>\n\t<array>\n")

	for _, s := range subs {
		sb.WriteString("\t\t<dict>\n")
		sb.WriteString("\t\t\t<key>addedDate</key>\n\t\t\t<date>" + s.AddedDate.UTC().Format(time.RFC3339) + "</date>\n")
		sb.WriteString("\t\t\t<key>darkCount</key>\n\t\t\t<integer>" + strconv.Itoa(s.DarkCount) + "</integer>\n")
		sb.WriteString("\t\t\t<key>feedURL</key>\n\t\t\t<string>" + xmlEscape(s.FeedURL) + "</string>\n")
		sb.WriteString("\t\t\t<key>lastTouchDate</key>\n\t\t\t<date>" + s.LastTouchDate.UTC().Format(time.RFC3339) + "</date>\n")
		if s.PlaybackNewestToOldest {
			sb.WriteString("\t\t\t<key>playbackNewestToOldest</key>\n\t\t\t<true/>\n")
		} else {
			sb.WriteString("\t\t\t<key>playbackNewestToOldest</key>\n\t\t\t<false/>\n")
		}
		if s.PodcastPID > 0 {
			sb.WriteString("\t\t\t<key>podcastPID</key>\n\t\t\t<integer>" + strconv.FormatInt(s.PodcastPID, 10) + "</integer>\n")
		}
		sb.WriteString("\t\t\t<key>showTypeSetting</key>\n\t\t\t<integer>" + strconv.Itoa(s.ShowTypeSetting) + "</integer>\n")
		if s.SortAscending {
			sb.WriteString("\t\t\t<key>sortAscending</key>\n\t\t\t<true/>\n")
		} else {
			sb.WriteString("\t\t\t<key>sortAscending</key>\n\t\t\t<false/>\n")
		}
		if s.StoreCollectionID > 0 {
			sb.WriteString("\t\t\t<key>storeCollectionId</key>\n\t\t\t<integer>" + strconv.FormatInt(s.StoreCollectionID, 10) + "</integer>\n")
		}
		if s.Subscribed == 1 {
			sb.WriteString("\t\t\t<key>subscribed</key>\n\t\t\t<true/>\n")
		} else {
			sb.WriteString("\t\t\t<key>subscribed</key>\n\t\t\t<false/>\n")
		}
		sb.WriteString("\t\t\t<key>title</key>\n\t\t\t<string>" + xmlEscape(s.Title) + "</string>\n")
		sb.WriteString("\t\t\t<key>updatedDate</key>\n\t\t\t<date>" + s.UpdatedDate.UTC().Format(time.RFC3339) + "</date>\n")
		sb.WriteString("\t\t\t<key>uuid</key>\n\t\t\t<string>" + s.UUID + "</string>\n")
		sb.WriteString("\t\t</dict>\n")
	}

	sb.WriteString("\t</array>\n")
	sb.WriteString("\t<key>DataVersion</key>\n\t<integer>2</integer>\n")
	sb.WriteString("</dict>\n</plist>\n")

	bplist, err := xmlToBinaryPlist(sb.String())
	if err != nil {
		return nil, fmt.Errorf("subscription plist: %w", err)
	}
	return deflateCompress(bplist)
}

// ---------------------------------------------------------------------------
// Low-level utilities
// ---------------------------------------------------------------------------

// bplistToXML converts a binary plist to its XML string representation.
// Uses howett.net/plist — pure Go, works on all platforms.
func bplistToXML(_ context.Context, data []byte) (string, error) {
	var v interface{}
	if _, err := plist.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("plist decode: %w", err)
	}
	out, err := plist.MarshalIndent(v, plist.XMLFormat, "\t")
	if err != nil {
		return "", fmt.Errorf("plist encode xml: %w", err)
	}
	return string(out), nil
}

// bplistToJSON converts a binary plist to JSON.
// Uses howett.net/plist — pure Go, works on all platforms.
func bplistToJSON(_ context.Context, data []byte) ([]byte, error) {
	var v interface{}
	if _, err := plist.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("plist decode: %w", err)
	}
	return json.Marshal(v)
}

// deflateDecompress decompresses raw DEFLATE data (no zlib header).
func deflateDecompress(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	return io.ReadAll(r)
}

// deflateCompress compresses data with raw DEFLATE (no zlib header).
func deflateCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		fw.Close()
		return nil, err
	}
	if err := fw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// xmlDataAfter returns the raw (base64-decoded) bytes from the first <data>
// element following tag within s.
func xmlDataAfter(s, tag string) []byte {
	i := strings.Index(s, tag)
	if i == -1 {
		return nil
	}
	after := s[i+len(tag):]
	start := strings.Index(after, "<data>")
	if start == -1 {
		return nil
	}
	after = after[start+len("<data>"):]
	end := strings.Index(after, "</data>")
	if end == -1 {
		return nil
	}
	b64 := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			return -1 // drop whitespace
		}
		return r
	}, after[:end])
	decoded, _ := base64.StdEncoding.DecodeString(b64)
	return decoded
}

// xmlEscape returns s with XML special characters escaped.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// newUUID generates a random UUID v4 string in upper-case format.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("kvs: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}

package apple

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// iTunesLookupResult holds metadata for one podcast returned by the iTunes
// Store lookup API.
type iTunesLookupResult struct {
	FeedURL  string
	Title    string
	Author   string
	ImageURL string
}

// batchITunesLookup resolves canonical feed URLs for a list of iTunes Store
// podcast IDs using the iTunes lookup API. Results are keyed by PID; entries
// without a feedUrl in the response are omitted.
//
// The API accepts up to 200 IDs per request; larger batches are split
// automatically. Partial failures on individual batches are returned as errors.
func batchITunesLookup(ctx context.Context, client *http.Client, pids []int64) (map[int64]iTunesLookupResult, error) {
	const batchSize = 200
	out := make(map[int64]iTunesLookupResult, len(pids))

	for start := 0; start < len(pids); start += batchSize {
		end := start + batchSize
		if end > len(pids) {
			end = len(pids)
		}
		batch := pids[start:end]

		ids := make([]string, len(batch))
		for i, pid := range batch {
			ids[i] = strconv.FormatInt(pid, 10)
		}
		lookupURL := "https://itunes.apple.com/lookup?id=" + strings.Join(ids, ",") + "&media=podcast&entity=podcast"

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupURL, nil)
		if err != nil {
			return nil, fmt.Errorf("itunes lookup: build request: %w", err)
		}
		req.Header.Set("User-Agent", "podcast-migrate/1.0 (+https://github.com/tylertufano/podcast-migrate)")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("itunes lookup: request: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("itunes lookup: read: %w", readErr)
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("itunes lookup: HTTP %d", resp.StatusCode)
		}

		var payload struct {
			Results []struct {
				CollectionID int64  `json:"collectionId"`
				FeedURL      string `json:"feedUrl"`
				Title        string `json:"collectionName"`
				Author       string `json:"artistName"`
				ImageURL     string `json:"artworkUrl600"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("itunes lookup: parse: %w", err)
		}

		for _, r := range payload.Results {
			if r.FeedURL != "" && r.CollectionID > 0 {
				out[r.CollectionID] = iTunesLookupResult{
					FeedURL:  r.FeedURL,
					Title:    r.Title,
					Author:   r.Author,
					ImageURL: r.ImageURL,
				}
			}
		}
	}
	return out, nil
}


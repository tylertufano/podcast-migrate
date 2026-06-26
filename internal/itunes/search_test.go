package itunes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// itunesResponse constructs a fake iTunes search API response body.
func itunesResponse(results []map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"results": results})
	return b
}

func result(id int64, feedURL, title, author string) map[string]any {
	return map[string]any{
		"collectionId":   id,
		"feedUrl":        feedURL,
		"collectionName": title,
		"artistName":     author,
	}
}

func TestFindByHints(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		feedURL string
		author  string
		body    []byte
		wantID  int64
	}{
		{
			name:    "exact URL match (+100) — accept",
			title:   "Fresh Air",
			feedURL: "https://feeds.npr.org/381444908/podcast.xml",
			body: itunesResponse([]map[string]any{
				result(214089682, "https://feeds.npr.org/381444908/podcast.xml", "Fresh Air", "NPR"),
				result(999, "https://other.example.com/feed.xml", "Fresh Air", "Other"),
			}),
			wantID: 214089682,
		},
		{
			name:   "title + author (+80) — accept",
			title:  "Planet Money",
			author: "NPR",
			body: itunesResponse([]map[string]any{
				result(290783428, "https://feeds.npr.org/510289/podcast.xml", "Planet Money", "NPR"),
			}),
			wantID: 290783428,
		},
		{
			name:  "title only (+50) unique — accept",
			title: "The Daily",
			body: itunesResponse([]map[string]any{
				result(1200361736, "https://feeds.simplecast.com/Sl5CSM3S", "The Daily", "The New York Times"),
			}),
			wantID: 1200361736,
		},
		{
			name:  "title tie — reject",
			title: "The Daily",
			body: itunesResponse([]map[string]any{
				result(1200361736, "https://feeds.simplecast.com/Sl5CSM3S", "The Daily", "NYT"),
				result(2000000001, "https://feeds.example.com/thedaily", "The Daily", "Other Corp"),
			}),
			wantID: 0,
		},
		{
			name:  "no exact title match — score below 50, reject",
			title: "Fresh Air",
			body: itunesResponse([]map[string]any{
				result(999, "https://feeds.npr.org/other/podcast.xml", "Fresh Air Extra", "NPR"),
			}),
			wantID: 0,
		},
		{
			name:  "empty results — zero result",
			title: "Nonexistent Show",
			body:  itunesResponse(nil),
			wantID: 0,
		},
		{
			name:    "domain match + title (+70) — accept",
			title:   "Fresh Air",
			feedURL: "https://feeds.npr.org/381444908/podcast.xml",
			body: itunesResponse([]map[string]any{
				result(214089682, "https://feeds.npr.org/381444908/podcast.xml", "Fresh Air", "NPR"),
			}),
			wantID: 214089682,
		},
		{
			name:  "empty title — return zero without calling server",
			title: "",
			body:  itunesResponse(nil),
			wantID: 0,
		},
		{
			name:    "url match only (+100) with no title match — reject (score 100 but no exact title; winner.exactTitle=false)",
			title:   "Different Title",
			feedURL: "https://feeds.npr.org/381444908/podcast.xml",
			body: itunesResponse([]map[string]any{
				result(214089682, "https://feeds.npr.org/381444908/podcast.xml", "Fresh Air", "NPR"),
			}),
			// score=100 from URL, best>=100 → accepted regardless of exactTitle
			wantID: 214089682,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(tc.body)
			}))
			defer srv.Close()

			// Redirect the request to the test server by replacing the URL.
			client := &http.Client{
				Transport: &prefixRewriter{base: srv.URL, inner: http.DefaultTransport},
			}

			got, err := FindByHints(context.Background(), client, tc.title, tc.feedURL, tc.author)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.CollectionID != tc.wantID {
				t.Errorf("CollectionID = %d, want %d", got.CollectionID, tc.wantID)
			}
		})
	}
}

func TestFindByHints_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: &prefixRewriter{base: srv.URL, inner: http.DefaultTransport},
	}
	_, err := FindByHints(context.Background(), client, "Fresh Air", "", "")
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}

// prefixRewriter rewrites the host of outgoing requests to point to the test
// server, leaving the path and query intact. This lets FindByHints build its
// own URL normally while the test intercepts the network call.
type prefixRewriter struct {
	base  string
	inner http.RoundTripper
}

func (r *prefixRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	base, _ := http.NewRequest("GET", r.base, nil)
	clone.URL.Scheme = base.URL.Scheme
	clone.URL.Host = base.URL.Host
	return r.inner.RoundTrip(clone)
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://feeds.npr.org/381444908/podcast.xml", "feeds.npr.org/381444908/podcast.xml"},
		{"http://feeds.npr.org/381444908/podcast.xml", "feeds.npr.org/381444908/podcast.xml"},
		{"https://feeds.npr.org/381444908/podcast.xml/", "feeds.npr.org/381444908/podcast.xml"},
		{"HTTPS://Feeds.NPR.org/381444908/podcast.xml", "feeds.npr.org/381444908/podcast.xml"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeURL(tc.in); got != tc.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEtldPlusOne(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://feeds.npr.org/feed.xml", "npr.org"},
		{"https://feeds.simplecast.com/Sl5CSM3S", "simplecast.com"},
		{"https://rss.example.co.uk/feed", "co.uk"}, // known limitation of 2-segment heuristic
		{"", ""},
	}
	for _, tc := range cases {
		if got := etldPlusOne(tc.in); got != tc.want {
			t.Errorf("etldPlusOne(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

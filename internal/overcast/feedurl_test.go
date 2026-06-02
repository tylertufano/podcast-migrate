package overcast

// White-box tests for normalizeFeedURL and for the URL-normalisation behaviour
// of buildOvercastIndex + findInOvercastIndex.

import (
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

func TestNormalizeFeedURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// http → https promotion
		{
			in:   "http://feeds.example.com/podcast",
			want: "https://feeds.example.com/podcast",
		},
		// trailing slash stripped
		{
			in:   "https://feeds.example.com/podcast/",
			want: "https://feeds.example.com/podcast",
		},
		// http + trailing slash (both normalised)
		{
			in:   "http://feeds.example.com/podcast/",
			want: "https://feeds.example.com/podcast",
		},
		// host lowercased, path case preserved (RFC 3986)
		{
			in:   "HTTPS://FEEDS.EXAMPLE.COM/MyPodcast",
			want: "https://feeds.example.com/MyPodcast",
		},
		// already normalised — no change
		{
			in:   "https://feeds.example.com/podcast",
			want: "https://feeds.example.com/podcast",
		},
		// query params preserved (some feeds use them for identity)
		{
			in:   "https://feeds.example.com/podcast?feed=rss2",
			want: "https://feeds.example.com/podcast?feed=rss2",
		},
		// root path — trailing slash kept (bare root)
		{
			in:   "https://feeds.example.com/",
			want: "https://feeds.example.com/",
		},
		// fragment stripped
		{
			in:   "https://feeds.example.com/podcast#section",
			want: "https://feeds.example.com/podcast",
		},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := normalizeFeedURL(c.in)
			if got != c.want {
				t.Errorf("normalizeFeedURL(%q)\n  got  %q\n  want %q", c.in, got, c.want)
			}
		})
	}
}

// TestBuildOvercastIndex_NormalisesURLs verifies that an episode indexed under
// an https:// Overcast feed URL is found when the Apple episode uses an http://
// URL for the same feed.
func TestBuildOvercastIndex_NormalisesURLs(t *testing.T) {
	pubDate := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	lib := &model.Library{
		Episodes: []model.EpisodeState{
			{
				GUID:      "overcast-id-99",
				FeedURL:   "https://feeds.example.com/show", // https in Overcast
				Title:     "URL Mismatch Episode",
				PubDate:   pubDate,
				PlayState: model.PlayStatePlayed,
			},
		},
	}
	index := buildOvercastIndex(lib)

	// Apple episode with http:// URL — should still find the entry.
	appleEp := model.EpisodeState{
		FeedURL: "http://feeds.example.com/show/", // http + trailing slash
		Title:   "URL Mismatch Episode",
		PubDate: pubDate,
	}
	entry, ok := findInOvercastIndex(index, appleEp, false)
	if !ok {
		t.Fatal("findInOvercastIndex: episode not found — URL normalisation failed")
	}
	if entry.numericID != "overcast-id-99" {
		t.Errorf("numericID: got %q, want %q", entry.numericID, "overcast-id-99")
	}
}

// TestFindInOvercastIndex_TrailingSlashMismatch verifies that a trailing slash
// difference between Apple and Overcast feed URLs does not break matching.
func TestFindInOvercastIndex_TrailingSlashMismatch(t *testing.T) {
	pubDate := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	lib := &model.Library{
		Episodes: []model.EpisodeState{
			{
				GUID:    "overcast-id-55",
				FeedURL: "https://rss.example.com/feed/", // Overcast has trailing slash
				Title:   "Trailing Slash Test",
				PubDate: pubDate,
			},
		},
	}
	index := buildOvercastIndex(lib)

	// Apple has no trailing slash.
	appleEp := model.EpisodeState{
		FeedURL: "https://rss.example.com/feed",
		Title:   "Trailing Slash Test",
		PubDate: pubDate,
	}
	if _, ok := findInOvercastIndex(index, appleEp, false); !ok {
		t.Error("trailing-slash mismatch prevented match — normalisation should handle this")
	}
}

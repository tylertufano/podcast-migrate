package model

import (
	"testing"
)

func TestIsSubscriberFeed(t *testing.T) {
	cases := []struct {
		title   string
		feedURL string
		want    bool
	}{
		// Title markers — subscriber/paid suffixes
		{"Fresh Air Plus", "https://feeds.npr.org/381444908/podcast.xml", true},
		{"Planet Money +", "https://feeds.npr.org/pm/podcast.xml", true},
		{"Planet Money+", "https://feeds.npm.org/pm/podcast.xml", true},
		{"The Daily - Subscriber Feed (🔓 for you@example.com)", "https://thedaily.supercast.com", true},
		{"Amicus - Member Feed", "https://feeds.megaphone.fm/amicus", true},
		{"Some Show - Private Feed", "https://rss.example.com/private", true},
		{"Some Show - Premium Feed", "https://rss.example.com/premium", true},
		{"Some Show (🔓)", "https://rss.example.com/feed", true},

		// Public titles — no markers
		{"Fresh Air", "https://feeds.npr.org/381444908/podcast.xml", false},
		{"The Daily", "https://feeds.simplecast.com/Sl5CSM3S", false},
		{"Surplus", "https://rss.example.com/surplus", false}, // "plus" embedded in word
		{"Plus", "https://rss.example.com/plus", false},       // lone "Plus" is not a suffix

		// URL scheme — Apple internal
		{"The Story of Classical", "internal://12345/feed", true},

		// Known subscriber platform domains
		{"Talking Feds", "https://talkingfeds.supercast.com/feed", true},
		{"Some Show", "https://feeds.memberful.com/show/feed", true},
		{"Some Show", "https://someshow.supporting.cast.st/feed", true},
		{"Some Show", "https://www.patreon.com/rss/someshow", true},

		// Subdomain of subscriber platform
		{"Some Show", "https://show.supercast.com/rss", true},

		// Regular CDN used for both public and subscriber — not caught here
		// (KVS URL-mismatch signal handles these in the Apple reader)
		{"The Daily", "https://feeds.simplecast.com/54nAGcIl", false},
		{"Amicus With Dahlia Lithwick", "https://feeds.megaphone.fm/slatesamicuswithdahlialithwick", false},

		// Empty/malformed URL — should not panic
		{"Some Show", "", false},
		{"Some Show", "not-a-url", false},
	}
	for _, tc := range cases {
		got := IsSubscriberFeed(tc.title, tc.feedURL)
		if got != tc.want {
			t.Errorf("IsSubscriberFeed(%q, %q) = %v, want %v", tc.title, tc.feedURL, got, tc.want)
		}
	}
}

func TestNormalizePlusTitle(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// NPR Plus " Plus" suffix
		{"Fresh Air Plus", "fresh air"},
		{"Planet Money Plus", "planet money"},
		{"Here & Now Plus", "here & now"},
		// "+" suffix (no space)
		{"Planet Money+", "planet money"},
		// " +" suffix (space before plus)
		{"Planet Money +", "planet money"},
		// Case-insensitive — input mixed case
		{"Fresh Air PLUS", "fresh air"},
		{"Fresh Air Plus", "fresh air"},
		// Already-public titles are unchanged (just lowercased)
		{"Fresh Air", "fresh air"},
		{"Planet Money", "planet money"},
		// Empty and whitespace
		{"", ""},
		{"  ", ""},
		// Whitespace around title
		{"  Fresh Air Plus  ", "fresh air"},
		// "Plus" alone is NOT stripped (no leading space, and it IS the whole title)
		{"Plus", "plus"},
		// Title that ends with "plus" as part of a word (no space before) — unchanged
		{"Surplus", "surplus"},
		// Multiple suffix occurrences — only the outermost is stripped
		{"Fresh Air Plus Plus", "fresh air plus"},
		// NYT subscriber feed — static suffix
		{"The Daily - Subscriber Feed (🔓)", "the daily"},
		{"The Daily - Subscriber Feed", "the daily"},
		// NYT subscriber feed — dynamic trailing content ("🔓 for <name/email>")
		{"The Daily - Subscriber Feed (🔓 for you@example.com)", "the daily"},
		{"The Daily - Subscriber Feed (🔓 for John Smith)", "the daily"},
		// Member / private / premium variants (static)
		{"Some Show - Member Feed (🔓)", "some show"},
		{"Some Show - Member Feed", "some show"},
		{"Some Show - Private Feed", "some show"},
		{"Some Show - Premium Feed", "some show"},
		// Member / private variants — dynamic trailing content
		{"Some Show - Member Feed (🔓 for subscriber@news.com)", "some show"},
		{"Some Show - Private Feed (access token here)", "some show"},
		// Standalone lock emoji
		{"Some Show (🔓)", "some show"},
		// Subscriber suffix + Plus suffix: index-based stripping hits
		// "- subscriber feed" first, so the whole decoration is removed.
		{"Show - Subscriber Feed Plus", "show"},
		// Public title unchanged
		{"The Daily", "the daily"},
	}
	for _, tc := range cases {
		got := NormalizePlusTitle(tc.input)
		if got != tc.want {
			t.Errorf("NormalizePlusTitle(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

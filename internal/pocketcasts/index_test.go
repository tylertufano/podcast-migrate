package pocketcasts

import (
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// TestFindInIndex_CrossPodcastFallback guards against the regression where an
// episode cross-posted by a podcast network (e.g. a Crooked Media episode
// published on both "Justice by Design" and "#SistersInLaw") would be missed
// because Apple attributed it to a different show than PC did.
//
// The source episode carries the Apple podcast's feed URL ("#SistersInLaw"),
// but the only indexed entry for it uses the PC podcast's feed URL
// ("Justice by Design").  The title+date fallback must find it.
func TestFindInIndex_CrossPodcastFallback(t *testing.T) {
	pubTime := time.Date(2025, 2, 21, 12, 0, 0, 0, time.UTC)

	// Index the episode under "Justice by Design" feed URL (as PC would).
	index := make(map[string]pcIndexEntry)
	pcEp := &APIEpisode{
		UUID:          "pc-ep-uuid-123",
		PodcastUUID:   "jbd-podcast-uuid",
		Title:         "Schools: Still Separate and Unequal",
		PublishedAt:   pubTime.Format(time.RFC3339),
		PlayingStatus: PlayingUnplayed,
	}
	addToIndex(index, pcEp, "https://feeds.simplecast.com/justice-by-design")

	// Source episode has the #SistersInLaw feed URL (Apple attribution).
	srcEp := model.EpisodeState{
		FeedURL:   "https://feeds.simplecast.com/sistersinlaw",
		Title:     "Schools: Still Separate and Unequal",
		PubDate:   pubTime,
		PlayState: model.PlayStatePlayed,
	}

	// Feed-URL-keyed lookups must fail (different feed).
	normSL := normalizeFeedURL(srcEp.FeedURL)
	if _, ok := index["feeddate:"+normSL+"|"+pubTime.UTC().Format(time.RFC3339)]; ok {
		t.Fatal("feeddate key should not exist for #SistersInLaw feed URL")
	}

	// Cross-podcast fallback must succeed.
	entry, ok := findInIndex(index, srcEp)
	if !ok {
		t.Fatal("findInIndex: cross-podcast title+date fallback failed — episode not found")
	}
	if entry.episodeUUID != "pc-ep-uuid-123" {
		t.Errorf("episodeUUID: got %q, want %q", entry.episodeUUID, "pc-ep-uuid-123")
	}
}

// TestFindInIndex_FeedURLKeyTakesPriority ensures that when an episode exists
// for both the correct feed URL and a different podcast on the same day, the
// feed-URL key is preferred over the cross-podcast fallback.
func TestFindInIndex_FeedURLKeyTakesPriority(t *testing.T) {
	pubTime := time.Date(2025, 2, 21, 12, 0, 0, 0, time.UTC)

	index := make(map[string]pcIndexEntry)

	// "Correct" entry: same feed URL, correct episode UUID.
	correctEp := &APIEpisode{
		UUID:        "correct-uuid",
		PodcastUUID: "pod-a",
		Title:       "Some Episode Title",
		PublishedAt: pubTime.Format(time.RFC3339),
	}
	addToIndex(index, correctEp, "https://feeds.example.com/podcast-a")

	// "Wrong" entry: different feed, same title+date (simulates another show).
	wrongEp := &APIEpisode{
		UUID:        "wrong-uuid",
		PodcastUUID: "pod-b",
		Title:       "Some Episode Title",
		PublishedAt: pubTime.Format(time.RFC3339),
	}
	addToIndex(index, wrongEp, "https://feeds.example.com/podcast-b")

	// Source episode has the correct feed URL — must get the correct entry.
	srcEp := model.EpisodeState{
		FeedURL:   "https://feeds.example.com/podcast-a",
		Title:     "Some Episode Title",
		PubDate:   pubTime,
		PlayState: model.PlayStatePlayed,
	}

	entry, ok := findInIndex(index, srcEp)
	if !ok {
		t.Fatal("findInIndex: expected a match")
	}
	if entry.episodeUUID != "correct-uuid" {
		t.Errorf("episodeUUID: got %q, want %q (feed-URL key should win over cross-podcast fallback)",
			entry.episodeUUID, "correct-uuid")
	}
}

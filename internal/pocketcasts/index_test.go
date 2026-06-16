package pocketcasts

import (
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// ── inferPlayState tests ──────────────────────────────────────────────────────

func TestInferPlayState_Played_ReturnsPlayed(t *testing.T) {
	state, pos := inferPlayState(PlayingPlayed, 3600, 3600)
	if state != model.PlayStatePlayed {
		t.Errorf("PlayingPlayed: got state %v, want PlayStatePlayed", state)
	}
	if pos != 0 {
		t.Errorf("PlayingPlayed: got pos %v, want 0", pos)
	}
}

func TestInferPlayState_InProgress_MidEpisode_ReturnsInProgress(t *testing.T) {
	state, pos := inferPlayState(PlayingInProgress, 1800, 3600) // 30m of 60m
	if state != model.PlayStateInProgress {
		t.Errorf("InProgress mid-episode: got state %v, want PlayStateInProgress", state)
	}
	if pos != 30*time.Minute {
		t.Errorf("InProgress mid-episode: got pos %v, want 30m", pos)
	}
}

func TestInferPlayState_InProgress_NearEnd_PromotedToPlayed(t *testing.T) {
	// 59m55s of 60m — within the 60s nearEndThreshold.
	state, pos := inferPlayState(PlayingInProgress, 3595, 3600)
	if state != model.PlayStatePlayed {
		t.Errorf("near-end InProgress: got state %v, want PlayStatePlayed", state)
	}
	if pos != 0 {
		t.Errorf("near-end InProgress: got pos %v, want 0 (played promotion clears position)", pos)
	}
}

func TestInferPlayState_InProgress_ExactlyAtEnd_PromotedToPlayed(t *testing.T) {
	state, _ := inferPlayState(PlayingInProgress, 3600, 3600)
	if state != model.PlayStatePlayed {
		t.Errorf("at-end InProgress: got state %v, want PlayStatePlayed", state)
	}
}

func TestInferPlayState_InProgress_JustOutsideThreshold_ReturnsInProgress(t *testing.T) {
	// nearEndThreshold=60: 3600-60=3540; played_up_to must be >= 3540.
	// 3539 is one second outside the threshold → must stay InProgress.
	state, pos := inferPlayState(PlayingInProgress, 3539, 3600)
	if state != model.PlayStateInProgress {
		t.Errorf("just-outside threshold: got state %v, want PlayStateInProgress", state)
	}
	if pos != time.Duration(3539)*time.Second {
		t.Errorf("just-outside threshold: got pos %v, want %v", pos, time.Duration(3539)*time.Second)
	}
}

func TestInferPlayState_InProgress_NoDuration_ReturnsInProgress(t *testing.T) {
	// Duration unknown (0): cannot apply near-end check; trust PlayingStatus.
	state, pos := inferPlayState(PlayingInProgress, 3595, 0)
	if state != model.PlayStateInProgress {
		t.Errorf("no duration: got state %v, want PlayStateInProgress", state)
	}
	if pos != time.Duration(3595)*time.Second {
		t.Errorf("no duration: got pos %v, want %v", pos, time.Duration(3595)*time.Second)
	}
}

func TestInferPlayState_Unplayed_ReturnsUnplayed(t *testing.T) {
	state, pos := inferPlayState(PlayingUnplayed, 0, 3600)
	if state != model.PlayStateUnplayed {
		t.Errorf("unplayed: got state %v, want PlayStateUnplayed", state)
	}
	if pos != 0 {
		t.Errorf("unplayed: got pos %v, want 0", pos)
	}
}

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
	addToIndex(index, pcEp, "https://feeds.simplecast.com/justice-by-design", "test")

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
	addToIndex(index, correctEp, "https://feeds.example.com/podcast-a", "test")

	// "Wrong" entry: different feed, same title+date (simulates another show).
	wrongEp := &APIEpisode{
		UUID:        "wrong-uuid",
		PodcastUUID: "pod-b",
		Title:       "Some Episode Title",
		PublishedAt: pubTime.Format(time.RFC3339),
	}
	addToIndex(index, wrongEp, "https://feeds.example.com/podcast-b", "test")

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

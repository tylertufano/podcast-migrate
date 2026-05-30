package overcast

// White-box tests for filterEpisodesByPodcast and buildFeedToTitle.

import (
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

func TestFilterEpisodesByPodcast(t *testing.T) {
	feed1 := "https://feeds.example.com/show-a"
	feed2 := "https://feeds.example.com/show-b"
	feed3 := "https://feeds.example.com/show-c"

	feedToTitle := map[string]string{
		feed1: "#sistersinlaw",
		feed2: "the daily",
		feed3: "hard fork",
	}

	eps := []model.EpisodeState{
		{FeedURL: feed1, Title: "Ep 1", PubDate: time.Now(), PlayState: model.PlayStatePlayed},
		{FeedURL: feed1, Title: "Ep 2", PubDate: time.Now(), PlayState: model.PlayStatePlayed},
		{FeedURL: feed2, Title: "Ep 3", PubDate: time.Now(), PlayState: model.PlayStatePlayed},
		{FeedURL: feed3, Title: "Ep 4", PubDate: time.Now(), PlayState: model.PlayStatePlayed},
	}

	t.Run("no filter returns all", func(t *testing.T) {
		got := filterEpisodesByPodcast(eps, feedToTitle, nil)
		if len(got) != 4 {
			t.Errorf("want 4 episodes, got %d", len(got))
		}
	})

	t.Run("exact title word match", func(t *testing.T) {
		got := filterEpisodesByPodcast(eps, feedToTitle, []string{"sistersinlaw"})
		if len(got) != 2 {
			t.Errorf("want 2 episodes (both from feed1), got %d", len(got))
		}
		for _, ep := range got {
			if ep.FeedURL != feed1 {
				t.Errorf("expected all episodes from feed1, got FeedURL=%q", ep.FeedURL)
			}
		}
	})

	t.Run("case-insensitive match", func(t *testing.T) {
		got := filterEpisodesByPodcast(eps, feedToTitle, []string{"DAILY"})
		if len(got) != 1 {
			t.Errorf("want 1 episode, got %d", len(got))
		}
		if len(got) > 0 && got[0].FeedURL != feed2 {
			t.Errorf("expected episode from feed2, got FeedURL=%q", got[0].FeedURL)
		}
	})

	t.Run("partial word match", func(t *testing.T) {
		// "fork" matches "hard fork"
		got := filterEpisodesByPodcast(eps, feedToTitle, []string{"fork"})
		if len(got) != 1 {
			t.Errorf("want 1 episode, got %d", len(got))
		}
	})

	t.Run("multiple patterns OR logic", func(t *testing.T) {
		// "daily" matches feed2, "fork" matches feed3 → 2 episodes total
		got := filterEpisodesByPodcast(eps, feedToTitle, []string{"daily", "fork"})
		if len(got) != 2 {
			t.Errorf("want 2 episodes, got %d", len(got))
		}
	})

	t.Run("no match returns empty", func(t *testing.T) {
		got := filterEpisodesByPodcast(eps, feedToTitle, []string{"zzznomatch"})
		if len(got) != 0 {
			t.Errorf("want 0 episodes, got %d", len(got))
		}
	})

	t.Run("empty pattern string skipped", func(t *testing.T) {
		// An empty pattern after trimming should not match everything.
		got := filterEpisodesByPodcast(eps, feedToTitle, []string{"   "})
		if len(got) != 0 {
			t.Errorf("blank pattern should match nothing, got %d episodes", len(got))
		}
	})
}

func TestBuildFeedToTitle(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/show-a", Title: "#SistersInLaw"},
			{FeedURL: "https://feeds.example.com/show-b", Title: "  The Daily  "},
			{FeedURL: "", Title: "No Feed"}, // should be skipped
		},
	}
	m := buildFeedToTitle(lib)
	if len(m) != 2 {
		t.Errorf("want 2 entries (empty feedURL skipped), got %d", len(m))
	}
	if m["https://feeds.example.com/show-a"] != "#sistersinlaw" {
		t.Errorf("title should be lowercased, got %q", m["https://feeds.example.com/show-a"])
	}
	if m["https://feeds.example.com/show-b"] != "the daily" {
		t.Errorf("title should be trimmed+lowercased, got %q", m["https://feeds.example.com/show-b"])
	}
}

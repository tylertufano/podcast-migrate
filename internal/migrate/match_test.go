package migrate_test

import (
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
)

// ── NormalizeFeedURL ──────────────────────────────────────────────────────────

func TestNormalizeFeedURL_HTTPtoHTTPS(t *testing.T) {
	got := migrate.NormalizeFeedURL("http://feeds.example.com/show")
	want := "https://feeds.example.com/show"
	if got != want {
		t.Errorf("http→https: got %q, want %q", got, want)
	}
}

func TestNormalizeFeedURL_TrailingSlashStripped(t *testing.T) {
	got := migrate.NormalizeFeedURL("https://feeds.example.com/show/")
	want := "https://feeds.example.com/show"
	if got != want {
		t.Errorf("trailing slash: got %q, want %q", got, want)
	}
}

func TestNormalizeFeedURL_RootSlashPreserved(t *testing.T) {
	got := migrate.NormalizeFeedURL("https://feeds.example.com/")
	want := "https://feeds.example.com/"
	if got != want {
		t.Errorf("root slash: got %q, want %q", got, want)
	}
}

func TestNormalizeFeedURL_HostLowercased(t *testing.T) {
	got := migrate.NormalizeFeedURL("https://FEEDS.Example.COM/show")
	want := "https://feeds.example.com/show"
	if got != want {
		t.Errorf("host case: got %q, want %q", got, want)
	}
}

func TestNormalizeFeedURL_FragmentDropped(t *testing.T) {
	got := migrate.NormalizeFeedURL("https://feeds.example.com/show#section")
	want := "https://feeds.example.com/show"
	if got != want {
		t.Errorf("fragment: got %q, want %q", got, want)
	}
}

func TestNormalizeFeedURL_QueryPreserved(t *testing.T) {
	got := migrate.NormalizeFeedURL("https://feeds.example.com/show?feed=rss2")
	want := "https://feeds.example.com/show?feed=rss2"
	if got != want {
		t.Errorf("query: got %q, want %q", got, want)
	}
}

func TestNormalizeFeedURL_HTTPSUnchanged(t *testing.T) {
	in := "https://feeds.example.com/show"
	if got := migrate.NormalizeFeedURL(in); got != in {
		t.Errorf("already-canonical: got %q, want %q", got, in)
	}
}

// ── BuildFeedToTitle ─────────────────────────────────────────────────────────

func TestBuildFeedToTitle_NilLib(t *testing.T) {
	if m := migrate.BuildFeedToTitle(nil); m != nil {
		t.Errorf("nil lib: got %v, want nil", m)
	}
}

func TestBuildFeedToTitle_LowercasesTitles(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/a", Title: "Fresh Air"},
			{FeedURL: "https://feeds.example.com/b", Title: "PLANET MONEY"},
		},
	}
	m := migrate.BuildFeedToTitle(lib)
	if got := m["https://feeds.example.com/a"]; got != "fresh air" {
		t.Errorf("title a: got %q, want %q", got, "fresh air")
	}
	if got := m["https://feeds.example.com/b"]; got != "planet money" {
		t.Errorf("title b: got %q, want %q", got, "planet money")
	}
}

func TestBuildFeedToTitle_EmptyFeedURLSkipped(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "", Title: "No URL"},
		},
	}
	m := migrate.BuildFeedToTitle(lib)
	if len(m) != 0 {
		t.Errorf("empty feed URL: expected empty map, got %v", m)
	}
}

// ── FilterEpisodesByPodcast ───────────────────────────────────────────────────

func TestFilterEpisodesByPodcast_EmptyFilters_ReturnsAll(t *testing.T) {
	eps := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/a", Title: "Ep 1"},
		{FeedURL: "https://feeds.example.com/b", Title: "Ep 2"},
	}
	feedToTitle := map[string]string{
		"https://feeds.example.com/a": "fresh air",
		"https://feeds.example.com/b": "planet money",
	}
	got := migrate.FilterEpisodesByPodcast(eps, feedToTitle, nil)
	if len(got) != 2 {
		t.Errorf("no filter: got %d episodes, want 2", len(got))
	}
}

func TestFilterEpisodesByPodcast_FilterByTitle(t *testing.T) {
	eps := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/a", Title: "Ep 1"},
		{FeedURL: "https://feeds.example.com/b", Title: "Ep 2"},
	}
	feedToTitle := map[string]string{
		"https://feeds.example.com/a": "fresh air",
		"https://feeds.example.com/b": "planet money",
	}
	got := migrate.FilterEpisodesByPodcast(eps, feedToTitle, []string{"fresh air"})
	if len(got) != 1 || got[0].FeedURL != "https://feeds.example.com/a" {
		t.Errorf("filter 'fresh air': got %v, want only ep a", got)
	}
}

func TestFilterEpisodesByPodcast_CaseInsensitiveFilter(t *testing.T) {
	eps := []model.EpisodeState{
		{FeedURL: "https://feeds.example.com/a", Title: "Ep"},
	}
	feedToTitle := map[string]string{"https://feeds.example.com/a": "fresh air"}
	got := migrate.FilterEpisodesByPodcast(eps, feedToTitle, []string{"FRESH AIR"})
	if len(got) != 1 {
		t.Errorf("case-insensitive filter: got %d, want 1", len(got))
	}
}

// ── FuzzyNormalizeTitle ───────────────────────────────────────────────────────

func TestFuzzyNormalizeTitle_SeasonMarkerS01Stripped(t *testing.T) {
	got := migrate.FuzzyNormalizeTitle("The Retrievals S01 - Ep. 4")
	want := "the retrievals ep 4"
	if got != want {
		t.Errorf("S01 stripped: got %q, want %q", got, want)
	}
}

func TestFuzzyNormalizeTitle_SeasonMarkerS1Stripped(t *testing.T) {
	got := migrate.FuzzyNormalizeTitle("Some Show S1 - Episode 10")
	want := "some show episode 10"
	if got != want {
		t.Errorf("S1 stripped: got %q, want %q", got, want)
	}
}

func TestFuzzyNormalizeTitle_SeasonWordStripped(t *testing.T) {
	got := migrate.FuzzyNormalizeTitle("The Crown: Season 4 - Episode 1")
	want := "the crown episode 1"
	if got != want {
		t.Errorf("Season N stripped: got %q, want %q", got, want)
	}
}

func TestFuzzyNormalizeTitle_NoSeasonMarker_Unchanged(t *testing.T) {
	got := migrate.FuzzyNormalizeTitle("The Retrievals - Ep. 4")
	want := "the retrievals ep 4"
	if got != want {
		t.Errorf("no season: got %q, want %q", got, want)
	}
}

func TestFuzzyNormalizeTitle_MatchesAcrossFeedVariants(t *testing.T) {
	// The core use case: same episode title, one feed has "S01" and the other doesn't.
	a := migrate.FuzzyNormalizeTitle("The Retrievals - Ep. 4")
	b := migrate.FuzzyNormalizeTitle("The Retrievals S01 - Ep. 4")
	if a != b {
		t.Errorf("feed variant mismatch: %q vs %q", a, b)
	}
}

func TestFuzzyNormalizeTitle_DifferentTitlesDontMatch(t *testing.T) {
	// Unrelated episode titles must not be considered equal after normalisation.
	a := migrate.FuzzyNormalizeTitle("Pollercoaster: What the Primaries Tell Us About the Midterms, So Far")
	b := migrate.FuzzyNormalizeTitle("Bondi Gets the Boot")
	if a == b {
		t.Errorf("different titles should not normalise to the same string; both gave %q", a)
	}
}

func TestFuzzyNormalizeTitle_ApostropheRemoved(t *testing.T) {
	// Apostrophes are removed rather than replaced with a space so that
	// "O'Brien" and "OBrien" normalise to the same string.
	a := migrate.FuzzyNormalizeTitle("Conan O'Brien Needs a Friend")
	b := migrate.FuzzyNormalizeTitle("Conan OBrien Needs a Friend")
	if a != b {
		t.Errorf("apostrophe variant: %q vs %q — should be equal after normalisation", a, b)
	}
}

func TestFuzzyNormalizeTitle_HyphenBecomesSpace(t *testing.T) {
	// Hyphens are word separators, not contractions — they should become a space.
	got := migrate.FuzzyNormalizeTitle("Self-Help Podcast")
	want := "self help podcast"
	if got != want {
		t.Errorf("hyphen: got %q, want %q", got, want)
	}
}

func TestFuzzyNormalizeTitle_SerialNotStripped(t *testing.T) {
	// "serial" contains 's' but is not a season marker.
	got := migrate.FuzzyNormalizeTitle("Serial: Season 1")
	// "serial" stays; "season 1" is stripped.
	want := "serial"
	if got != want {
		t.Errorf("serial: got %q, want %q", got, want)
	}
}

// ── SkipReason ────────────────────────────────────────────────────────────────

func TestSkipReason_PlayedDesired_AlreadyPlayed(t *testing.T) {
	got := migrate.SkipReason(model.PlayStatePlayed, 0, model.PlayStatePlayed, 0)
	if got != "already_played" {
		t.Errorf("got %q, want already_played", got)
	}
}

func TestSkipReason_PlayedDesired_CurrentUnplayed_Empty(t *testing.T) {
	got := migrate.SkipReason(model.PlayStatePlayed, 0, model.PlayStateUnplayed, 0)
	if got != "" {
		t.Errorf("got %q, want empty (should write)", got)
	}
}

func TestSkipReason_PlayedDesired_CurrentInProgress_Empty(t *testing.T) {
	got := migrate.SkipReason(model.PlayStatePlayed, 0, model.PlayStateInProgress, 300*time.Second)
	if got != "" {
		t.Errorf("got %q, want empty (should write)", got)
	}
}

func TestSkipReason_InProgressDesired_CurrentAhead_AlreadyAhead(t *testing.T) {
	// Desired: 200s in, Overcast: 300s in — Overcast is ahead.
	got := migrate.SkipReason(model.PlayStateInProgress, 200*time.Second, model.PlayStateInProgress, 300*time.Second)
	if got != "already_ahead" {
		t.Errorf("got %q, want already_ahead", got)
	}
}

func TestSkipReason_InProgressDesired_EqualPosition_AlreadyAhead(t *testing.T) {
	got := migrate.SkipReason(model.PlayStateInProgress, 300*time.Second, model.PlayStateInProgress, 300*time.Second)
	if got != "already_ahead" {
		t.Errorf("equal positions: got %q, want already_ahead", got)
	}
}

func TestSkipReason_InProgressDesired_SourceAhead_Empty(t *testing.T) {
	got := migrate.SkipReason(model.PlayStateInProgress, 400*time.Second, model.PlayStateInProgress, 300*time.Second)
	if got != "" {
		t.Errorf("source ahead: got %q, want empty (should write)", got)
	}
}

func TestSkipReason_InProgressDesired_CurrentPlayed_AlreadyPlayed(t *testing.T) {
	// Overcast already played — skip updating to in-progress.
	got := migrate.SkipReason(model.PlayStateInProgress, 400*time.Second, model.PlayStatePlayed, 0)
	if got != "already_played" {
		t.Errorf("in-progress desired / current played: got %q, want already_played", got)
	}
}

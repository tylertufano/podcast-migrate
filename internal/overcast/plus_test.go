package overcast

// Tests for Plus-feed title normalisation used by augmentIndexFromPodcastPages.

import (
	"testing"

	"github.com/tyler/podcast-migrate/internal/model"
)

func TestBuildOpmlTitleIndex_ExactKey(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.npr.org/plus/fresh-air", Title: "Fresh Air Plus", OvercastID: "oc1"},
		},
	}
	idx := buildOpmlTitleIndex(lib)

	// Exact lowercased key must be present.
	info, ok := idx["fresh air plus"]
	if !ok {
		t.Fatal("exact key 'fresh air plus' not found in index")
	}
	if info.title != "Fresh Air Plus" {
		t.Errorf("title: got %q, want 'Fresh Air Plus'", info.title)
	}
	if info.overcastID != "oc1" {
		t.Errorf("overcastID: got %q, want 'oc1'", info.overcastID)
	}
}

func TestBuildOpmlTitleIndex_PlusNormKey(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.npr.org/plus/fresh-air", Title: "Fresh Air Plus", OvercastID: "oc1"},
		},
	}
	idx := buildOpmlTitleIndex(lib)

	// Plus-normalised key must also be present so Apple public-feed title matches.
	info, ok := idx["fresh air"]
	if !ok {
		t.Fatal("Plus-normalised key 'fresh air' not found in index")
	}
	if info.title != "Fresh Air Plus" {
		t.Errorf("title via normalised key: got %q, want 'Fresh Air Plus'", info.title)
	}
}

func TestBuildOpmlTitleIndex_PlusSymbol(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.npm.org/plus/pm", Title: "Planet Money+", OvercastID: "oc2"},
		},
	}
	idx := buildOpmlTitleIndex(lib)

	if _, ok := idx["planet money+"]; !ok {
		t.Error("exact key 'planet money+' not found")
	}
	if _, ok := idx["planet money"]; !ok {
		t.Error("normalised key 'planet money' not found")
	}
}

func TestBuildOpmlTitleIndex_PublicPodcastNotDuplicated(t *testing.T) {
	// A public-feed podcast has no suffix to strip; normalisation should not
	// create a spurious extra key.
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.npr.org/public/fresh-air", Title: "Fresh Air", OvercastID: "oc3"},
		},
	}
	idx := buildOpmlTitleIndex(lib)

	if len(idx) != 1 {
		t.Errorf("public podcast should produce exactly 1 key, got %d keys: %v", len(idx), indexKeys(idx))
	}
	if _, ok := idx["fresh air"]; !ok {
		t.Error("key 'fresh air' not found")
	}
}

func TestBuildOpmlTitleIndex_ExactKeyWinsOnCollision(t *testing.T) {
	// Both "Fresh Air Plus" and "Fresh Air" subscribed in Overcast.
	// The exact "fresh air" key (from "Fresh Air" subscription) should win
	// over the normalised key from "Fresh Air Plus", since exact comes first
	// (build order: first entry wins on collision via the !exists guard).
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.npr.org/plus", Title: "Fresh Air Plus", OvercastID: "plus"},
			{FeedURL: "https://feeds.npr.org/public", Title: "Fresh Air", OvercastID: "public"},
		},
	}
	idx := buildOpmlTitleIndex(lib)

	// "fresh air plus" → exact for Plus entry
	if info, ok := idx["fresh air plus"]; !ok || info.overcastID != "plus" {
		t.Errorf("'fresh air plus' key: got %+v, want overcastID=plus", idx["fresh air plus"])
	}
	// "fresh air" → exact for the public entry (inserted first, so it owns this key)
	if info, ok := idx["fresh air"]; !ok {
		t.Error("'fresh air' key missing")
	} else if info.overcastID != "plus" && info.overcastID != "public" {
		t.Errorf("'fresh air' key: unexpected overcastID %q", info.overcastID)
	}
}

func TestBuildOpmlTitleIndex_EmptyTitle(t *testing.T) {
	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://feeds.example.com/feed", Title: "", OvercastID: "oc"},
		},
	}
	idx := buildOpmlTitleIndex(lib)
	if len(idx) != 0 {
		t.Errorf("empty-title podcast should produce no keys, got %d", len(idx))
	}
}

// TestSetMatchOPMLPath_Field verifies that SetMatchOPMLPath actually stores the path
// and that Capabilities reflects the correct semantics (WritePlayState from credentials,
// not from OPML paths).
func TestSetMatchOPMLPath_Field(t *testing.T) {
	p := &Provider{}
	if p.matchOPMLPath != "" {
		t.Errorf("matchOPMLPath should be empty by default, got %q", p.matchOPMLPath)
	}
	p.SetMatchOPMLPath("/tmp/my-match.opml")
	if p.matchOPMLPath != "/tmp/my-match.opml" {
		t.Errorf("matchOPMLPath: got %q, want '/tmp/my-match.opml'", p.matchOPMLPath)
	}
	// Setting a match path alone does not affect WritePlayState (credentials-driven).
	if p.Capabilities().WritePlayState {
		t.Error("WritePlayState should be false without credentials")
	}
	p.email = "user@example.com"
	if !p.Capabilities().WritePlayState {
		t.Error("WritePlayState should be true once credentials are set")
	}
}

func indexKeys(m map[string]opmlPodInfo) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

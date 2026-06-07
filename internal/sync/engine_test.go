package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

// ---- episodeKey ----

func TestEpisodeKey_GUIDTakesPriority(t *testing.T) {
	ep := model.EpisodeState{
		GUID:    "rss-guid-abc",
		FeedURL: "https://feeds.example.com/show",
		PubDate: time.Now(),
		Title:   "Some Title",
	}
	got := episodeKey(ep)
	if got != "guid:rss-guid-abc" {
		t.Errorf("got %q, want guid:rss-guid-abc", got)
	}
}

func TestEpisodeKey_FeedURLPubDateWhenNoGUID(t *testing.T) {
	ep := model.EpisodeState{
		FeedURL: "https://feeds.example.com/show",
		PubDate: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
		Title:   "Some Title",
	}
	want := "feeddate:https://feeds.example.com/show|2024-01-15T12:00:00"
	got := episodeKey(ep)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEpisodeKey_PubDateFormattedInUTC(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	ep := model.EpisodeState{
		FeedURL: "https://feeds.example.com/show",
		PubDate: time.Date(2024, 1, 15, 7, 0, 0, 0, loc), // 07:00 EST = 12:00 UTC
	}
	want := "feeddate:https://feeds.example.com/show|2024-01-15T12:00:00"
	got := episodeKey(ep)
	if got != want {
		t.Errorf("got %q, want %q — PubDate should be formatted in UTC", got, want)
	}
}

func TestEpisodeKey_FeedURLTitleWhenNoGUIDOrPubDate(t *testing.T) {
	ep := model.EpisodeState{
		FeedURL: "https://feeds.example.com/show",
		Title:   "  My EPISODE  ", // whitespace and mixed case normalised
	}
	want := "feedtitle:https://feeds.example.com/show|my episode"
	got := episodeKey(ep)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEpisodeKey_FeedURLPubDateRequiresBothFeedAndDate(t *testing.T) {
	// No FeedURL → falls through to feedtitle even if PubDate is set.
	ep := model.EpisodeState{
		PubDate: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
		Title:   "My Episode",
	}
	got := episodeKey(ep)
	if got[:9] == "feeddate:" {
		t.Errorf("feeddate key should require non-empty FeedURL, got %q", got)
	}
}

// ---- furthestWins ----

func TestFurthestWins_PlayedBeatsInProgress(t *testing.T) {
	played := model.EpisodeState{GUID: "played", PlayState: model.PlayStatePlayed, PlayPosition: 100 * time.Second}
	inProg := model.EpisodeState{GUID: "inprog", PlayState: model.PlayStateInProgress, PlayPosition: 9000 * time.Second}

	if got := furthestWins(played, inProg); got.GUID != "played" {
		t.Error("played should beat in-progress regardless of position (a=played, b=in-progress)")
	}
	if got := furthestWins(inProg, played); got.GUID != "played" {
		t.Error("played should beat in-progress regardless of position (a=in-progress, b=played)")
	}
}

func TestFurthestWins_PlayedBeatsUnplayed(t *testing.T) {
	played := model.EpisodeState{GUID: "played", PlayState: model.PlayStatePlayed}
	unplayed := model.EpisodeState{GUID: "unplayed", PlayState: model.PlayStateUnplayed}

	if got := furthestWins(played, unplayed); got.GUID != "played" {
		t.Error("played should beat unplayed")
	}
	if got := furthestWins(unplayed, played); got.GUID != "played" {
		t.Error("played should beat unplayed (reversed)")
	}
}

func TestFurthestWins_BothPlayed_ReturnsA(t *testing.T) {
	a := model.EpisodeState{GUID: "a", PlayState: model.PlayStatePlayed}
	b := model.EpisodeState{GUID: "b", PlayState: model.PlayStatePlayed}
	if got := furthestWins(a, b); got.GUID != "a" {
		t.Errorf("when both played, should return first argument; got GUID=%q", got.GUID)
	}
}

func TestFurthestWins_FurtherPositionWins(t *testing.T) {
	short := model.EpisodeState{GUID: "short", PlayState: model.PlayStateInProgress, PlayPosition: 300 * time.Second}
	long := model.EpisodeState{GUID: "long", PlayState: model.PlayStateInProgress, PlayPosition: 900 * time.Second}

	if got := furthestWins(short, long); got.GUID != "long" {
		t.Errorf("further position should win (a=short, b=long); got %q", got.GUID)
	}
	if got := furthestWins(long, short); got.GUID != "long" {
		t.Errorf("further position should win (a=long, b=short); got %q", got.GUID)
	}
}

func TestFurthestWins_EqualPosition_ReturnsA(t *testing.T) {
	a := model.EpisodeState{GUID: "a", PlayState: model.PlayStateInProgress, PlayPosition: 500 * time.Second}
	b := model.EpisodeState{GUID: "b", PlayState: model.PlayStateInProgress, PlayPosition: 500 * time.Second}
	if got := furthestWins(a, b); got.GUID != "a" {
		t.Errorf("equal positions: should return first argument; got %q", got.GUID)
	}
}

func TestFurthestWins_InProgressBeatsUnplayed(t *testing.T) {
	inProg := model.EpisodeState{GUID: "inprog", PlayState: model.PlayStateInProgress, PlayPosition: 60 * time.Second}
	unplayed := model.EpisodeState{GUID: "unplayed", PlayState: model.PlayStateUnplayed, PlayPosition: 0}

	if got := furthestWins(inProg, unplayed); got.GUID != "inprog" {
		t.Error("in-progress should beat unplayed")
	}
}

// ---- resolveConflict ----

func TestResolveConflict_SourceWins(t *testing.T) {
	src := model.EpisodeState{GUID: "src", PlayState: model.PlayStateUnplayed}
	dst := model.EpisodeState{GUID: "dst", PlayState: model.PlayStatePlayed}
	got := resolveConflict(src, dst, provider.SourceWins)
	if got.GUID != "src" {
		t.Errorf("SourceWins: got GUID %q, want src", got.GUID)
	}
}

func TestResolveConflict_TargetWins(t *testing.T) {
	src := model.EpisodeState{GUID: "src", PlayState: model.PlayStatePlayed}
	dst := model.EpisodeState{GUID: "dst", PlayState: model.PlayStateUnplayed}
	got := resolveConflict(src, dst, provider.TargetWins)
	if got.GUID != "dst" {
		t.Errorf("TargetWins: got GUID %q, want dst", got.GUID)
	}
}

func TestResolveConflict_FurthestWins_Default(t *testing.T) {
	src := model.EpisodeState{GUID: "src", PlayState: model.PlayStateInProgress, PlayPosition: 100 * time.Second}
	dst := model.EpisodeState{GUID: "dst", PlayState: model.PlayStateInProgress, PlayPosition: 500 * time.Second}
	got := resolveConflict(src, dst, provider.FurthestWins)
	if got.GUID != "dst" {
		t.Errorf("FurthestWins: got GUID %q, want dst (further position)", got.GUID)
	}
}

// ---- merge ----

func TestMerge_SubscriptionUnion(t *testing.T) {
	src := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://a.example.com/feed", Title: "Show A"},
			{FeedURL: "https://shared.example.com/feed", Title: "Shared"},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://b.example.com/feed", Title: "Show B"},
			{FeedURL: "https://shared.example.com/feed", Title: "Shared"},
		},
	}
	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	if len(result.Podcasts) != 3 {
		t.Errorf("union should deduplicate: got %d podcasts, want 3", len(result.Podcasts))
	}
	seen := make(map[string]bool)
	for _, p := range result.Podcasts {
		seen[p.FeedURL] = true
	}
	for _, url := range []string{
		"https://a.example.com/feed",
		"https://b.example.com/feed",
		"https://shared.example.com/feed",
	} {
		if !seen[url] {
			t.Errorf("missing feed URL %s in merged result", url)
		}
	}
}

func TestMerge_SubscriptionNoDuplicates(t *testing.T) {
	// src has the same feed twice; should be deduplicated.
	src := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: "https://a.example.com/feed"},
			{FeedURL: "https://a.example.com/feed"},
		},
	}
	result := merge(src, nil, provider.WriteOptions{})
	if len(result.Podcasts) != 1 {
		t.Errorf("duplicate src feeds should be deduplicated: got %d, want 1", len(result.Podcasts))
	}
}

func TestMerge_EpisodeConflict_FurthestWins(t *testing.T) {
	guid := "shared-guid"
	src := &model.Library{
		Episodes: []model.EpisodeState{
			{GUID: guid, PlayState: model.PlayStateInProgress, PlayPosition: 300 * time.Second},
		},
	}
	dst := &model.Library{
		Episodes: []model.EpisodeState{
			{GUID: guid, PlayState: model.PlayStateInProgress, PlayPosition: 700 * time.Second},
		},
	}
	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if len(result.Episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(result.Episodes))
	}
	if result.Episodes[0].PlayPosition != 700*time.Second {
		t.Errorf("FurthestWins: got %v, want 700s", result.Episodes[0].PlayPosition)
	}
}

func TestMerge_SrcOnlyEpisodeIncluded(t *testing.T) {
	src := &model.Library{
		Episodes: []model.EpisodeState{{GUID: "src-only", PlayState: model.PlayStatePlayed}},
	}
	dst := &model.Library{}
	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if len(result.Episodes) != 1 || result.Episodes[0].GUID != "src-only" {
		t.Error("episode only in src should appear in result")
	}
}

func TestMerge_DstOnlyEpisodeIncluded(t *testing.T) {
	src := &model.Library{}
	dst := &model.Library{
		Episodes: []model.EpisodeState{{GUID: "dst-only", PlayState: model.PlayStatePlayed}},
	}
	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if len(result.Episodes) != 1 || result.Episodes[0].GUID != "dst-only" {
		t.Error("episode only in dst should appear in result")
	}
}

func TestMerge_OnlySubscriptions_SkipsEpisodes(t *testing.T) {
	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://a.example.com/feed"}},
		Episodes: []model.EpisodeState{{GUID: "ep1", PlayState: model.PlayStatePlayed}},
	}
	opts := provider.WriteOptions{OnlySubscriptions: true}
	result := merge(src, nil, opts)

	if len(result.Podcasts) != 1 {
		t.Errorf("OnlySubscriptions: got %d podcasts, want 1", len(result.Podcasts))
	}
	if len(result.Episodes) != 0 {
		t.Errorf("OnlySubscriptions: got %d episodes, want 0", len(result.Episodes))
	}
}

func TestMerge_OnlyPlayState_SkipsSubscriptions(t *testing.T) {
	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://a.example.com/feed"}},
		Episodes: []model.EpisodeState{{GUID: "ep1", PlayState: model.PlayStatePlayed}},
	}
	opts := provider.WriteOptions{OnlyPlayState: true}
	result := merge(src, nil, opts)

	if len(result.Podcasts) != 0 {
		t.Errorf("OnlyPlayState: got %d podcasts, want 0", len(result.Podcasts))
	}
	if len(result.Episodes) != 1 {
		t.Errorf("OnlyPlayState: got %d episodes, want 1", len(result.Episodes))
	}
}

func TestMerge_NilDst_UsesSrcOnly(t *testing.T) {
	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: "https://a.example.com/feed"}},
		Episodes: []model.EpisodeState{{GUID: "ep1", PlayState: model.PlayStatePlayed}},
	}
	result := merge(src, nil, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	if len(result.Podcasts) != 1 {
		t.Errorf("nil dst: got %d podcasts, want 1", len(result.Podcasts))
	}
	if len(result.Episodes) != 1 {
		t.Errorf("nil dst: got %d episodes, want 1", len(result.Episodes))
	}
}

func TestMerge_PreservesSourceProviderAndExportedAt(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	src := &model.Library{SourceProvider: "Apple Podcasts (SQLite)", ExportedAt: ts}
	result := merge(src, nil, provider.WriteOptions{})
	if result.SourceProvider != "Apple Podcasts (SQLite)" {
		t.Errorf("SourceProvider not preserved: %q", result.SourceProvider)
	}
	if !result.ExportedAt.Equal(ts) {
		t.Errorf("ExportedAt not preserved: %v", result.ExportedAt)
	}
}

// ---- Engine.Run ----

type mockProvider struct {
	name        string
	caps        provider.Capabilities
	lib         *model.Library
	written     *model.Library
	writtenOpts provider.WriteOptions
	getErr      error
	setErr      error
}

func (m *mockProvider) Name() string                     { return m.name }
func (m *mockProvider) Capabilities() provider.Capabilities { return m.caps }
func (m *mockProvider) GetLibrary(_ context.Context) (*model.Library, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.lib, nil
}
func (m *mockProvider) SetLibrary(_ context.Context, lib *model.Library, opts provider.WriteOptions) error {
	m.written = lib
	m.writtenOpts = opts
	return m.setErr
}

var fullCaps = provider.Capabilities{
	ReadSubscriptions:  true,
	WriteSubscriptions: true,
	ReadPlayState:      true,
	WritePlayState:     true,
}

func TestEngine_Run_WritesToDst(t *testing.T) {
	src := &mockProvider{
		name: "src", caps: fullCaps,
		lib: &model.Library{
			Podcasts: []model.Podcast{{FeedURL: "https://a.example.com/feed", Title: "Show A"}},
			Episodes: []model.EpisodeState{{GUID: "ep1", PlayState: model.PlayStatePlayed}},
		},
	}
	dst := &mockProvider{name: "dst", caps: fullCaps, lib: &model.Library{}}

	_, err := New(src, dst).Run(context.Background(), provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dst.written == nil {
		t.Fatal("SetLibrary was not called on dst")
	}
	if len(dst.written.Podcasts) != 1 {
		t.Errorf("got %d podcasts written, want 1", len(dst.written.Podcasts))
	}
}

func TestEngine_Run_PassesOptsToDst(t *testing.T) {
	src := &mockProvider{name: "src", caps: fullCaps, lib: &model.Library{}}
	dst := &mockProvider{name: "dst", caps: fullCaps, lib: &model.Library{}}

	opts := provider.WriteOptions{DryRun: true, OnlySubscriptions: true, ConflictStrategy: provider.SourceWins}
	if _, err := New(src, dst).Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !dst.writtenOpts.DryRun {
		t.Error("DryRun flag not passed through to SetLibrary")
	}
	if !dst.writtenOpts.OnlySubscriptions {
		t.Error("OnlySubscriptions flag not passed through to SetLibrary")
	}
	if dst.writtenOpts.ConflictStrategy != provider.SourceWins {
		t.Error("ConflictStrategy not passed through to SetLibrary")
	}
}

func TestEngine_Run_SrcReadError(t *testing.T) {
	src := &mockProvider{name: "src", caps: fullCaps, getErr: errors.New("disk error")}
	dst := &mockProvider{name: "dst", caps: fullCaps, lib: &model.Library{}}

	_, err := New(src, dst).Run(context.Background(), provider.WriteOptions{})
	if err == nil {
		t.Error("expected error when src read fails")
	}
}

func TestEngine_Run_DstReadError_ContinuesWithNilDst(t *testing.T) {
	// When the dst cannot be read, the engine should still proceed and write.
	src := &mockProvider{
		name: "src", caps: fullCaps,
		lib: &model.Library{
			Podcasts: []model.Podcast{{FeedURL: "https://a.example.com/feed"}},
		},
	}
	dst := &mockProvider{
		name:   "dst",
		caps:   fullCaps,
		getErr: errors.New("permission denied"),
	}

	_, err := New(src, dst).Run(context.Background(), provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if err != nil {
		t.Fatalf("Run should not fail when dst read fails: %v", err)
	}
	if dst.written == nil {
		t.Fatal("SetLibrary should still be called even when GetLibrary failed")
	}
}

func TestEngine_Run_DstWriteError(t *testing.T) {
	src := &mockProvider{name: "src", caps: fullCaps, lib: &model.Library{}}
	dst := &mockProvider{name: "dst", caps: fullCaps, lib: &model.Library{}, setErr: errors.New("write failed")}

	_, err := New(src, dst).Run(context.Background(), provider.WriteOptions{})
	if err == nil {
		t.Error("expected error when dst write fails")
	}
}

func TestEngine_Run_SubscriptionsAdded_WithDst(t *testing.T) {
	src := &mockProvider{
		name: "src", caps: fullCaps,
		lib: &model.Library{
			Podcasts: []model.Podcast{
				{FeedURL: "https://a.example.com/feed"},
				{FeedURL: "https://b.example.com/feed"},
			},
		},
	}
	dst := &mockProvider{
		name: "dst", caps: fullCaps,
		lib: &model.Library{
			Podcasts: []model.Podcast{
				{FeedURL: "https://a.example.com/feed"}, // already subscribed
			},
		},
	}

	result, err := New(src, dst).Run(context.Background(), provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Merged has 2; dst had 1 → 1 added.
	if result.SubscriptionsAdded != 1 {
		t.Errorf("SubscriptionsAdded: got %d, want 1", result.SubscriptionsAdded)
	}
}

func TestEngine_Run_SubscriptionsAdded_WithNilDst(t *testing.T) {
	// When dst cannot be read, all src subscriptions count as added.
	src := &mockProvider{
		name: "src", caps: fullCaps,
		lib: &model.Library{
			Podcasts: []model.Podcast{
				{FeedURL: "https://a.example.com/feed"},
				{FeedURL: "https://b.example.com/feed"},
			},
		},
	}
	// dst read will fail, so dstLib stays nil
	dst := &mockProvider{name: "dst", caps: fullCaps, getErr: errors.New("unreadable")}

	result, err := New(src, dst).Run(context.Background(), provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SubscriptionsAdded != 2 {
		t.Errorf("SubscriptionsAdded: got %d, want 2 (all src subs when dst unreadable)", result.SubscriptionsAdded)
	}
}

func TestEngine_Run_SkippedCountsPropagated(t *testing.T) {
	src := &mockProvider{
		name: "src", caps: fullCaps,
		lib: &model.Library{
			PaywalledEpisodesIncluded: 42,
			SkippedInternalPodcasts:   3,
		},
	}
	dst := &mockProvider{name: "dst", caps: fullCaps, lib: &model.Library{}}

	result, err := New(src, dst).Run(context.Background(), provider.WriteOptions{ConflictStrategy: provider.FurthestWins})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.PaywalledEpisodesIncluded != 42 {
		t.Errorf("PaywalledEpisodesIncluded: got %d, want 42", result.PaywalledEpisodesIncluded)
	}
	if result.SkippedInternalPodcasts != 3 {
		t.Errorf("SkippedInternalPodcasts: got %d, want 3", result.SkippedInternalPodcasts)
	}
}

func TestEngine_Run_WriteOnlyDst_SkipsRead(t *testing.T) {
	// Dst with no read capabilities should not have GetLibrary called.
	writeOnlyCaps := provider.Capabilities{WriteSubscriptions: true}
	src := &mockProvider{name: "src", caps: fullCaps, lib: &model.Library{}}
	dst := &mockProvider{
		name: "dst",
		caps: writeOnlyCaps,
		// getErr set: if GetLibrary is called it would fail, proving it was called
		getErr: errors.New("should not be called"),
	}

	_, err := New(src, dst).Run(context.Background(), provider.WriteOptions{})
	if err != nil {
		t.Fatalf("Run against write-only dst should not fail: %v", err)
	}
}

// ---- Result.String ----

func TestResult_String_DryRunPrefix(t *testing.T) {
	r := Result{DryRun: true}
	s := r.String()
	if len(s) < 9 || s[:9] != "[dry-run]" {
		t.Errorf("dry-run result should start with [dry-run]: %q", s)
	}
}

func TestResult_String_IncludesSkippedWarnings(t *testing.T) {
	r := Result{
		PaywalledEpisodesIncluded: 10,
		SkippedInternalPodcasts:   2,
	}
	s := r.String()
	if len(s) == 0 {
		t.Fatal("result string is empty")
	}
	// Both warnings must appear.
	for _, fragment := range []string{"10", "2", "PSUB", "internal"} {
		found := false
		for i := 0; i+len(fragment) <= len(s); i++ {
			if s[i:i+len(fragment)] == fragment {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("result string missing %q:\n%s", fragment, s)
		}
	}
}

func TestResult_String_NoWarningsWhenClear(t *testing.T) {
	r := Result{SubscriptionsAdded: 5}
	s := r.String()
	for _, bad := range []string{"note:", "PSUB", "internal"} {
		for i := 0; i+len(bad) <= len(s); i++ {
			if s[i:i+len(bad)] == bad {
				t.Errorf("result string should not include warning %q when counts are zero:\n%s", bad, s)
				break
			}
		}
	}
}

// ---- cross-feed (Plus-title) matching ----

// pubAt is a helper that returns a test pub date at the given hour.
func pubAt(hour int) time.Time {
	return time.Date(2024, 6, 15, hour, 0, 0, 0, time.UTC)
}

// TestMerge_CrossFeed_PlusVsPublic verifies that when one side has a podcast
// subscribed via the paid feed ("Fresh Air Plus") and the other has the public
// feed ("Fresh Air"), episodes are matched and conflict-resolved rather than
// appearing twice.
func TestMerge_CrossFeed_PlusVsPublic(t *testing.T) {
	plusFeed := "https://feeds.npr.org/plus/fresh-air"
	publicFeed := "https://feeds.npr.org/381444908/podcast.xml"
	pubDate := pubAt(12)

	// Source: Overcast with Plus feed, episode InProgress at 30 min.
	src := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: plusFeed, Title: "Fresh Air Plus"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:      plusFeed,
				PubDate:      pubDate,
				Title:        "Great Interview",
				PlayState:    model.PlayStateInProgress,
				PlayPosition: 30 * time.Minute,
			},
		},
	}

	// Destination: Apple with public feed, same episode already Played.
	dst := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: publicFeed, Title: "Fresh Air"},
		},
		Episodes: []model.EpisodeState{
			{
				FeedURL:   publicFeed,
				PubDate:   pubDate,
				Title:     "Great Interview",
				PlayState: model.PlayStatePlayed,
			},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	// FurthestWins: Played (dst) beats InProgress (src) → one episode, Played.
	if len(result.Episodes) != 1 {
		t.Fatalf("expected 1 merged episode, got %d (cross-feed match failed — duplicate entries)", len(result.Episodes))
	}
	if result.Episodes[0].PlayState != model.PlayStatePlayed {
		t.Errorf("FurthestWins: got PlayState=%v, want PlayStatePlayed", result.Episodes[0].PlayState)
	}
}

// TestMerge_CrossFeed_SourceWins verifies SourceWins strategy with cross-feed match.
// The merged episode must use the destination's FeedURL (so downstream writers that
// index by FeedURL can locate it), while play state comes from the source.
func TestMerge_CrossFeed_SourceWins(t *testing.T) {
	plusFeed := "https://feeds.npr.org/plus/fresh-air"
	publicFeed := "https://feeds.npr.org/381444908/podcast.xml"
	pubDate := pubAt(12)

	src := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: plusFeed, Title: "Fresh Air Plus"},
		},
		Episodes: []model.EpisodeState{
			{FeedURL: plusFeed, PubDate: pubDate, PlayState: model.PlayStateInProgress, PlayPosition: 10 * time.Minute},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: publicFeed, Title: "Fresh Air"},
		},
		Episodes: []model.EpisodeState{
			{FeedURL: publicFeed, PubDate: pubDate, PlayState: model.PlayStatePlayed},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.SourceWins})

	if len(result.Episodes) != 1 {
		t.Fatalf("SourceWins cross-feed: expected 1 episode, got %d", len(result.Episodes))
	}
	ep := result.Episodes[0]
	if ep.PlayState != model.PlayStateInProgress {
		t.Errorf("SourceWins: got PlayState=%v, want InProgress (src wins)", ep.PlayState)
	}
	// Destination feed URL must be preserved so Overcast/Apple writers can locate the episode.
	if ep.FeedURL != publicFeed {
		t.Errorf("SourceWins cross-feed: FeedURL should be destination's (%q), got %q", publicFeed, ep.FeedURL)
	}
}

// TestMerge_CrossFeed_DestIdentifiersPreservedFurthestWins verifies that when
// FurthestWins selects the source (e.g. source is further along), the merged
// episode still carries the destination's FeedURL and GUID so downstream
// writers keyed on destination identifiers can locate it.
func TestMerge_CrossFeed_DestIdentifiersPreservedFurthestWins(t *testing.T) {
	srcFeed := "https://feeds.example.com/public/show"
	dstFeed := "https://feeds.example.com/plus/show"
	pubDate := pubAt(10)

	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: srcFeed, Title: "My Show"}},
		Episodes: []model.EpisodeState{
			{GUID: "apple-internal-guid", FeedURL: srcFeed, PubDate: pubDate, PlayState: model.PlayStatePlayed},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: dstFeed, Title: "My Show Plus"}},
		Episodes: []model.EpisodeState{
			{GUID: "rss-native-guid", FeedURL: dstFeed, PubDate: pubDate, PlayState: model.PlayStateUnplayed},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	if len(result.Episodes) != 1 {
		t.Fatalf("expected 1 merged episode, got %d", len(result.Episodes))
	}
	ep := result.Episodes[0]
	// Source wins on play state (Played > Unplayed).
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("FurthestWins: PlayState got %v, want Played", ep.PlayState)
	}
	// Destination identifiers must be preserved.
	if ep.GUID != "rss-native-guid" {
		t.Errorf("FurthestWins cross-feed: GUID should be destination's %q, got %q", "rss-native-guid", ep.GUID)
	}
	if ep.FeedURL != dstFeed {
		t.Errorf("FurthestWins cross-feed: FeedURL should be destination's %q, got %q", dstFeed, ep.FeedURL)
	}
}

// TestMerge_CrossFeed_EarlyAccessTimingDifference verifies that when a
// private/member feed releases an episode hours before the public RSS
// (early-access window), the cross-feed match still succeeds because the
// key uses the UTC calendar date rather than an exact timestamp.
func TestMerge_CrossFeed_EarlyAccessTimingDifference(t *testing.T) {
	privateFeed := "https://feeds.example.com/members/pod-save-america"
	publicFeed := "https://feeds.example.com/pod-save-america"

	// Private feed releases at 02:00 ET = 06:00 UTC.
	privateRelease := time.Date(2024, 9, 12, 6, 0, 0, 0, time.UTC)
	// Public feed releases 8 hours later, same calendar day.
	publicRelease := time.Date(2024, 9, 12, 14, 0, 0, 0, time.UTC)

	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: privateFeed, Title: "Pod Save America"}},
		Episodes: []model.EpisodeState{
			{GUID: "apple-private-guid", FeedURL: privateFeed, Title: "JD Is Lame", PubDate: privateRelease, PlayState: model.PlayStatePlayed},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: publicFeed, Title: "Pod Save America"}},
		Episodes: []model.EpisodeState{
			{GUID: "rss-public-guid", FeedURL: publicFeed, Title: "JD Is Lame", PubDate: publicRelease, PlayState: model.PlayStateUnplayed},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	if len(result.Episodes) != 1 {
		t.Fatalf("early-access timing: expected 1 merged episode, got %d (8-hour pub-date gap broke cross-feed match)", len(result.Episodes))
	}
	ep := result.Episodes[0]
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("FurthestWins: PlayState got %v, want Played", ep.PlayState)
	}
	// Destination identifiers preserved.
	if ep.GUID != "rss-public-guid" {
		t.Errorf("GUID should be destination's %q, got %q", "rss-public-guid", ep.GUID)
	}
}

// TestMerge_PSUBEpisode_MatchesCrossFeed simulates the PSUB scenario: a source
// episode has an Apple-proprietary GUID and a public feed URL, but the destination
// has the same episode under a different (private/Plus) feed URL. The engine should
// match them via podcast title + pub date and preserve destination identifiers.
func TestMerge_PSUBEpisode_MatchesCrossFeed(t *testing.T) {
	publicFeed := "https://feeds.npr.org/381444908/podcast.xml"
	plusFeed := "https://feeds.npr.org/plus/381444908/podcast.xml"
	pubDate := pubAt(8)

	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: publicFeed, Title: "Fresh Air"}},
		Episodes: []model.EpisodeState{
			// PSUB episode: Apple-internal GUID, public feed URL, played state.
			{GUID: "apple-psub-hex-id", FeedURL: publicFeed, Title: "Episode Title", PubDate: pubDate, PlayState: model.PlayStatePlayed},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: plusFeed, Title: "Fresh Air Plus"}},
		Episodes: []model.EpisodeState{
			// Same episode in destination under Plus feed, unplayed.
			{GUID: "rss-guid-123", FeedURL: plusFeed, Title: "Episode Title", PubDate: pubDate, PlayState: model.PlayStateUnplayed},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	if len(result.Episodes) != 1 {
		t.Fatalf("PSUB cross-feed: expected 1 merged episode, got %d (match failed — duplicate entries)", len(result.Episodes))
	}
	ep := result.Episodes[0]
	if ep.PlayState != model.PlayStatePlayed {
		t.Errorf("PSUB cross-feed: PlayState got %v, want Played (Apple played > Overcast unplayed)", ep.PlayState)
	}
	if ep.GUID != "rss-guid-123" {
		t.Errorf("PSUB cross-feed: GUID should be destination's RSS GUID %q, got %q", "rss-guid-123", ep.GUID)
	}
	if ep.FeedURL != plusFeed {
		t.Errorf("PSUB cross-feed: FeedURL should be destination's Plus feed %q, got %q", plusFeed, ep.FeedURL)
	}
}

// TestMerge_CrossFeed_PlusPlusMatch verifies that two Plus feeds normalise to
// the same base title and are treated as the same podcast.
func TestMerge_CrossFeed_PlusPlusMatch(t *testing.T) {
	plusFeedA := "https://feeds.example.com/plus/show"
	plusFeedB := "https://feeds.example.com/plus2/show"
	pubDate := pubAt(9)

	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: plusFeedA, Title: "Great Show Plus"}},
		Episodes: []model.EpisodeState{
			{FeedURL: plusFeedA, PubDate: pubDate, PlayState: model.PlayStatePlayed},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: plusFeedB, Title: "Great Show Plus"}},
		Episodes: []model.EpisodeState{
			{FeedURL: plusFeedB, PubDate: pubDate, PlayState: model.PlayStateInProgress, PlayPosition: 5 * time.Minute},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	if len(result.Episodes) != 1 {
		t.Fatalf("Plus+Plus cross-feed: expected 1 merged episode, got %d", len(result.Episodes))
	}
	if result.Episodes[0].PlayState != model.PlayStatePlayed {
		t.Errorf("FurthestWins: got PlayState=%v, want Played", result.Episodes[0].PlayState)
	}
}

// TestMerge_CrossFeed_NoPrimaryMissUnrelated verifies that episodes from unrelated
// podcasts with different titles are not cross-matched just because their pub dates
// happen to coincide.
func TestMerge_CrossFeed_NoPrimaryMissUnrelated(t *testing.T) {
	feedA := "https://feeds.example.com/show-a"
	feedB := "https://feeds.example.com/show-b"
	pubDate := pubAt(12)

	src := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: feedA, Title: "Show A"}},
		Episodes: []model.EpisodeState{
			{FeedURL: feedA, PubDate: pubDate, PlayState: model.PlayStatePlayed},
		},
	}
	dst := &model.Library{
		Podcasts: []model.Podcast{{FeedURL: feedB, Title: "Show B"}},
		Episodes: []model.EpisodeState{
			{FeedURL: feedB, PubDate: pubDate, PlayState: model.PlayStateInProgress, PlayPosition: 15 * time.Minute},
		},
	}

	result := merge(src, dst, provider.WriteOptions{ConflictStrategy: provider.FurthestWins})

	// No cross-feed match (different titles): both episodes survive.
	if len(result.Episodes) != 2 {
		t.Errorf("unrelated podcasts at same pub date: expected 2 episodes (no cross-match), got %d", len(result.Episodes))
	}
}

// TestBuildCrossFeedIndex verifies the index structure produced for Plus and public feeds.
func TestBuildCrossFeedIndex(t *testing.T) {
	plusFeed := "https://feeds.npr.org/plus"
	pubDate := time.Date(2024, 3, 10, 14, 0, 0, 0, time.UTC)

	lib := &model.Library{
		Podcasts: []model.Podcast{
			{FeedURL: plusFeed, Title: "Fresh Air Plus"},
		},
		Episodes: []model.EpisodeState{
			{FeedURL: plusFeed, PubDate: pubDate, PlayState: model.PlayStatePlayed},
		},
	}

	idx := buildCrossFeedIndex(lib)

	wantKey := "xfeed:fresh air|2024-03-10"
	if _, ok := idx[wantKey]; !ok {
		t.Errorf("buildCrossFeedIndex: key %q not found in index; keys = %v", wantKey, keys(idx))
	}
}

func TestEngine_Run_SubscriptionsAdded_RespectsFilter(t *testing.T) {
	// Source has 3 podcasts; only "gamma" matches --podcast "gamma". Dst has none.
	// SubscriptionsAdded must be 1, not 3.
	src := &mockProvider{
		name: "src", caps: fullCaps,
		lib: &model.Library{
			Podcasts: []model.Podcast{
				{FeedURL: "https://feeds.example.com/alpha", Title: "Alpha Show"},
				{FeedURL: "https://feeds.example.com/beta", Title: "Beta Show"},
				{FeedURL: "https://feeds.example.com/gamma", Title: "Gamma Show"},
			},
		},
	}
	dst := &mockProvider{name: "dst", caps: fullCaps, lib: &model.Library{}}

	opts := provider.WriteOptions{
		ConflictStrategy: provider.FurthestWins,
		PodcastFilter:    []string{"gamma"},
	}
	result, err := New(src, dst).Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SubscriptionsAdded != 1 {
		t.Errorf("SubscriptionsAdded with podcast filter: got %d, want 1", result.SubscriptionsAdded)
	}
}

func keys[K comparable, V any](m map[K]V) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

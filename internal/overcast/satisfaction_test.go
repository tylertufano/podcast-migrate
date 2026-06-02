package overcast

// White-box tests for overcastAlreadySatisfied and the skip logic baked into
// buildOvercastIndex + doWritePlayState. These live in package overcast (not
// overcast_test) so they can access unexported types directly.

import (
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

func TestOvercastSkipReason(t *testing.T) {
	played := overcastIndexEntry{numericID: "1", currentState: model.PlayStatePlayed}
	inProgress300 := overcastIndexEntry{numericID: "1", currentState: model.PlayStateInProgress, currentPos: 300 * time.Second}
	unplayed := overcastIndexEntry{numericID: "1", currentState: model.PlayStateUnplayed}

	cases := []struct {
		name    string
		desired model.EpisodeState
		current overcastIndexEntry
		want    string
	}{
		{"played desired / overcast played → already_played",
			model.EpisodeState{PlayState: model.PlayStatePlayed}, played, "already_played"},
		{"played desired / overcast in-progress → write",
			model.EpisodeState{PlayState: model.PlayStatePlayed}, inProgress300, ""},
		{"played desired / overcast unplayed → write",
			model.EpisodeState{PlayState: model.PlayStatePlayed}, unplayed, ""},
		{"in-progress 200s / overcast 300s → already_ahead",
			model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 200 * time.Second}, inProgress300, "already_ahead"},
		{"in-progress 300s / overcast 300s → already_ahead (equal)",
			model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 300 * time.Second}, inProgress300, "already_ahead"},
		{"in-progress 400s / overcast 300s → write (source ahead)",
			model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 400 * time.Second}, inProgress300, ""},
		{"in-progress 400s / overcast played → already_played",
			model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 400 * time.Second}, played, "already_played"},
		{"in-progress / overcast unplayed → write",
			model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 100 * time.Second}, unplayed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overcastSkipReason(tc.desired, tc.current)
			if got != tc.want {
				t.Errorf("overcastSkipReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFindInOvercastIndex_StrictFeedMatch(t *testing.T) {
	pub := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	feed := "https://feeds.example.com/show"

	index := map[string]overcastIndexEntry{
		"feeddate:https://feeds.example.com/show|2024-03-01T09:00:00Z": {numericID: "date-match"},
		"feedtitle:https://feeds.example.com/show|episode one":         {numericID: "title-match"},
	}

	ep := model.EpisodeState{
		FeedURL:   feed,
		Title:     "Episode One",
		PubDate:   pub,
		PlayState: model.PlayStatePlayed,
	}

	// Without strict matching: date key found.
	if entry, ok := findInOvercastIndex(index, ep, false); !ok || entry.numericID != "date-match" {
		t.Errorf("non-strict, date key: got ok=%v id=%q, want true/date-match", ok, entry.numericID)
	}

	// Title-only scenario (remove date key to force title fallback).
	indexTitleOnly := map[string]overcastIndexEntry{
		"feedtitle:https://feeds.example.com/show|episode one": {numericID: "title-match"},
	}
	epNoDate := ep
	epNoDate.PubDate = time.Time{}

	if entry, ok := findInOvercastIndex(indexTitleOnly, epNoDate, false); !ok || entry.numericID != "title-match" {
		t.Errorf("non-strict, title key: got ok=%v id=%q, want true/title-match", ok, entry.numericID)
	}

	// With strict matching: title key skipped, returns not-found.
	if _, ok := findInOvercastIndex(indexTitleOnly, epNoDate, true); ok {
		t.Error("strict mode should skip feedtitle key, expected not-found")
	}
}

func TestOvercastAlreadySatisfied(t *testing.T) {
	played := overcastIndexEntry{
		numericID:    "999",
		currentState: model.PlayStatePlayed,
	}
	inProgress300 := overcastIndexEntry{
		numericID:    "999",
		currentState: model.PlayStateInProgress,
		currentPos:   300 * time.Second,
	}
	unplayed := overcastIndexEntry{
		numericID:    "999",
		currentState: model.PlayStateUnplayed,
	}

	cases := []struct {
		name    string
		desired model.EpisodeState
		current overcastIndexEntry
		want    bool
	}{
		{
			name:    "played desired / played in overcast → satisfied",
			desired: model.EpisodeState{PlayState: model.PlayStatePlayed},
			current: played,
			want:    true,
		},
		{
			name:    "played desired / in-progress in overcast → not satisfied",
			desired: model.EpisodeState{PlayState: model.PlayStatePlayed},
			current: inProgress300,
			want:    false,
		},
		{
			name:    "played desired / unplayed in overcast → not satisfied",
			desired: model.EpisodeState{PlayState: model.PlayStatePlayed},
			current: unplayed,
			want:    false,
		},
		{
			name:    "in-progress 200s desired / overcast at 300s → satisfied (overcast ahead)",
			desired: model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 200 * time.Second},
			current: inProgress300,
			want:    true,
		},
		{
			name:    "in-progress 300s desired / overcast at 300s → satisfied (equal)",
			desired: model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 300 * time.Second},
			current: inProgress300,
			want:    true,
		},
		{
			name:    "in-progress 400s desired / overcast at 300s → not satisfied (source ahead)",
			desired: model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 400 * time.Second},
			current: inProgress300,
			want:    false,
		},
		{
			name:    "in-progress 400s desired / overcast already played → satisfied",
			desired: model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 400 * time.Second},
			current: played,
			want:    true,
		},
		{
			name:    "in-progress desired / overcast unplayed → not satisfied",
			desired: model.EpisodeState{PlayState: model.PlayStateInProgress, PlayPosition: 100 * time.Second},
			current: unplayed,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overcastAlreadySatisfied(tc.desired, tc.current)
			if got != tc.want {
				t.Errorf("overcastAlreadySatisfied() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildOvercastIndex_StoresCurrentState(t *testing.T) {
	lib := &model.Library{
		Episodes: []model.EpisodeState{
			{
				GUID:         "overcast-id-1",
				FeedURL:      "https://feeds.example.com/show",
				Title:        "Episode One",
				PubDate:      time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				PlayState:    model.PlayStatePlayed,
				PlayPosition: 0,
			},
			{
				GUID:         "overcast-id-2",
				FeedURL:      "https://feeds.example.com/show",
				Title:        "Episode Two",
				PubDate:      time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
				PlayState:    model.PlayStateInProgress,
				PlayPosition: 250 * time.Second,
			},
		},
	}

	index := buildOvercastIndex(lib)

	ep1Key := "feeddate:https://feeds.example.com/show|2024-01-01T00:00:00Z"
	if entry, ok := index[ep1Key]; !ok {
		t.Errorf("ep1 not found in index by feeddate key")
	} else {
		if entry.numericID != "overcast-id-1" {
			t.Errorf("ep1 numericID: got %q, want %q", entry.numericID, "overcast-id-1")
		}
		if entry.currentState != model.PlayStatePlayed {
			t.Errorf("ep1 currentState: got %v, want PlayStatePlayed", entry.currentState)
		}
	}

	ep2Key := "feeddate:https://feeds.example.com/show|2024-01-02T00:00:00Z"
	if entry, ok := index[ep2Key]; !ok {
		t.Errorf("ep2 not found in index by feeddate key")
	} else {
		if entry.currentState != model.PlayStateInProgress {
			t.Errorf("ep2 currentState: got %v, want PlayStateInProgress", entry.currentState)
		}
		if entry.currentPos != 250*time.Second {
			t.Errorf("ep2 currentPos: got %v, want 250s", entry.currentPos)
		}
	}
}

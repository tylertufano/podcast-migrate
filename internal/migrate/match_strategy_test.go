package migrate_test

import (
	"testing"

	"github.com/tyler/podcast-migrate/internal/migrate"
)

func TestMatchStrategy_String(t *testing.T) {
	cases := []struct {
		s    migrate.MatchStrategy
		want string
	}{
		{migrate.MatchByGUID, "guid"},
		{migrate.MatchByFeedDate, "feeddate"},
		{migrate.MatchByFeedTitle, "feedtitle"},
		{migrate.MatchByTitleDate, "titledate"},
		{migrate.MatchByPodDate, "poddate"},
		{migrate.MatchByPodTitle, "podtitle"},
		{migrate.MatchStrategy(99), "MatchStrategy(99)"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("MatchStrategy(%d).String() = %q, want %q", int(tc.s), got, tc.want)
		}
	}
}

func TestMatchStrategy_Order(t *testing.T) {
	// Canonical priority order: lower iota = higher confidence.
	order := []migrate.MatchStrategy{
		migrate.MatchByGUID,
		migrate.MatchByFeedDate,
		migrate.MatchByFeedTitle,
		migrate.MatchByTitleDate,
		migrate.MatchByPodDate,
		migrate.MatchByPodTitle,
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Errorf("strategy order violated: %s (%d) should be > %s (%d)",
				order[i], int(order[i]), order[i-1], int(order[i-1]))
		}
	}
}

package pocketcasts

// White-box tests for helpers that were previously private to this package
// and have since moved to internal/migrate.

import (
	"testing"

	"github.com/tyler/podcast-migrate/internal/migrate"
)

// ---- migrate.TitleHasWordPrefix ----

func TestTitleHasWordPrefix_ExactMatch(t *testing.T) {
	if !migrate.TitleHasWordPrefix("pod save america", "pod save america") {
		t.Error("exact match should return true")
	}
}

func TestTitleHasWordPrefix_ShorterIsPrefix_WordBoundary(t *testing.T) {
	if !migrate.TitleHasWordPrefix("pod save america the podcast", "pod save america") {
		t.Error("shorter is a word-boundary prefix of longer")
	}
}

func TestTitleHasWordPrefix_ShorterIsPrefix_NoWordBoundary(t *testing.T) {
	if migrate.TitleHasWordPrefix("pod saveamerica", "pod save") {
		t.Error("prefix without word boundary should return false")
	}
}

func TestTitleHasWordPrefix_LongerIsShorterThanShorter(t *testing.T) {
	if migrate.TitleHasWordPrefix("abc", "abcdef") {
		t.Error("shorter longer than longer: should return false")
	}
}

func TestTitleHasWordPrefix_NotAPrefix(t *testing.T) {
	if migrate.TitleHasWordPrefix("breaking news from pod save america", "pod save america") {
		t.Error("substring-not-prefix should return false")
	}
}

func TestTitleHasWordPrefix_EmptyLonger(t *testing.T) {
	if migrate.TitleHasWordPrefix("", "abc") {
		t.Error("empty longer with non-empty shorter should be false")
	}
}

func TestTitleHasWordPrefix_EmptyShorter(t *testing.T) {
	result := migrate.TitleHasWordPrefix("something", "")
	if result {
		t.Error("empty shorter: expected false (next char after prefix is not a space)")
	}
}

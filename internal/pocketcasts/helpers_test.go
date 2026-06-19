package pocketcasts

// White-box tests for unexported helpers in provider.go.

import (
	"testing"
)

// ---- titleHasWordPrefix ----

func TestTitleHasWordPrefix_ExactMatch(t *testing.T) {
	if !titleHasWordPrefix("pod save america", "pod save america") {
		t.Error("exact match should return true")
	}
}

func TestTitleHasWordPrefix_ShorterIsPrefix_WordBoundary(t *testing.T) {
	if !titleHasWordPrefix("pod save america the podcast", "pod save america") {
		t.Error("shorter is a word-boundary prefix of longer")
	}
}

func TestTitleHasWordPrefix_ShorterIsPrefix_NoWordBoundary(t *testing.T) {
	// "pod save" is a prefix of "pod saveamerica" but the next char is not a space.
	if titleHasWordPrefix("pod saveamerica", "pod save") {
		t.Error("prefix without word boundary should return false")
	}
}

func TestTitleHasWordPrefix_LongerIsShorterThanShorter(t *testing.T) {
	if titleHasWordPrefix("abc", "abcdef") {
		t.Error("shorter longer than longer: should return false")
	}
}

func TestTitleHasWordPrefix_NotAPrefix(t *testing.T) {
	if titleHasWordPrefix("breaking news from pod save america", "pod save america") {
		t.Error("substring-not-prefix should return false")
	}
}

func TestTitleHasWordPrefix_EmptyLonger(t *testing.T) {
	// Empty string can't be a prefix of anything (shorter = "abc").
	if titleHasWordPrefix("", "abc") {
		t.Error("empty longer with non-empty shorter should be false")
	}
}

func TestTitleHasWordPrefix_EmptyShorter(t *testing.T) {
	// Empty shorter is a prefix of everything (vacuously true via strings.HasPrefix).
	// The function doesn't guard against this; verify behaviour is consistent.
	result := titleHasWordPrefix("something", "")
	// strings.HasPrefix("something", "") == true, len("") == len("") is false,
	// longer[0] == ' ' is false → function returns false.
	if result {
		t.Error("empty shorter: expected false (next char after prefix is not a space)")
	}
}

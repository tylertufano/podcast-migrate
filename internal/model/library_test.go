package model

import "testing"

func TestNormalizePlusTitle(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// NPR Plus " Plus" suffix
		{"Fresh Air Plus", "fresh air"},
		{"Planet Money Plus", "planet money"},
		{"Here & Now Plus", "here & now"},
		// "+" suffix (no space)
		{"Planet Money+", "planet money"},
		// " +" suffix (space before plus)
		{"Planet Money +", "planet money"},
		// Case-insensitive — input mixed case
		{"Fresh Air PLUS", "fresh air"},
		{"Fresh Air Plus", "fresh air"},
		// Already-public titles are unchanged (just lowercased)
		{"Fresh Air", "fresh air"},
		{"Planet Money", "planet money"},
		// Empty and whitespace
		{"", ""},
		{"  ", ""},
		// Whitespace around title
		{"  Fresh Air Plus  ", "fresh air"},
		// "Plus" alone is NOT stripped (no leading space, and it IS the whole title)
		{"Plus", "plus"},
		// Title that ends with "plus" as part of a word (no space before) — unchanged
		{"Surplus", "surplus"},
		// Multiple suffix occurrences — only the outermost is stripped
		{"Fresh Air Plus Plus", "fresh air plus"},
		// NYT subscriber feed pattern
		{"The Daily - Subscriber Feed (🔓)", "the daily"},
		{"The Daily - Subscriber Feed", "the daily"},
		// Member / private / premium variants
		{"Some Show - Member Feed (🔓)", "some show"},
		{"Some Show - Member Feed", "some show"},
		{"Some Show - Private Feed", "some show"},
		{"Some Show - Premium Feed", "some show"},
		// Standalone lock emoji
		{"Some Show (🔓)", "some show"},
		// Subscriber suffix + Plus suffix (combined)
		{"Show - Subscriber Feed Plus", "show - subscriber feed"},
		// Public title unchanged
		{"The Daily", "the daily"},
	}
	for _, tc := range cases {
		got := NormalizePlusTitle(tc.input)
		if got != tc.want {
			t.Errorf("NormalizePlusTitle(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

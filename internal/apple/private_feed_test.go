package apple

import (
	"strings"
	"testing"
	"time"
)

// ---- promptIncludePrivateAuthFrom ----

func TestPromptIncludePrivateAuth_IncludeResponse_ReturnsTrue(t *testing.T) {
	for _, input := range []string{"i", "I", "include", "INCLUDE"} {
		got := promptIncludePrivateAuthFrom(strings.NewReader(input+"\n"), 3)
		if !got {
			t.Errorf("input %q: got false, want true", input)
		}
	}
}

func TestPromptIncludePrivateAuth_ExcludeResponse_ReturnsFalse(t *testing.T) {
	for _, input := range []string{"e", "E", "exclude", "p", "k", "anything-else"} {
		got := promptIncludePrivateAuthFrom(strings.NewReader(input+"\n"), 3)
		if got {
			t.Errorf("input %q: got true, want false", input)
		}
	}
}

func TestPromptIncludePrivateAuth_EOF_ReturnsFalse(t *testing.T) {
	got := promptIncludePrivateAuthFrom(strings.NewReader(""), 3)
	if got {
		t.Error("EOF: got true, want false")
	}
}

// ---- promptPrivateFeedChoiceFrom ----

// classPrivateAuth feeds now use the same [p/k/u] menu as other classes
// (the upfront include/exclude decision has already been made by the caller).

func TestPromptPrivateFeedChoice_PrivateAuth_KChoiceReturnsKVSURL(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/private.rss", "https://itunes.example.com/canonical", "Secret Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader("k\n"), m, classPrivateAuth, 0)
	if got != m.kvsURL {
		t.Errorf("got %q, want KVS URL", got)
	}
}

func TestPromptPrivateFeedChoice_PrivateAuth_PChoiceReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/private.rss", "https://itunes.example.com/canonical", "Secret Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader("p\n"), m, classPrivateAuth, 0)
	if got != m.canonical {
		t.Errorf("got %q, want canonical", got)
	}
}

func TestPromptPrivateFeedChoice_PrivateAuth_DefaultReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/private.rss", "https://itunes.example.com/canonical", "Secret Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader("unrecognised\n"), m, classPrivateAuth, 0)
	if got != m.canonical {
		t.Errorf("got %q, want canonical", got)
	}
}

func TestPromptPrivateFeedChoice_PublicSubscriber_KChoiceReturnsKVSURL(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed.rss", "https://itunes.example.com/canonical", "My Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader("k\n"), m, classPublicSubscriber, 5)
	if got != m.kvsURL {
		t.Errorf("got %q, want KVS URL", got)
	}
}

func TestPromptPrivateFeedChoice_PublicSubscriber_PChoiceReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed.rss", "https://itunes.example.com/canonical", "My Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader("p\n"), m, classPublicSubscriber, 5)
	if got != m.canonical {
		t.Errorf("got %q, want canonical", got)
	}
}

func TestPromptPrivateFeedChoice_PublicSubscriber_UChoiceReturnsCustomURL(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed.rss", "https://itunes.example.com/canonical", "My Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader("u\nhttps://custom.example.com/feed.rss\n"), m, classPublicSubscriber, 5)
	if got != "https://custom.example.com/feed.rss" {
		t.Errorf("got %q, want custom URL", got)
	}
}

func TestPromptPrivateFeedChoice_EOFReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed.rss", "https://itunes.example.com/canonical", "My Show")
	got := promptPrivateFeedChoiceFrom(strings.NewReader(""), m, classPublicSubscriber, 0)
	if got != m.canonical {
		t.Errorf("EOF: got %q, want canonical", got)
	}
}

func TestPromptPrivateFeedChoice_URLTypedDirectlyAtChoicePrompt(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed.rss", "https://itunes.example.com/canonical", "My Show")
	for _, u := range []string{
		"https://feeds.supercast.com/feeds/hBkzfhw3fk29wFUCijcRezrq",
		"http://example.com/feed.rss",
	} {
		got := promptPrivateFeedChoiceFrom(strings.NewReader(u+"\n"), m, classPublicSubscriber, 0)
		if got != u {
			t.Errorf("input %q: got %q, want the URL itself", u, got)
		}
	}
}

// ---- normalizeEpTitle ----

func TestNormalizeEpTitle_LowercasesAndTrims(t *testing.T) {
	if got := normalizeEpTitle("  Hello World  "); got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestNormalizeEpTitle_UnescapesHTMLEntities(t *testing.T) {
	// Ampersand entity and accented character entity.
	if got := normalizeEpTitle("Q&amp;A"); got != "q&a" {
		t.Errorf("got %q, want %q", got, "q&a")
	}
}

func TestNormalizeEpTitle_PreservesNonASCII(t *testing.T) {
	if got := normalizeEpTitle("Café"); got != "café" {
		t.Errorf("got %q, want %q", got, "café")
	}
}

func TestNormalizeEpTitle_EmptyString(t *testing.T) {
	if got := normalizeEpTitle(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ---- classifyMismatchedFeed helpers ----

func makeEp(title string, pubDate time.Time) rssItem {
	return rssItem{Title: title, PubDate: pubDate}
}

// ---- classifyMismatchedFeed ----

func TestClassifyMismatchedFeed_EmptyKVS_ReturnsPrivateAuth(t *testing.T) {
	// Empty KVS items: URL is auth-gated or feed is blank.
	kvs := rssFeed{}
	itunes := rssFeed{Items: []rssItem{makeEp("Episode One", time.Now())}}
	class, n := classifyMismatchedFeed(kvs, itunes)
	if class != classPrivateAuth || n != 0 {
		t.Errorf("got (%v, %d), want (classPrivateAuth, 0)", class, n)
	}
}

func TestClassifyMismatchedFeed_IdenticalContent_ReturnsPublicEquivalent(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	ep1 := makeEp("Episode One", base)
	ep2 := makeEp("Episode Two", base.Add(7*24*time.Hour))
	kvs := rssFeed{Items: []rssItem{ep1, ep2}}
	itunes := rssFeed{Items: []rssItem{ep1, ep2}}
	class, n := classifyMismatchedFeed(kvs, itunes)
	if class != classPublicEquivalent || n != 0 {
		t.Errorf("got (%v, %d), want (classPublicEquivalent, 0)", class, n)
	}
}

func TestClassifyMismatchedFeed_KVSHasExclusiveEpisode_ReturnsPublicSubscriber(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	floor := base.Add(-14 * 24 * time.Hour) // oldest iTunes item — sets windowFloor
	kvs := rssFeed{Items: []rssItem{
		makeEp("Episode One", base),
		makeEp("Subscriber Bonus", base.Add(3*24*time.Hour)), // in window, absent from iTunes
	}}
	itunes := rssFeed{Items: []rssItem{
		makeEp("Episode One", base),
		makeEp("Old Episode", floor), // sets floor
	}}
	class, n := classifyMismatchedFeed(kvs, itunes)
	if class != classPublicSubscriber || n != 1 {
		t.Errorf("got (%v, %d), want (classPublicSubscriber, 1)", class, n)
	}
}

func TestClassifyMismatchedFeed_MultipleExclusiveEpisodes_CountsCorrectly(t *testing.T) {
	base := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	floor := base.Add(-30 * 24 * time.Hour) // oldest iTunes item
	kvs := rssFeed{Items: []rssItem{
		makeEp("Shared Episode", base),
		makeEp("Exclusive One", base.Add(24*time.Hour)),
		makeEp("Exclusive Two", base.Add(48*time.Hour)),
	}}
	itunes := rssFeed{Items: []rssItem{
		makeEp("Shared Episode", base),
		makeEp("Older Episode", floor), // sets floor
	}}
	class, n := classifyMismatchedFeed(kvs, itunes)
	if class != classPublicSubscriber || n != 2 {
		t.Errorf("got (%v, %d), want (classPublicSubscriber, 2)", class, n)
	}
}

func TestClassifyMismatchedFeed_KVSHasOlderEpisodes_ReturnsPublicArchive(t *testing.T) {
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	floor := base.Add(-14 * 24 * time.Hour)
	kvs := rssFeed{Items: []rssItem{
		makeEp("Episode One", base),
		makeEp("Episode Two", floor),
		makeEp("Archive Episode", floor.Add(-7*24*time.Hour)), // before floor → archive
	}}
	itunes := rssFeed{Items: []rssItem{
		makeEp("Episode One", base),
		makeEp("Episode Two", floor),
	}}
	class, n := classifyMismatchedFeed(kvs, itunes)
	if class != classPublicArchive || n != 0 {
		t.Errorf("got (%v, %d), want (classPublicArchive, 0)", class, n)
	}
}

func TestClassifyMismatchedFeed_KVSItemBeforeFloor_NotCountedAsExclusive(t *testing.T) {
	// A KVS item that is BEFORE windowFloor must not be counted as exclusive,
	// even if its title is absent from iTunes.
	base := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	floor := base.Add(-14 * 24 * time.Hour)
	kvs := rssFeed{Items: []rssItem{
		makeEp("Episode One", base),
		makeEp("Old Exclusive", floor.Add(-1*24*time.Hour)), // before floor — archive
	}}
	itunes := rssFeed{Items: []rssItem{
		makeEp("Episode One", base),
		makeEp("Floor Setter", floor),
	}}
	// The old KVS episode triggers classPublicArchive (before floor), not classPublicSubscriber.
	class, _ := classifyMismatchedFeed(kvs, itunes)
	if class == classPublicSubscriber {
		t.Error("items before windowFloor must not count as exclusive subscriber episodes")
	}
	if class != classPublicArchive {
		t.Errorf("got %v, want classPublicArchive", class)
	}
}

func TestClassifyMismatchedFeed_iTunesNoDatableItems_ReturnsPublicArchive(t *testing.T) {
	// When iTunes has no dateable items, windowFloor stays zero → public-archive.
	kvs := rssFeed{Items: []rssItem{makeEp("KVS Episode", time.Now())}}
	itunes := rssFeed{Items: []rssItem{{Title: "Undated episode"}}} // zero PubDate
	class, n := classifyMismatchedFeed(kvs, itunes)
	if class != classPublicArchive || n != 0 {
		t.Errorf("got (%v, %d), want (classPublicArchive, 0)", class, n)
	}
}

func TestClassifyMismatchedFeed_TitleNormalizationMatchesHTMLEntities(t *testing.T) {
	// An episode titled "Q&amp;A" in iTunes should match "Q&A" in KVS (same after unescaping).
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	kvs := rssFeed{Items: []rssItem{makeEp("Q&A Special", base)}}
	itunes := rssFeed{Items: []rssItem{makeEp("Q&amp;A Special", base)}}
	class, n := classifyMismatchedFeed(kvs, itunes)
	// Titles normalise to the same string → no exclusive episodes → classPublicEquivalent.
	if class != classPublicEquivalent || n != 0 {
		t.Errorf("got (%v, %d), want (classPublicEquivalent, 0) — HTML entity mismatch should normalise", class, n)
	}
}

// ---- privateFeedClass.String ----

func TestPrivateFeedClass_String(t *testing.T) {
	cases := []struct {
		class privateFeedClass
		want  string
	}{
		{classPrivateAuth, "private-auth"},
		{classPublicSubscriber, "public-subscriber"},
		{classPublicArchive, "public-archive"},
		{classPublicEquivalent, "public-equivalent"},
	}
	for _, tc := range cases {
		if got := tc.class.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", tc.class, got, tc.want)
		}
	}
}

// ---- resolveURL ----

func makeMismatch(kvsURL, canonical, title string) mismatchedFeed {
	return mismatchedFeed{
		clean:     kvsURL,
		kvsURL:    kvsURL,
		canonical: canonical,
		title:     title,
	}
}

func TestResolveURL_PublicMode_AlwaysReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	for _, class := range []privateFeedClass{classPrivateAuth, classPublicSubscriber, classPublicArchive, classPublicEquivalent} {
		got := resolveURL(PrivateFeedPublic, m, class, 0)
		if got != m.canonical {
			t.Errorf("PrivateFeedPublic, class=%v: got %q, want canonical", class, got)
		}
	}
}

func TestResolveURL_KVSMode_AlwaysReturnsKVSURL(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	for _, class := range []privateFeedClass{classPrivateAuth, classPublicSubscriber, classPublicArchive, classPublicEquivalent} {
		got := resolveURL(PrivateFeedKVS, m, class, 0)
		if got != m.kvsURL {
			t.Errorf("PrivateFeedKVS, class=%v: got %q, want kvsURL", class, got)
		}
	}
}

func TestResolveURL_SubscriberMode_PrivateAuth_ReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	got := resolveURL(PrivateFeedSubscriber, m, classPrivateAuth, 0)
	if got != m.canonical {
		t.Errorf("subscriber+classPrivateAuth: got %q, want canonical", got)
	}
}

func TestResolveURL_SubscriberMode_PublicEquivalent_ReturnsCanonical(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	got := resolveURL(PrivateFeedSubscriber, m, classPublicEquivalent, 0)
	if got != m.canonical {
		t.Errorf("subscriber+classPublicEquivalent: got %q, want canonical", got)
	}
}

func TestResolveURL_SubscriberMode_PublicSubscriber_ReturnsKVSURL(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	got := resolveURL(PrivateFeedSubscriber, m, classPublicSubscriber, 3)
	if got != m.kvsURL {
		t.Errorf("subscriber+classPublicSubscriber: got %q, want kvsURL", got)
	}
}

func TestResolveURL_SubscriberMode_PublicArchive_ReturnsKVSURL(t *testing.T) {
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	got := resolveURL(PrivateFeedSubscriber, m, classPublicArchive, 0)
	if got != m.kvsURL {
		t.Errorf("subscriber+classPublicArchive: got %q, want kvsURL", got)
	}
}

func TestResolveURL_UnknownMode_ReturnsCanonical(t *testing.T) {
	// An unrecognised mode value should fall through to canonical (safe default).
	m := makeMismatch("https://kvs.example.com/feed", "https://itunes.example.com/canonical", "Test Show")
	got := resolveURL(PrivateFeedMode("unknown"), m, classPublicSubscriber, 1)
	if got != m.canonical {
		t.Errorf("unknown mode: got %q, want canonical", got)
	}
}

// ---- ParsePrivateFeedMode ----

func TestParsePrivateFeedMode_ValidValues(t *testing.T) {
	cases := []struct {
		input string
		want  PrivateFeedMode
	}{
		{"public", PrivateFeedPublic},
		{"kvs", PrivateFeedKVS},
		{"subscriber", PrivateFeedSubscriber},
		{"custom", PrivateFeedCustom},
		{"PUBLIC", PrivateFeedPublic},  // case-insensitive
		{"KVS", PrivateFeedKVS},       // case-insensitive
		{"  kvs  ", PrivateFeedKVS},   // whitespace trimmed
	}
	for _, tc := range cases {
		got, err := ParsePrivateFeedMode(tc.input)
		if err != nil {
			t.Errorf("ParsePrivateFeedMode(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParsePrivateFeedMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParsePrivateFeedMode_InvalidValue_ReturnsError(t *testing.T) {
	invalids := []string{"auto", "detect", "none", "always", ""}
	for _, s := range invalids {
		_, err := ParsePrivateFeedMode(s)
		if err == nil {
			t.Errorf("ParsePrivateFeedMode(%q): expected error, got nil", s)
		}
	}
}

func TestParsePrivateFeedMode_ErrorMessageContainsValidOptions(t *testing.T) {
	_, err := ParsePrivateFeedMode("bogus")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, valid := range []string{"public", "kvs", "subscriber", "custom"} {
		if !strings.Contains(msg, valid) {
			t.Errorf("error message %q does not mention valid option %q", msg, valid)
		}
	}
}

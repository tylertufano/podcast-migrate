package apple

// private_feed.go — URL resolution strategy for feeds where the Apple KVS
// subscription URL differs from the iTunes canonical URL.
//
// This arises when a podcast migrates hosts (feed URL changes) or when a
// publisher offers a separate subscriber feed with bonus episodes. Apple
// Podcasts stores the URL the user originally subscribed to, which may have
// subscriber-only content not present in the shorter-window iTunes canonical.

import (
	"bufio"
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"time"
)

// PrivateFeedMode controls URL selection for feeds where the KVS subscription
// URL differs from the iTunes canonical URL.
type PrivateFeedMode string

const (
	// PrivateFeedPublic always uses the iTunes canonical URL.
	// Subscriber-only episodes absent from the public feed are dropped.
	PrivateFeedPublic PrivateFeedMode = "public"

	// PrivateFeedKVS always uses the KVS subscription URL as-is.
	PrivateFeedKVS PrivateFeedMode = "kvs"

	// PrivateFeedSubscriber uses heuristics to keep the KVS URL when it
	// provides genuine subscriber access (auth-required OR has content absent
	// from the iTunes canonical feed) and falls back to iTunes canonical when
	// the KVS URL is merely a longer-window variant of the same public content.
	// This is the default.
	PrivateFeedSubscriber PrivateFeedMode = "subscriber"

	// PrivateFeedCustom prompts the user interactively for each mismatched feed.
	// Requires a TTY; returns an error when stdout is not a terminal.
	PrivateFeedCustom PrivateFeedMode = "custom"
)

// privateFeedClass is the classification result for a single mismatched feed.
type privateFeedClass int

const (
	// classPrivateAuth: KVS URL is not publicly accessible (auth-required or
	// empty response). Retaining the KVS URL is the only access path.
	classPrivateAuth privateFeedClass = iota

	// classPublicSubscriber: KVS URL is publicly accessible but contains
	// episodes absent from the iTunes canonical feed in the same date window.
	// The KVS URL surfaces subscriber content via an unauthenticated path.
	classPublicSubscriber

	// classPublicArchive: KVS URL is publicly accessible; all episodes in the
	// iTunes canonical window also appear in the KVS feed, AND the KVS feed
	// has older episodes beyond that window. The extended archive is the
	// subscriber benefit.
	classPublicArchive

	// classPublicEquivalent: KVS URL is publicly accessible and functionally
	// identical to the iTunes canonical — same episodes in the window and no
	// older episodes in the KVS feed. No subscriber benefit over using canonical.
	classPublicEquivalent
)

func (c privateFeedClass) String() string {
	switch c {
	case classPrivateAuth:
		return "private-auth"
	case classPublicSubscriber:
		return "public-subscriber"
	case classPublicArchive:
		return "public-archive"
	default:
		return "public-equivalent"
	}
}

// mismatchedFeed records a single subscription where the KVS URL ≠ iTunes canonical.
type mismatchedFeed struct {
	clean     string // cleaned KVS URL (key in cleanToCanonical)
	kvsURL    string // the KVS subscription URL (= clean)
	canonical string // iTunes canonical URL
	title     string // podcast title (for logging / prompts)
}

// classifyMismatchedFeed classifies a feed based on its RSS content.
// kvsRSS is the RSS parsed from kvsURL (its Items may be empty if the URL
// was inaccessible). itunesRSS is the RSS parsed from the iTunes canonical.
// Returns the class and the number of exclusive episodes (> 0 for classPublicSubscriber).
func classifyMismatchedFeed(kvsRSS rssFeed, itunesRSS rssFeed) (privateFeedClass, int) {
	if len(kvsRSS.Items) == 0 {
		// Either the KVS URL was inaccessible (auth-gated) or the feed is
		// genuinely empty. In both cases we can't use the KVS URL for episodes.
		return classPrivateAuth, 0
	}

	// Determine the comparison window: oldest pub date in the iTunes canonical feed.
	var windowFloor time.Time
	for _, item := range itunesRSS.Items {
		if !item.PubDate.IsZero() && (windowFloor.IsZero() || item.PubDate.Before(windowFloor)) {
			windowFloor = item.PubDate
		}
	}
	if windowFloor.IsZero() {
		// iTunes feed has no dateable episodes — can't compare.
		return classPublicArchive, 0
	}

	// Build the iTunes title set (normalised for case/entity comparison).
	itunesTitles := make(map[string]bool, len(itunesRSS.Items))
	for _, item := range itunesRSS.Items {
		itunesTitles[normalizeEpTitle(item.Title)] = true
	}

	// Count KVS episodes in the iTunes window that are absent from iTunes.
	var exclusive int
	for _, item := range kvsRSS.Items {
		if item.PubDate.Before(windowFloor) {
			continue
		}
		if !itunesTitles[normalizeEpTitle(item.Title)] {
			exclusive++
		}
	}

	if exclusive > 0 {
		return classPublicSubscriber, exclusive
	}

	// No exclusive episodes in the window. Check whether KVS has older episodes
	// that extend the archive beyond what iTunes carries.
	for _, item := range kvsRSS.Items {
		if !item.PubDate.IsZero() && item.PubDate.Before(windowFloor) {
			return classPublicArchive, 0 // deeper archive — subscriber benefit
		}
	}
	return classPublicEquivalent, 0 // same content and depth — no subscriber benefit
}

func normalizeEpTitle(s string) string {
	return strings.ToLower(strings.TrimSpace(html.UnescapeString(s)))
}

// resolveURL picks the URL to use for a mismatched feed given the mode and classification.
// For PrivateFeedCustom, call promptPrivateFeedChoice instead.
func resolveURL(mode PrivateFeedMode, m mismatchedFeed, class privateFeedClass, exclusiveEps int) string {
	switch mode {
	case PrivateFeedPublic:
		return m.canonical
	case PrivateFeedKVS:
		return m.kvsURL
	case PrivateFeedSubscriber:
		switch class {
		case classPrivateAuth, classPublicEquivalent:
			// classPrivateAuth: KVS URL inaccessible — subscriber content unreachable.
			// classPublicEquivalent: KVS and iTunes are identical in content and depth.
			// In both cases the canonical is the better choice.
			return m.canonical
		case classPublicSubscriber:
			fmt.Printf("apple: %q — KVS URL has %d subscriber episode(s) not in iTunes canonical;\n"+
				"  retaining KVS URL to preserve access (--private-feed=subscriber)\n"+
				"  note: this URL is publicly accessible without authentication\n",
				m.title, exclusiveEps)
			return m.kvsURL
		default: // classPublicArchive
			// Extended archive beyond the iTunes window — the depth is the subscriber benefit.
			return m.kvsURL
		}
	}
	return m.canonical
}

// promptIncludePrivateAuth asks the user once, before the per-feed loop,
// whether private-auth class feeds should be included in the review.
// Returns true if the user chooses to include them. Reads from os.Stdin.
func promptIncludePrivateAuth(count int) bool {
	return promptIncludePrivateAuthFrom(os.Stdin, count)
}

// promptIncludePrivateAuthFrom is the testable core of promptIncludePrivateAuth.
func promptIncludePrivateAuthFrom(r io.Reader, count int) bool {
	sc := bufio.NewScanner(r)
	fmt.Printf("\n  %d feed(s) classified private-auth (KVS URL not publicly accessible — auth-gated or empty).\n", count)
	fmt.Println("  [i] include  — review each feed below (destinations that cannot reach the URL will")
	fmt.Println("                 collect it in the skipped-feeds OPML for manual import)")
	fmt.Println("  [e] exclude  — use iTunes URL for all private-auth feeds (default)")
	fmt.Print("  Choice [i/e]: ")
	if !sc.Scan() {
		fmt.Println()
		return false
	}
	ch := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ch == "i" || ch == "include"
}

// promptPrivateFeedChoice interactively asks the user which URL to use for a
// mismatched feed. Used only in PrivateFeedCustom mode. Reads from os.Stdin.
func promptPrivateFeedChoice(m mismatchedFeed, class privateFeedClass, exclusiveEps int) string {
	return promptPrivateFeedChoiceFrom(os.Stdin, m, class, exclusiveEps)
}

// promptPrivateFeedChoiceFrom is the testable core of promptPrivateFeedChoice.
// Shows the full [p/k/u] menu for all classes, including classPrivateAuth when
// the user has already opted in via promptIncludePrivateAuth.
func promptPrivateFeedChoiceFrom(r io.Reader, m mismatchedFeed, class privateFeedClass, exclusiveEps int) string {
	sc := bufio.NewScanner(r)

	var classLabel string
	switch class {
	case classPrivateAuth:
		classLabel = "private-auth (KVS URL not publicly accessible — auth-gated or empty)"
	case classPublicSubscriber:
		classLabel = fmt.Sprintf("public-subscriber (%d episode(s) in KVS absent from iTunes)", exclusiveEps)
	case classPublicArchive:
		classLabel = "public-archive (KVS URL has deeper archive than iTunes canonical)"
	default: // classPublicEquivalent
		classLabel = "public-equivalent (KVS URL identical in content and depth to iTunes canonical)"
	}

	fmt.Printf("\n  Feed: %q\n", m.title)
	fmt.Printf("  Detection: %s\n", classLabel)
	fmt.Printf("  KVS URL:    %s\n", m.kvsURL)
	fmt.Printf("  iTunes URL: %s\n", m.canonical)
	fmt.Println()
	fmt.Println("  [p] public  — use iTunes URL (public episodes only)")
	fmt.Println("  [k] kvs     — use KVS URL")
	fmt.Println("  [u] url     — enter a custom URL")
	fmt.Print("  Choice [p/k/u]: ")

	if !sc.Scan() {
		fmt.Println()
		return m.canonical
	}
	input := strings.TrimSpace(sc.Text())
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		return input
	}
	switch strings.ToLower(input) {
	case "k", "kvs":
		return m.kvsURL
	case "u", "url":
		fmt.Print("  URL: ")
		if sc.Scan() {
			if u := strings.TrimSpace(sc.Text()); u != "" {
				return u
			}
		}
	}
	return m.canonical
}

// ParsePrivateFeedMode parses the --private-feed flag value.
func ParsePrivateFeedMode(s string) (PrivateFeedMode, error) {
	switch PrivateFeedMode(strings.ToLower(strings.TrimSpace(s))) {
	case PrivateFeedPublic:
		return PrivateFeedPublic, nil
	case PrivateFeedKVS:
		return PrivateFeedKVS, nil
	case PrivateFeedSubscriber:
		return PrivateFeedSubscriber, nil
	case PrivateFeedCustom:
		return PrivateFeedCustom, nil
	default:
		return "", fmt.Errorf("unknown --private-feed value %q: must be public, kvs, subscriber, or custom", s)
	}
}

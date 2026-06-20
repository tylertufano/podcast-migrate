package apple

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// TestParseSubscriptionXML validates the XML plist subscription parser without
// requiring live credentials.
func TestParseSubscriptionXML(t *testing.T) {
	// Minimal subscription XML plist with two entries: one public (with PID),
	// one private (no PID), using real-looking values.
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>2</key>
	<array>
		<dict>
			<key>addedDate</key>
			<date>2023-01-15T10:30:00Z</date>
			<key>darkCount</key>
			<integer>0</integer>
			<key>feedURL</key>
			<string>https://feeds.npr.org/344098539/podcast.xml</string>
			<key>lastTouchDate</key>
			<date>2025-06-01T08:00:00Z</date>
			<key>playbackNewestToOldest</key>
			<true/>
			<key>podcastPID</key>
			<integer>344098539</integer>
			<key>showTypeSetting</key>
			<integer>1</integer>
			<key>sortAscending</key>
			<false/>
			<key>storeCollectionId</key>
			<integer>460183386</integer>
			<key>subscribed</key>
			<true/>
			<key>title</key>
			<string>Fresh Air</string>
			<key>updatedDate</key>
			<date>2025-06-01T08:00:00Z</date>
			<key>uuid</key>
			<string>AABBCCDD-1234-5678-ABCD-000011112222</string>
		</dict>
		<dict>
			<key>addedDate</key>
			<date>2024-03-10T12:00:00Z</date>
			<key>darkCount</key>
			<integer>3</integer>
			<key>feedURL</key>
			<string>https://podsync.tufanito.com/thehangup.xml</string>
			<key>lastTouchDate</key>
			<date>2025-05-20T07:00:00Z</date>
			<key>playbackNewestToOldest</key>
			<false/>
			<key>showTypeSetting</key>
			<integer>1</integer>
			<key>sortAscending</key>
			<false/>
			<key>subscribed</key>
			<true/>
			<key>title</key>
			<string>The Hang Up</string>
			<key>updatedDate</key>
			<date>2025-05-20T07:00:00Z</date>
			<key>uuid</key>
			<string>DDEEFF00-9999-0000-BBBB-CCCCDDDDEEEE</string>
		</dict>
	</array>
	<key>DataVersion</key>
	<integer>2</integer>
</dict>
</plist>`

	subs, err := parseSubscriptionXML(xml)
	if err != nil {
		t.Fatalf("parseSubscriptionXML: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d subscriptions, want 2", len(subs))
	}

	// Public feed (Fresh Air)
	fa := subs[0]
	if fa.FeedURL != "https://feeds.npr.org/344098539/podcast.xml" {
		t.Errorf("Fresh Air feedURL = %q", fa.FeedURL)
	}
	if fa.Title != "Fresh Air" {
		t.Errorf("title = %q", fa.Title)
	}
	if fa.PodcastPID != 344098539 {
		t.Errorf("PodcastPID = %d, want 344098539", fa.PodcastPID)
	}
	if fa.StoreCollectionID != 460183386 {
		t.Errorf("StoreCollectionID = %d, want 460183386", fa.StoreCollectionID)
	}
	if fa.Subscribed != 1 {
		t.Errorf("Subscribed = %d, want 1", fa.Subscribed)
	}
	if !fa.PlaybackNewestToOldest {
		t.Errorf("PlaybackNewestToOldest = false, want true")
	}
	if fa.AddedDate.Year() != 2023 {
		t.Errorf("AddedDate year = %d, want 2023", fa.AddedDate.Year())
	}

	// Private feed (The Hang Up) — no PID fields
	hu := subs[1]
	if hu.FeedURL != "https://podsync.tufanito.com/thehangup.xml" {
		t.Errorf("The Hang Up feedURL = %q", hu.FeedURL)
	}
	if hu.PodcastPID != 0 {
		t.Errorf("PodcastPID = %d, want 0 (private feed)", hu.PodcastPID)
	}
	if hu.Subscribed != 1 {
		t.Errorf("Subscribed = %d, want 1", hu.Subscribed)
	}
	if hu.DarkCount != 3 {
		t.Errorf("DarkCount = %d, want 3", hu.DarkCount)
	}
	t.Logf("parseSubscriptionXML OK: %d subscriptions", len(subs))
}

// TestKVSPodcastsDomainLive validates the com.apple.podcasts domain integration.
// Run with: go test -v -tags integration ./internal/apple/ -run TestKVSPodcastsDomainLive
// Requires APPLE_KVS_DSID and APPLE_KVS_COOKIES to be set.
func TestKVSPodcastsDomainLive(t *testing.T) {
	if os.Getenv("APPLE_KVS_DSID") == "" {
		t.Skip("APPLE_KVS_DSID not set — skipping live KVS test")
	}

	kvs, err := NewKVSWriter("")
	if err != nil {
		t.Fatalf("NewKVSWriter: %v", err)
	}

	ctx := context.Background()

	t.Run("initPodcastsDomain", func(t *testing.T) {
		if err := kvs.initPodcastsDomain(ctx); err != nil {
			t.Fatalf("initPodcastsDomain: %v", err)
		}
		t.Logf("subscriptions: %d", len(kvs.subscriptions))
		t.Logf("play state feeds: %d", len(kvs.podcastsFeeds))
		t.Logf("sub version: %q", kvs.subVersion)
		if kvs.subVersion == "" {
			t.Error("subVersion is empty — subscription list did not parse")
		}
	})

	t.Run("lookupEpisodeViaPlayState", func(t *testing.T) {
		// The Hang Up is a private feed on podsync.tufanito.com.
		// Its first episode GUID from the test script output is "NAdCiIDbbSk".
		feedURL := "https://podsync.tufanito.com/thehangup.xml"
		guid := "NAdCiIDbbSk"
		expectedMeta := "2f656deef7cfc8eff2cfb4eaab4b23ef"

		metaID, ok := kvs.lookupEpisodeViaPlayState(feedURL, guid)
		if !ok {
			t.Errorf("lookupEpisodeViaPlayState(%q, %q) = not found", feedURL, guid)
			return
		}
		if metaID != expectedMeta {
			t.Errorf("metadataIdentifier = %q, want %q", metaID, expectedMeta)
		}
		t.Logf("metadataIdentifier for The Hang Up ep %q: %s", guid, metaID)
	})

	t.Run("IsSubscribed_existing", func(t *testing.T) {
		// The Hang Up (private podsync feed) should already be subscribed.
		// If not, this test is expected to skip — the feed may have been removed.
		feedURL := "https://podsync.tufanito.com/thehangup.xml"
		if !kvs.IsSubscribed(feedURL) {
			t.Logf("WARN: IsSubscribed(%q) = false — feed may have been removed or credentials changed", feedURL)
		} else {
			t.Logf("IsSubscribed(%q) = true OK", feedURL)
		}
	})

	t.Run("Subscribe_new_feed", func(t *testing.T) {
		if testing.Short() {
			t.Skip("skipping live write in short mode")
		}
		if kvs.subVersion == "" {
			t.Skip("subVersion empty — subscription list was not parsed, skipping write test to avoid data loss")
		}

		// Use a throwaway feed URL unlikely to be in the subscription list.
		testFeed := "https://example-test-podcast.invalid/rss"
		testTitle := "Test Podcast (podcast-migrate integration test)"

		// Check it's not subscribed first.
		if kvs.IsSubscribed(testFeed) {
			t.Log("test feed already subscribed — skipping subscribe test")
			return
		}

		prevCount := len(kvs.subscriptions)

		// Subscribe.
		if _, err := kvs.Subscribe(ctx, testFeed, testTitle); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		t.Logf("Subscribed to test feed; sub version now: %q  total subs: %d", kvs.subVersion, len(kvs.subscriptions))

		if len(kvs.subscriptions) != prevCount+1 {
			t.Errorf("subscription count after Subscribe = %d, want %d", len(kvs.subscriptions), prevCount+1)
		}

		// Verify it's in the list.
		if !kvs.IsSubscribed(testFeed) {
			t.Error("IsSubscribed after Subscribe = false, want true")
		}

		// Unsubscribe.
		if err := kvs.Unsubscribe(ctx, testFeed); err != nil {
			t.Fatalf("Unsubscribe: %v", err)
		}
		if kvs.IsSubscribed(testFeed) {
			t.Error("IsSubscribed after Unsubscribe = true, want false")
		}
		t.Logf("Subscribe/Unsubscribe round-trip OK  final sub version: %q", kvs.subVersion)
	})

	t.Run("print_subscription_list_sample", func(t *testing.T) {
		// Print first 5 subscriptions for manual inspection.
		for i, s := range kvs.subscriptions {
			if i >= 5 {
				break
			}
			fmt.Printf("  [%d] %s (subscribed=%d, PID=%d)\n", i, s.Title, s.Subscribed, s.PodcastPID)
		}
	})
}

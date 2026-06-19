package apple_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/apple"
	"github.com/tyler/podcast-migrate/internal/model"
	"github.com/tyler/podcast-migrate/internal/provider"
)

const appleSubscriptionOPML = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Fresh Air" type="rss" xmlUrl="https://feeds.npr.org/381444908/podcast.xml"/>
  </body>
</opml>`

func writeAppleOPML(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "subs.opml")
	if err := os.WriteFile(p, []byte(appleSubscriptionOPML), 0644); err != nil {
		t.Fatalf("write OPML: %v", err)
	}
	return p
}

// ---- Name ----

func TestAppleProvider_Name(t *testing.T) {
	p := apple.NewProvider("/nonexistent/path.sqlite", "")
	if p.Name() != "Apple Podcasts" {
		t.Errorf("Name: got %q, want %q", p.Name(), "Apple Podcasts")
	}
}

// ---- Capabilities ----

func TestAppleProvider_Capabilities_NoCredentials(t *testing.T) {
	p := apple.NewProvider("/nonexistent/path.sqlite", "")
	caps := p.Capabilities()
	if !caps.ReadSubscriptions {
		t.Error("ReadSubscriptions should always be true")
	}
	if !caps.ReadPlayState {
		t.Error("ReadPlayState should always be true")
	}
	if caps.WritePlayState {
		t.Error("WritePlayState should be false without credentials")
	}
	if caps.WriteSubscriptions {
		t.Error("WriteSubscriptions is never supported")
	}
}

func TestAppleProvider_Capabilities_WithCredentials(t *testing.T) {
	p := apple.NewProvider("/nonexistent/path.sqlite", "")
	p.SetWebAPICredentials("test-bearer", "test-media-user")
	caps := p.Capabilities()
	if !caps.WritePlayState {
		t.Error("WritePlayState should be true after SetWebAPICredentials")
	}
}

// ---- SetSinceTime ----

func TestAppleProvider_SetSinceTime_StoresValue(t *testing.T) {
	// SetSinceTime's effect is visible through GetLibrary with a SQLite path that
	// doesn't exist. We verify the method doesn't panic and only test its observable
	// effect indirectly (SQLite read would filter by the time if the DB existed).
	p := apple.NewProvider("/nonexistent/path.sqlite", "")
	p.SetSinceTime(time.Now().Add(-24 * time.Hour))
	// No panic = pass. Effect on filtering is covered by SQLiteReader tests.
}

// ---- SetLibrary error paths ----

func TestAppleProvider_SetLibrary_OnlySubscriptions_ReturnsError(t *testing.T) {
	p := apple.NewProvider("/nonexistent/path.sqlite", "")
	p.SetWebAPICredentials("bearer", "media-user")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{OnlySubscriptions: true})
	if err == nil {
		t.Fatal("expected error for OnlySubscriptions (Apple has no subscription write API)")
	}
	var capErr *provider.ErrCapabilityUnsupported
	if !isCapabilityUnsupported(err, &capErr) {
		t.Errorf("expected ErrCapabilityUnsupported, got %T: %v", err, err)
	}
}

func TestAppleProvider_SetLibrary_NoCredentials_ReturnsError(t *testing.T) {
	p := apple.NewProvider("/nonexistent/path.sqlite", "")
	err := p.SetLibrary(context.Background(), &model.Library{}, provider.WriteOptions{})
	if err == nil {
		t.Fatal("expected error when no web API credentials are set")
	}
	// Error message should hint at what's needed.
	if !strings.Contains(err.Error(), "apple-bearer-token") && !strings.Contains(err.Error(), "credentials") {
		t.Errorf("error should mention credentials, got: %v", err)
	}
}

// ---- GetLibrary fallback paths ----

func TestAppleProvider_GetLibrary_NoSQLite_NoOPML_ReturnsError(t *testing.T) {
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", "") // no OPML fallback
	_, err := p.GetLibrary(context.Background())
	if err == nil {
		t.Error("expected error when SQLite not accessible and no OPML fallback")
	}
}

func TestAppleProvider_GetLibrary_NoSQLite_WithOPMLFallback_ReadsOPML(t *testing.T) {
	opmlPath := writeAppleOPML(t)
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", opmlPath)
	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary with OPML fallback: %v", err)
	}
	if len(lib.Podcasts) != 1 {
		t.Errorf("got %d podcasts from OPML fallback, want 1", len(lib.Podcasts))
	}
	if lib.Podcasts[0].Title != "Fresh Air" {
		t.Errorf("podcast title: got %q, want %q", lib.Podcasts[0].Title, "Fresh Air")
	}
}

func TestAppleProvider_GetLibrary_OPMLFallback_NoEpisodes(t *testing.T) {
	// OPML fallback provides subscriptions only — no play state.
	opmlPath := writeAppleOPML(t)
	p := apple.NewProvider("/nonexistent/MTLibrary.sqlite", opmlPath)
	lib, err := p.GetLibrary(context.Background())
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if len(lib.Episodes) != 0 {
		t.Errorf("OPML fallback should return no episodes; got %d", len(lib.Episodes))
	}
}

// isCapabilityUnsupported returns true if err can be unwrapped to
// *provider.ErrCapabilityUnsupported and sets *out to that value.
func isCapabilityUnsupported(err error, out **provider.ErrCapabilityUnsupported) bool {
	if err == nil {
		return false
	}
	type unwrapper interface{ Unwrap() error }
	for e := err; e != nil; {
		if ce, ok := e.(*provider.ErrCapabilityUnsupported); ok {
			*out = ce
			return true
		}
		u, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	// Also try direct type assertion.
	if ce, ok := err.(*provider.ErrCapabilityUnsupported); ok {
		*out = ce
		return true
	}
	return strings.Contains(err.Error(), "is not supported")
}

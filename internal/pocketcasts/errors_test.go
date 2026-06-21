package pocketcasts_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyler/podcast-migrate/internal/httputil"
	"github.com/tyler/podcast-migrate/internal/pocketcasts"
)

// ---- RateLimitError ----

func TestRateLimitError_ErrorString_ContainsStatusAndWait(t *testing.T) {
	// Get a RateLimitError by calling a web function against a 429 server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	mux := http.NewServeMux()
	mux.Handle("/", srv.Config.Handler)
	restore := func() {
		pocketcasts.SetBaseURLForTest("https://api.pocketcasts.com")
	}
	pocketcasts.SetBaseURLForTest(srv.URL)
	defer restore()

	client := srv.Client()
	_, err := pocketcasts.FetchSubscribedPodcasts(context.Background(), client)
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
	var rl *httputil.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if rl.Wait != 42*time.Second {
		t.Errorf("Wait: got %v, want 42s", rl.Wait)
	}
	msg := rl.Error()
	if !strings.Contains(msg, "429") {
		t.Errorf("Error() should mention 429: %q", msg)
	}
	if !strings.Contains(msg, "42s") {
		t.Errorf("Error() should mention wait duration: %q", msg)
	}
}

// ---- TransientError ----

func TestTransientError_ErrorString_ContainsCauseMessage(t *testing.T) {
	// Get a TransientError by calling a web function against a 500 server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	pocketcasts.SetBaseURLForTest(srv.URL)
	defer pocketcasts.SetBaseURLForTest("https://api.pocketcasts.com")

	client := srv.Client()
	_, err := pocketcasts.FetchSubscribedPodcasts(context.Background(), client)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	var te *httputil.TransientError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TransientError, got %T: %v", err, err)
	}
	msg := te.Error()
	if msg == "" {
		t.Error("TransientError.Error() should return non-empty string")
	}
	// Error message should contain context about which endpoint failed.
	if !strings.Contains(msg, "500") && !strings.Contains(msg, "pocketcasts") {
		t.Errorf("TransientError.Error() should mention context: %q", msg)
	}
}

func TestTransientError_Unwrap_ReturnsWrappedCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // 502
	}))
	defer srv.Close()

	pocketcasts.SetBaseURLForTest(srv.URL)
	defer pocketcasts.SetBaseURLForTest("https://api.pocketcasts.com")

	_, err := pocketcasts.FetchSubscribedPodcasts(context.Background(), srv.Client())
	if err == nil {
		t.Fatal("expected error for 502")
	}
	var te *httputil.TransientError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TransientError, got %T", err)
	}
	cause := errors.Unwrap(te)
	if cause == nil {
		t.Error("TransientError.Unwrap() should return non-nil cause")
	}
}

// ---- FetchExportFeedURLs ----

func TestFetchExportFeedURLs_EmptyUUIDs_ReturnsNil(t *testing.T) {
	// Empty UUID slice must not make a network call.
	result, err := pocketcasts.FetchExportFeedURLs(context.Background(), http.DefaultClient, nil)
	if err != nil {
		t.Fatalf("FetchExportFeedURLs nil: %v", err)
	}
	if result != nil {
		t.Error("empty UUIDs should return nil, nil")
	}

	result, err = pocketcasts.FetchExportFeedURLs(context.Background(), http.DefaultClient, []string{})
	if err != nil {
		t.Fatalf("FetchExportFeedURLs empty: %v", err)
	}
	if result != nil {
		t.Error("empty UUIDs should return nil, nil")
	}
}

func TestFetchExportFeedURLs_ReturnsMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/import/export_feed_urls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","result":{"uuid1":"https://feeds.example.com/pod1","uuid2":"https://feeds.example.com/pod2"}}`))
	})
	restore := newTestServer(t, mux)
	defer restore()

	client := &http.Client{}
	result, err := pocketcasts.FetchExportFeedURLs(context.Background(), client, []string{"uuid1", "uuid2"})
	if err != nil {
		t.Fatalf("FetchExportFeedURLs: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d entries, want 2", len(result))
	}
	if result["uuid1"] != "https://feeds.example.com/pod1" {
		t.Errorf("uuid1: got %q, want %q", result["uuid1"], "https://feeds.example.com/pod1")
	}
}

func TestFetchExportFeedURLs_RateLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/import/export_feed_urls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	restore := newTestServer(t, mux)
	defer restore()

	_, err := pocketcasts.FetchExportFeedURLs(context.Background(), &http.Client{}, []string{"uuid1"})
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
	var rl *httputil.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitError, got %T", err)
	}
	if rl.Wait != 15*time.Second {
		t.Errorf("Wait: got %v, want 15s", rl.Wait)
	}
}

func TestFetchExportFeedURLs_5xx_ReturnsTransientError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/import/export_feed_urls", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 503
	})
	restore := newTestServer(t, mux)
	defer restore()

	_, err := pocketcasts.FetchExportFeedURLs(context.Background(), &http.Client{}, []string{"uuid1"})
	if err == nil {
		t.Fatal("expected error for 503, got nil")
	}
	var te *httputil.TransientError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TransientError for 5xx, got %T: %v", err, err)
	}
}

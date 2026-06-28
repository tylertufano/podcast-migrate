package apple

// White-box tests for WebAPIWriter — getServerPosition and markPosition.
// Uses httptest servers pointed at via w.httpClient so no real Apple tokens
// are needed. The CatalogClient-dependent Write path is tested separately
// at the provider_test.go level via end-to-end stub scenarios.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serverPositionResponse builds the JSON body returned by GET /v1/me/playback/positions/…
func serverPositionResponse(completed bool, positionMs int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"data": []map[string]any{
			{
				"attributes": map[string]any{
					"completed":              completed,
					"positionInMilliseconds": positionMs,
				},
			},
		},
	})
	return b
}

// newWebAPIWriterWithClient returns a WebAPIWriter that routes all HTTP through client.
func newWebAPIWriterWithClient(client *http.Client) *WebAPIWriter {
	w := NewWebAPIWriter("bearer-token", "user-token")
	w.httpClient = client
	return w
}

// ---- getServerPosition ----

func TestGetServerPosition_Completed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(serverPositionResponse(true, 0))
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	pos, err := writer.getServerPosition(context.Background(), 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pos.recorded {
		t.Error("expected recorded=true")
	}
	if !pos.completed {
		t.Error("expected completed=true")
	}
	if pos.positionMs != 0 {
		t.Errorf("positionMs: got %d, want 0", pos.positionMs)
	}
}

func TestGetServerPosition_InProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(serverPositionResponse(false, 12345))
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	pos, err := writer.getServerPosition(context.Background(), 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pos.recorded {
		t.Error("expected recorded=true")
	}
	if pos.completed {
		t.Error("expected completed=false")
	}
	if pos.positionMs != 12345 {
		t.Errorf("positionMs: got %d, want 12345", pos.positionMs)
	}
}

func TestGetServerPosition_NotFound_ReturnsFalseRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	pos, err := writer.getServerPosition(context.Background(), 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos.recorded {
		t.Error("expected recorded=false for 404")
	}
	if pos.completed {
		t.Error("expected completed=false for 404")
	}
}

func TestGetServerPosition_EmptyData_ReturnsFalseRecorded(t *testing.T) {
	// Server returns 200 with an empty data array (no position on record).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	pos, err := writer.getServerPosition(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pos.recorded {
		t.Error("expected recorded=false for empty data array")
	}
}

func TestGetServerPosition_ServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	_, err := writer.getServerPosition(context.Background(), 5)
	if err == nil {
		t.Error("expected error for 5xx response, got nil")
	}
}

func TestGetServerPosition_MalformedJSON_ReturnsFalseRecorded(t *testing.T) {
	// A parse error is treated as unknown — no error returned, recorded=false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not valid json`))
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	pos, err := writer.getServerPosition(context.Background(), 3)
	if err != nil {
		t.Errorf("expected nil error for malformed JSON (caller proceeds with write), got: %v", err)
	}
	if pos.recorded {
		t.Error("expected recorded=false for malformed JSON")
	}
}

// ---- markPosition ----

func TestMarkPosition_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	if err := writer.markPosition(context.Background(), 42, 0, true); err != nil {
		t.Errorf("unexpected error for 200: %v", err)
	}
}

func TestMarkPosition_ClientError_ReturnsError(t *testing.T) {
	// 400 Bad Request — permanent error, not retried.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	err := writer.markPosition(context.Background(), 42, 5000, false)
	if err == nil {
		t.Error("expected error for 400 response, got nil")
	}
}

func TestMarkPosition_ContextCancelled_ReturnsError(t *testing.T) {
	// A cancelled context should cause the request to fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	err := writer.markPosition(ctx, 1, 0, true)
	if err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}

func TestMarkPosition_RequestBodyIncludesFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	writer := newWebAPIWriterWithClient(&http.Client{Transport: rewriteHostTransport(srv.URL)})
	if err := writer.markPosition(context.Background(), 42, 5000, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody == nil {
		t.Fatal("request body was empty or not JSON")
	}
	if gotBody["type"] != "playback-positions" {
		t.Errorf("type field = %v, want playback-positions", gotBody["type"])
	}
	attrs, _ := gotBody["attributes"].(map[string]any)
	if attrs == nil {
		t.Fatal("attributes field missing or not an object")
	}
	if completed, _ := attrs["completed"].(bool); !completed {
		t.Error("expected completed=true in request body")
	}
	posMs, _ := attrs["positionInMilliseconds"].(float64) // JSON numbers decode as float64
	if int64(posMs) != 5000 {
		t.Errorf("positionInMilliseconds = %v, want 5000", posMs)
	}
}

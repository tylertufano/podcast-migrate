package provider

import (
	"context"
	"io"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// Capabilities declares what a provider can read and write.
// Callers must check before invoking the corresponding methods.
type Capabilities struct {
	ReadSubscriptions  bool
	WriteSubscriptions bool
	ReadPlayState      bool
	WritePlayState     bool
}

// WriteOptions controls how a SetLibrary call behaves.
type WriteOptions struct {
	// DryRun logs what would happen without making any changes.
	DryRun bool

	// Restrict which data types are written.
	OnlySubscriptions bool
	OnlyPlayState     bool

	// ConflictStrategy determines the winner when both sides have data.
	ConflictStrategy ConflictStrategy

	// RequestDelay is the minimum pause between consecutive outbound API
	// requests during a write. Providers that call remote APIs (e.g. the
	// Overcast play-state writer) use this to stay within rate limits.
	// Zero means use the provider's built-in default.
	RequestDelay time.Duration

	// PodcastFilter, when non-empty, restricts play-state writes to episodes
	// from podcasts whose title contains at least one of the filter strings
	// (case-insensitive substring match). An empty slice means "all podcasts".
	PodcastFilter []string

	// LogWriter, when non-nil, receives one CSV line per episode describing
	// the outcome of the write attempt: updated, skipped (already satisfied),
	// or not_found (no match in the target database). In dry-run mode the
	// status is "would_update" instead of "updated".
	// The writer emits a header row followed by one data row per episode.
	// nil disables per-episode logging.
	LogWriter io.Writer

	// TitleMatchDateTolerance limits title-based episode matching (strategies 2
	// and 4 in the cascade) to episodes whose pub dates are within this window of
	// the source episode's pub date.  Exact pub-date matching (strategies 1 and 3)
	// is not affected.
	//
	// A value of 0 disables the guard and accepts any date combination — this
	// preserves backward-compatible behaviour for API callers that do not set
	// the field.  The CLI sets a sensible default (≈3 days) to prevent false
	// title matches between episodes published years apart.
	//
	// If either side has no pub date the check is skipped and the title match
	// is accepted regardless of the tolerance value.
	TitleMatchDateTolerance time.Duration
}

// ConflictStrategy selects which side wins when both provider and library have state.
type ConflictStrategy int

const (
	// FurthestWins takes whichever play position is further along.
	// For played vs. in-progress, played wins.
	FurthestWins ConflictStrategy = iota

	// SourceWins always uses the library being imported.
	SourceWins

	// TargetWins preserves whatever the target provider already has.
	TargetWins
)

// Provider is the interface every podcast app adapter must satisfy.
type Provider interface {
	// Name returns a short human-readable name, e.g. "Apple Podcasts".
	Name() string

	// Capabilities reports what this provider supports.
	Capabilities() Capabilities

	// GetLibrary reads the current state from the provider.
	// Implementations should only populate fields covered by their Capabilities.
	GetLibrary(ctx context.Context) (*model.Library, error)

	// SetLibrary writes the library to the provider, respecting opts.
	// Implementations must return ErrCapabilityUnsupported for operations
	// outside their Capabilities.
	SetLibrary(ctx context.Context, lib *model.Library, opts WriteOptions) error
}

// ErrCapabilityUnsupported is returned when a caller invokes an operation the
// provider does not support (e.g. writing play state to an OPML-only adapter).
type ErrCapabilityUnsupported struct {
	Provider  string
	Operation string
}

func (e *ErrCapabilityUnsupported) Error() string {
	return e.Provider + ": " + e.Operation + " is not supported"
}

package overcast

// log.go contains per-episode CSV log helpers used by the play-state writer.
// Implementations delegate to internal/migrate so the log format stays in sync
// with other providers (Pocket Casts).

import (
	"io"
	"time"

	"github.com/tyler/podcast-migrate/internal/migrate"
	"github.com/tyler/podcast-migrate/internal/model"
)

// writeLogHeader emits the CSV header row to w. No-op when w is nil.
func writeLogHeader(w io.Writer) { migrate.WriteLogHeader(w) }

// writeLogLine emits one CSV data row to w. No-op when w is nil.
func writeLogLine(w io.Writer, status, podcast, episode string, pubDate time.Time, srcState, tgtState, note string) {
	migrate.WriteLogLine(w, status, podcast, episode, pubDate, srcState, tgtState, note)
}

// playStateLabel returns a short human-readable label for a play state.
func playStateLabel(ps model.PlayState, pos time.Duration) string {
	return migrate.PlayStateLabel(ps, pos)
}

// csvField delegates to migrate.CSVField. Kept as a package-local alias so
// the white-box tests in log_test.go (package overcast) can call it directly.
func csvField(s string) string { return migrate.CSVField(s) }

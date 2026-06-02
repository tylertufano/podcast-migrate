package overcast

// log.go contains per-episode CSV log helpers used by the play-state writer.
// The format mirrors the apple package's log output so that --log-file produces
// the same columns regardless of which direction the sync runs.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// writeLogHeader emits the CSV header row to w. No-op when w is nil.
func writeLogHeader(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, "status,podcast,episode,pub_date,source_state,target_state,note")
}

// writeLogLine emits one CSV data row to w. No-op when w is nil.
func writeLogLine(w io.Writer, status, podcast, episode string, pubDate time.Time, srcState, tgtState, note string) {
	if w == nil {
		return
	}
	dateStr := ""
	if !pubDate.IsZero() {
		dateStr = pubDate.UTC().Format("2006-01-02")
	}
	fmt.Fprintf(w, "%s,%s,%s,%s,%s,%s,%s\n",
		csvField(status), csvField(podcast), csvField(episode),
		dateStr,
		csvField(srcState), csvField(tgtState), csvField(note))
}

// playStateLabel returns a short human-readable label for a play state, e.g.
// "played", "in_progress(1h2m3s)", or "unplayed".
func playStateLabel(ps model.PlayState, pos time.Duration) string {
	switch ps {
	case model.PlayStatePlayed:
		return "played"
	case model.PlayStateInProgress:
		if pos > 0 {
			return "in_progress(" + pos.Round(time.Second).String() + ")"
		}
		return "in_progress"
	default:
		return "unplayed"
	}
}

// csvField wraps s in double-quotes and escapes internal quotes when s contains
// characters that would break CSV parsing (comma, quote, newline).
func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

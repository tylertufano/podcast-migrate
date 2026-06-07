package migrate

// log.go contains CSV log helpers shared by all write-side providers.
// Both the Overcast and Pocket Casts providers emit the same seven-column CSV
// format so that --log-file output is consistent regardless of sync direction.
//
// Column layout: status, podcast, episode, pub_date, source_state, target_state, note

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tyler/podcast-migrate/internal/model"
)

// WriteLogHeader emits the CSV header row to w. No-op when w is nil.
func WriteLogHeader(w io.Writer) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, "status,podcast,episode,pub_date,source_state,target_state,note")
}

// WriteLogLine emits one CSV data row to w. No-op when w is nil.
func WriteLogLine(w io.Writer, status, podcast, episode string, pubDate time.Time, srcState, tgtState, note string) {
	if w == nil {
		return
	}
	dateStr := ""
	if !pubDate.IsZero() {
		dateStr = pubDate.UTC().Format("2006-01-02")
	}
	fmt.Fprintf(w, "%s,%s,%s,%s,%s,%s,%s\n",
		CSVField(status), CSVField(podcast), CSVField(episode),
		dateStr,
		CSVField(srcState), CSVField(tgtState), CSVField(note))
}

// PlayStateLabel returns a short human-readable label for a play state, e.g.
// "played", "in_progress(1h2m3s)", or "unplayed".
func PlayStateLabel(ps model.PlayState, pos time.Duration) string {
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

// CSVField wraps s in double-quotes and escapes internal quotes when s contains
// characters that would break CSV parsing (comma, quote, newline).
func CSVField(s string) string {
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

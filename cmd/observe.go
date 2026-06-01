package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

func observeCmd() *cobra.Command {
	var (
		dbPath        string
		podcastFilter string
		episodeFilter string
		intervalMs    int
	)

	cmd := &cobra.Command{
		Use:   "observe",
		Short: "Watch MTLibrary.sqlite in real time and print every play-state change",
		Long: `observe polls the Apple Podcasts SQLite database every N ms and prints a
structured diff whenever anything changes:

  • ZMTEPISODE column changes (ZPLAYSTATE, ZPLAYSTATESOURCE, ZPLAYHEAD, dates, …)
  • New ACHANGE rows (CoreData persistent history)
  • New ATRANSACTION rows (commit attribution)
  • playState:<feedURL> preference key value

Run this WHILE Apple Podcasts is open. Mark an episode as played in the UI and
watch the exact sequence of writes Apple makes. This is the ground-truth
reference for what our SQLite writer should reproduce.`,
		Example: `  # Watch all episodes while you mark one as played
  podcast-migrate observe

  # Narrow to a specific podcast
  podcast-migrate observe --podcast "Dreamtown"

  # Narrow to a specific episode
  podcast-migrate observe --podcast "Dreamtown" --episode "Chapter 8"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbPath == "" {
				home, _ := os.UserHomeDir()
				dbPath = filepath.Join(home,
					"Library/Group Containers/243LU875E5.groups.com.apple.podcasts/Documents/MTLibrary.sqlite")
			}
			prefPath := podcastsPrefPath2()
			interval := time.Duration(intervalMs) * time.Millisecond

			fmt.Printf("observe: watching %s\n", dbPath)
			fmt.Printf("observe: polling every %v  (Ctrl+C to stop)\n\n", interval)

			db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_journal=wal")
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer db.Close()

			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return runObserveLoop(ctx, db, prefPath, podcastFilter, episodeFilter, interval)
		},
	}

	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home,
		"Library/Group Containers/243LU875E5.groups.com.apple.podcasts/Documents/MTLibrary.sqlite")

	cmd.Flags().StringVar(&dbPath, "sqlite", defaultDB, "Path to MTLibrary.sqlite")
	cmd.Flags().StringVar(&podcastFilter, "podcast", "", "Case-insensitive substring filter for podcast title")
	cmd.Flags().StringVar(&episodeFilter, "episode", "", "Case-insensitive substring filter for episode title")
	cmd.Flags().IntVar(&intervalMs, "interval", 200, "Poll interval in milliseconds")
	return cmd
}

// ---------------------------------------------------------------------------
// Snapshot types
// ---------------------------------------------------------------------------

type epSnap struct {
	pk                    int64
	podcastTitle          string
	feedURL               string
	title                 string
	playState             int64
	playStateSource       int64
	playStateManuallyset  int64
	playHead              float64
	playCount             int64
	lastDatePlayed        sql.NullFloat64
	lastUserMarked        sql.NullFloat64
	playStateLastModified sql.NullFloat64
	zopt                  int64
}

type achangeSnap struct {
	pk            int64
	changeType    int64
	entity        int64
	entityPK      int64
	transactionID int64
	columns       sql.NullString
}

type atxSnap struct {
	pk            int64
	bundleIDTS    sql.NullInt64
	contextNameTS sql.NullInt64
	processIDTS   sql.NullInt64
	authorTS      sql.NullInt64
	timestamp     float64
}

// ---------------------------------------------------------------------------
// Main loop
// ---------------------------------------------------------------------------

func runObserveLoop(ctx context.Context, db *sql.DB, prefPath, podFilter, epFilter string, interval time.Duration) error {
	// Decode ATRANSACTIONSTRING once at start (and re-read on change).
	tsMap, _ := readTransactionStrings(ctx, db)

	// Take initial snapshots.
	prevEps, err := queryEpisodes(ctx, db, podFilter, epFilter)
	if err != nil {
		return err
	}
	prevMaxChange, _ := queryMaxPK(ctx, db, "ACHANGE")
	prevMaxTx, _ := queryMaxPK(ctx, db, "ATRANSACTION")
	prevPrefKeys := readAllPlayStateKeys(prefPath)

	// Print initial state summary.
	fmt.Printf("=== Initial state: %d episode(s) in scope ===\n", len(prevEps))
	for _, ep := range prevEps {
		fmt.Printf("  [%d] %q  (ZPLAYSTATE=%d  ZPLAYSTATESOURCE=%d  ZPLAYHEAD=%.1f  ZPLAYCOUNT=%d  Z_OPT=%d)\n",
			ep.pk, ep.title, ep.playState, ep.playStateSource, ep.playHead, ep.playCount, ep.zopt)
	}
	if len(prevPrefKeys) > 0 {
		fmt.Println("\n  Preference keys:")
		for k, v := range prevPrefKeys {
			fmt.Printf("    %s = %s\n", k, v)
		}
	}
	fmt.Println("\nWaiting for changes…")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nobserve: done.")
			return nil
		case <-ticker.C:
		}

		// Refresh transaction-string map in case new strings were inserted.
		if newMap, err2 := readTransactionStrings(ctx, db); err2 == nil && len(newMap) > len(tsMap) {
			tsMap = newMap
		}

		// --- Check episodes ---
		curEps, err := queryEpisodes(ctx, db, podFilter, epFilter)
		if err == nil {
			prevMap := make(map[int64]epSnap, len(prevEps))
			for _, e := range prevEps {
				prevMap[e.pk] = e
			}
			for _, cur := range curEps {
				prev, known := prevMap[cur.pk]
				if !known {
					// New episode row (unusual but report it).
					printTimestamp()
					fmt.Printf("  NEW EPISODE row: pk=%d %q\n", cur.pk, cur.title)
					continue
				}
				if cur.zopt != prev.zopt {
					printTimestamp()
					fmt.Printf("  EPISODE CHANGED  pk=%d %q\n", cur.pk, cur.title)
					diffEpisode(prev, cur)
				}
			}
			prevEps = curEps
		}

		// --- Check ACHANGE ---
		curMaxChange, _ := queryMaxPK(ctx, db, "ACHANGE")
		if curMaxChange > prevMaxChange {
			printTimestamp()
			newRows, _ := queryAchange(ctx, db, prevMaxChange)
			for _, row := range newRows {
				fmt.Printf("  ACHANGE row inserted: pk=%d  changeType=%d  entity=%d  entityPK=%d  transactionID=%d  columns=%s\n",
					row.pk, row.changeType, row.entity, row.entityPK, row.transactionID,
					nullStr(row.columns))
			}
			prevMaxChange = curMaxChange
		}

		// --- Check ATRANSACTION ---
		curMaxTx, _ := queryMaxPK(ctx, db, "ATRANSACTION")
		if curMaxTx > prevMaxTx {
			printTimestamp()
			newTxs, _ := queryAtransaction(ctx, db, prevMaxTx)
			for _, tx := range newTxs {
				fmt.Printf("  ATRANSACTION row inserted: pk=%d  bundleID=%s  contextName=%s  processID=%s  author=%s  timestamp=%.0f (%s)\n",
					tx.pk,
					tsLookup(tsMap, tx.bundleIDTS),
					tsLookup(tsMap, tx.contextNameTS),
					tsLookup(tsMap, tx.processIDTS),
					tsLookup(tsMap, tx.authorTS),
					tx.timestamp,
					fromCoreData(tx.timestamp).Format("2006-01-02 15:04:05"),
				)
			}
			prevMaxTx = curMaxTx
		}

		// --- Check preference keys ---
		curPrefKeys := readAllPlayStateKeys(prefPath)
		for k, v := range curPrefKeys {
			prev, ok := prevPrefKeys[k]
			if !ok {
				printTimestamp()
				fmt.Printf("  PREF KEY created:  %s = %s\n", k, v)
			} else if v != prev {
				printTimestamp()
				fmt.Printf("  PREF KEY changed:  %s  %s → %s\n", k, prev, v)
			}
		}
		for k, prev := range prevPrefKeys {
			if _, ok := curPrefKeys[k]; !ok {
				printTimestamp()
				fmt.Printf("  PREF KEY deleted:  %s (was %s)\n", k, prev)
			}
		}
		prevPrefKeys = curPrefKeys
	}
}

// ---------------------------------------------------------------------------
// Diff printer
// ---------------------------------------------------------------------------

func diffEpisode(prev, cur epSnap) {
	type field struct {
		name    string
		prevStr string
		curStr  string
	}
	fields := []field{
		{"ZPLAYSTATE", fmt.Sprint(prev.playState), fmt.Sprint(cur.playState)},
		{"ZPLAYSTATESOURCE", fmt.Sprint(prev.playStateSource), fmt.Sprint(cur.playStateSource)},
		{"ZPLAYSTATEMANUALLYSET", fmt.Sprint(prev.playStateManuallyset), fmt.Sprint(cur.playStateManuallyset)},
		{"ZPLAYHEAD", fmt.Sprintf("%.1f", prev.playHead), fmt.Sprintf("%.1f", cur.playHead)},
		{"ZPLAYCOUNT", fmt.Sprint(prev.playCount), fmt.Sprint(cur.playCount)},
		{"ZLASTDATEPLAYED", cdateStr(prev.lastDatePlayed), cdateStr(cur.lastDatePlayed)},
		{"ZLASTUSERMARKEDASPLAYEDDATE", cdateStr(prev.lastUserMarked), cdateStr(cur.lastUserMarked)},
		{"ZPLAYSTATELASTMODIFIEDDATE", cdateStr(prev.playStateLastModified), cdateStr(cur.playStateLastModified)},
		{"Z_OPT", fmt.Sprint(prev.zopt), fmt.Sprint(cur.zopt)},
	}
	for _, f := range fields {
		if f.prevStr != f.curStr {
			fmt.Printf("    %-34s %s → %s\n", f.name, f.prevStr, f.curStr)
		}
	}
}

// ---------------------------------------------------------------------------
// Query helpers
// ---------------------------------------------------------------------------

func queryEpisodes(ctx context.Context, db *sql.DB, podFilter, epFilter string) ([]epSnap, error) {
	q := `
		SELECT
			e.Z_PK, p.ZTITLE, p.ZFEEDURL, e.ZTITLE,
			COALESCE(e.ZPLAYSTATE, 0),
			COALESCE(e.ZPLAYSTATESOURCE, 0),
			COALESCE(e.ZPLAYSTATEMANUALLYSET, 0),
			COALESCE(e.ZPLAYHEAD, 0.0),
			COALESCE(e.ZPLAYCOUNT, 0),
			e.ZLASTDATEPLAYED,
			e.ZLASTUSERMARKEDASPLAYEDDATE,
			e.ZPLAYSTATELASTMODIFIEDDATE,
			COALESCE(e.Z_OPT, 0)
		FROM ZMTEPISODE e
		JOIN ZMTPODCAST p ON e.ZPODCAST = p.Z_PK
		WHERE p.ZSUBSCRIBED = 1
		  AND p.ZFEEDURL LIKE 'http%'`

	var conditions []string
	var args []interface{}
	if podFilter != "" {
		conditions = append(conditions, "LOWER(p.ZTITLE) LIKE ?")
		args = append(args, "%"+strings.ToLower(podFilter)+"%")
	}
	if epFilter != "" {
		conditions = append(conditions, "LOWER(e.ZTITLE) LIKE ?")
		args = append(args, "%"+strings.ToLower(epFilter)+"%")
	}
	for _, c := range conditions {
		q += " AND " + c
	}
	q += " ORDER BY e.Z_PK"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []epSnap
	for rows.Next() {
		var s epSnap
		if err := rows.Scan(
			&s.pk, &s.podcastTitle, &s.feedURL, &s.title,
			&s.playState, &s.playStateSource, &s.playStateManuallyset,
			&s.playHead, &s.playCount,
			&s.lastDatePlayed, &s.lastUserMarked, &s.playStateLastModified,
			&s.zopt,
		); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

func queryMaxPK(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var max int64
	err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(Z_PK),0) FROM "+table).Scan(&max)
	return max, err
}

func queryAchange(ctx context.Context, db *sql.DB, afterPK int64) ([]achangeSnap, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT Z_PK, COALESCE(ZCHANGETYPE,0), COALESCE(ZENTITY,0), COALESCE(ZENTITYPK,0),
		        COALESCE(ZTRANSACTIONID,0), ZCOLUMNS
		 FROM ACHANGE WHERE Z_PK > ? ORDER BY Z_PK`, afterPK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snaps []achangeSnap
	for rows.Next() {
		var s achangeSnap
		if err := rows.Scan(&s.pk, &s.changeType, &s.entity, &s.entityPK, &s.transactionID, &s.columns); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

func queryAtransaction(ctx context.Context, db *sql.DB, afterPK int64) ([]atxSnap, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT Z_PK, ZBUNDLEIDTS, ZCONTEXTNAMETS, ZPROCESSIDTS, ZAUTHORTS,
		        COALESCE(ZTIMESTAMP,0)
		 FROM ATRANSACTION WHERE Z_PK > ? ORDER BY Z_PK`, afterPK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snaps []atxSnap
	for rows.Next() {
		var s atxSnap
		if err := rows.Scan(&s.pk, &s.bundleIDTS, &s.contextNameTS, &s.processIDTS, &s.authorTS, &s.timestamp); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

func readTransactionStrings(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT Z_PK, ZNAME FROM ATRANSACTIONSTRING`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]string)
	for rows.Next() {
		var pk int64
		var name string
		if err := rows.Scan(&pk, &name); err != nil {
			continue
		}
		m[pk] = name
	}
	return m, rows.Err()
}

// ---------------------------------------------------------------------------
// Preference key helpers
// ---------------------------------------------------------------------------

// readAllPlayStateKeys returns a map of playState:<feedURL> → revision string
// for all such keys in the Apple Podcasts preference plist.
func readAllPlayStateKeys(prefPath string) map[string]string {
	out, err := exec.Command("defaults", "read", prefPath).Output()
	if err != nil {
		return nil
	}
	m := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"playState:`) && !strings.HasPrefix(line, "playState:") {
			continue
		}
		// Lines look like:   "playState:https://..." = 70;
		parts := strings.SplitN(line, " = ", 2)
		if len(parts) != 2 {
			continue
		}
		k := strings.Trim(parts[0], `"`)
		v := strings.TrimRight(strings.TrimSpace(parts[1]), ";")
		m[k] = v
	}
	return m
}

// podcastsPrefPath2 returns the Apple Podcasts preference plist path.
func podcastsPrefPath2() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home,
		"Library/Containers/com.apple.podcasts/Data/Library/Preferences/com.apple.podcasts.plist")
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

var coreDataEpochObs = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func fromCoreData(secs float64) time.Time {
	return coreDataEpochObs.Add(time.Duration(secs * float64(time.Second)))
}

func cdateStr(v sql.NullFloat64) string {
	if !v.Valid {
		return "NULL"
	}
	t := fromCoreData(v.Float64)
	return fmt.Sprintf("%.0f (%s)", v.Float64, t.Local().Format("15:04:05"))
}

func nullStr(v sql.NullString) string {
	if !v.Valid {
		return "NULL"
	}
	return v.String
}

func tsLookup(m map[int64]string, id sql.NullInt64) string {
	if !id.Valid {
		return "NULL"
	}
	if s, ok := m[id.Int64]; ok {
		return fmt.Sprintf("%q (id=%d)", s, id.Int64)
	}
	return fmt.Sprintf("id=%d", id.Int64)
}

func printTimestamp() {
	fmt.Printf("\n[%s]\n", time.Now().Format("15:04:05.000"))
}

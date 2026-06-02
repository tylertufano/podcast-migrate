# podcast-migrate

A command-line tool for migrating podcast subscriptions and episode play state between podcast applications. Supports Apple Podcasts ↔ Overcast in both directions.

## How it works

podcast-migrate reads your library directly from the source app's local data, merges it with whatever the destination already knows, and writes the result. Each podcast service is an interchangeable adapter behind a common `Provider` interface, so adding new services doesn't require changes to the core migration logic.

## Current status

### What's working

**Apple Podcasts (source and destination)**

- Reads subscriptions and episode play state directly from `MTLibrary.sqlite` — no export step needed
- Falls back to a manually exported OPML file if the database isn't accessible
- **Play state write** via the Apple Podcasts web API (`amp-api.podcasts.apple.com`) — syncs both fully played and in-progress episodes to Apple's backend, which propagates automatically to iPhone, iPad, Mac, and the web player. Episode IDs are resolved through the Apple catalog API (iTunes Search + amp-api catalog with full pagination), so no local database or Full Disk Access is required for the write path. Before each write the server's current position is checked and the episode is skipped if Apple is already at or ahead of the source. See [Overcast → Apple Podcasts](#overcast--apple-podcasts-sync-play-state-to-iphone) for setup.
- Detects and reports two categories of content that can't be migrated:
  - **`internal://` feeds** — Apple-exclusive shows with no public RSS feed
  - **PSUB / PLUS episodes** — paywalled Apple Podcasts Subscriptions episodes; the parent podcast subscription is still migrated

**Overcast (source and destination)**

- Generates an OPML file ready to import via Overcast › Settings › Import OPML
- Reads an Overcast OPML export for inspection or two-way merging
- **Play state write** via the unofficial Overcast web API. See [Apple Podcasts → Overcast](#apple-podcasts--overcast-sync-play-state) for details.

**Sync engine**

- Three conflict resolution strategies when both sides have state for the same episode:
  - `furthest` *(default)* — whichever side is further along wins; fully-played always beats in-progress
  - `source` — source data always wins
  - `target` — existing destination data is never overwritten
- Episode matching across providers uses a four-strategy cascade: feed URL + pub date → feed URL + title → podcast title + pub date → podcast title + title (the last two handle feeds that differ between apps)
- `--dry-run` previews what would change without writing anything

### Supported providers

| Provider | Read subscriptions | Read play state | Write subscriptions | Write play state |
|---|:---:|:---:|:---:|:---:|
| Apple Podcasts | ✅ | ✅ | — | ✅ (web API → syncs to all devices) |
| Overcast | ✅ | ✅ | ✅ (OPML) | ✅ (unofficial web API) |

## Installation

**Prerequisites:** Go 1.21+

```sh
git clone https://github.com/tylertufano/podcast-migrate
cd podcast-migrate
go build -o podcast-migrate .
```

Or install directly:

```sh
go install github.com/tylertufano/podcast-migrate@latest
```

### macOS permissions

Reading the Apple Podcasts database requires **Full Disk Access** for your terminal app on macOS Ventura and later. Grant it in System Settings › Privacy & Security › Full Disk Access.

If you'd rather not grant Full Disk Access, export your subscriptions manually via Apple Podcasts › File › Export Subscriptions, then pass the file with `--opml-fallback`. This path carries subscriptions only — play state requires the SQLite database.

## Usage

### Apple Podcasts → Overcast (subscriptions)

```sh
# Preview what will be migrated (no files written)
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml \
  --dry-run

# Generate the import file
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml
```

Then in Overcast: **Settings › Import OPML** and select the generated file.

### Apple Podcasts → Overcast (sync play state)

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml \
  --play-state
```

**How it works:** Authenticates with your Overcast account, automatically fetches your current library from `overcast.fm/account/export_opml/extended`, and calls the same internal API endpoint the Overcast web player uses to save episode positions. For each played episode, it fetches the episode's Overcast page to resolve its internal numeric ID, then POSTs the played position.

No manual OPML export required — the tool fetches your live account state after login.

If you prefer to match against a specific snapshot instead of auto-fetching the live account (e.g. for reproducible dry-run previews), provide one explicitly:

```sh
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml \
  --overcast-match-opml ~/Downloads/overcast.opml \
  --play-state
```

> **Disclaimer:** Uses an undocumented Overcast endpoint that Marco Arment has not publicly supported. It works as of the implementation date but may break without notice. Use `--dry-run` to preview before committing.

### Overcast → Apple Podcasts (sync play state to iPhone)

This direction writes play state via the Apple Podcasts web API, which syncs to **all your Apple devices** (iPhone, iPad, Mac, and the web at podcasts.apple.com) automatically — no need to open the app, no iCloud delay.

Episode IDs are resolved through the Apple catalog API (iTunes Search API to find the podcast, then amp-api catalog with full pagination to index all episodes). **No local Apple Podcasts database is needed** — this works without Full Disk Access and on machines where Apple Podcasts has never been opened.

#### Step 1 — Get your tokens (one-time setup)

1. Open [podcasts.apple.com](https://podcasts.apple.com) in your browser and sign in
2. Open DevTools (⌥⌘I in Safari, F12 in Chrome) → Network tab
3. Mark any episode as played in the web UI
4. Find the `PUT` request to `amp-api.podcasts.apple.com/v1/me/playback/positions/...`
5. Copy two header values from that request:
   - `Authorization` — everything after `Bearer ` (a long JWT string)
   - `media-user-token` — the full value of this header

#### Step 2 — Run the migration

```sh
# Set tokens as env vars (avoids them appearing in shell history)
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."

# Export your Overcast library from overcast.fm/account/export_opml/extended first, then:
# Dry-run first to preview what will be marked
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state
```

Or pass the tokens directly as flags:

```sh
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state \
  --apple-bearer-token "eyJhbGci..." \
  --apple-media-user-token "0.Apgf..."
```

**Scope:** Only episodes in the Apple Podcasts catalog (public RSS feeds indexed by Apple) can be marked via this API. Private or unlisted feeds without an Apple catalog entry are skipped and reported.

**Episode coverage:** The Apple catalog API is paginated — all episodes for a podcast are indexed (not just the most recent), so old played episodes are handled correctly regardless of how far back your history goes.

**Token lifetimes:** The Bearer token is a short-lived JWT signed by Apple — capture a fresh one if you get `401` errors. The `media-user-token` is your account session and lasts longer but will eventually expire. Both are re-captured the same way (one network request in DevTools).

**Rate limiting:** The tool sends one API request per episode with a 500 ms delay between calls by default. Override with `--request-delay` (e.g. `--request-delay 1s`) if you hit rate limits.

#### Limit to specific podcasts

```sh
# Single podcast (case-insensitive substring match)
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --podcast "rogan"

# Multiple podcasts
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --podcast "rogan" --podcast "sistersinlaw"

# From a file (one podcast title/word per line)
podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --play-state --podcast-list ~/my-podcasts.txt
```

### Export your library to JSON

Snapshots your library as a portable JSON file. Useful for inspection, backup, or staging a migration.

```sh
# Print to stdout
podcast-migrate export --from podcasts

# Save to file
podcast-migrate export --from podcasts --out ~/Desktop/my-library.json

# Export from Overcast
podcast-migrate export --from overcast \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --out ~/Desktop/overcast-library.json
```

### Import from a JSON snapshot

```sh
podcast-migrate import --to overcast \
  --in ~/Desktop/my-library.json \
  --overcast-out ~/Desktop/overcast-import.opml
```

### Common flags

| Flag | Description |
|---|---|
| `--dry-run` | Preview changes without writing anything |
| `--only-subscriptions` | Migrate subscriptions, skip episode play state |
| `--conflict` | Conflict resolution: `furthest` (default), `source`, `target` |
| `--sqlite` | Custom path to `MTLibrary.sqlite` (auto-detected by default) |
| `--opml-fallback` | Apple Podcasts OPML export to use if SQLite is inaccessible |
| `--play-state` | Write episode play state |
| `--podcast` | Limit play-state sync to podcasts matching this word/phrase (repeatable) |
| `--podcast-list` | Path to a file with one podcast title/word per line |
| `--request-delay` | Delay between API requests (default 500ms; increase if you hit rate limits) |
| `--log-file` | Write per-episode CSV detail log (columns: status, podcast, episode, pub_date, source_state, target_state, note) |
| `--overcast-source-opml` | Path to Overcast extended OPML export used as the migration source (`--from overcast`) |
| `--overcast-match-opml` | Path to Overcast OPML used for destination episode matching when writing play state (optional; if omitted and credentials are set, the live account library is fetched automatically) |
| `--overcast-email` | Overcast account email (or `OVERCAST_EMAIL` env var) |
| `--overcast-password` | Overcast account password (or `OVERCAST_PASSWORD` env var) |
| `--apple-bearer-token` | Apple web API Bearer token (or `APPLE_BEARER_TOKEN` env var) |
| `--apple-media-user-token` | Apple media-user-token (or `APPLE_MEDIA_USER_TOKEN` env var) |
| `--strict-feed-match` | Only match episodes using feed-URL-anchored strategies; skips cross-feed title fallbacks |
| `--force-update` | Write source play state even if the destination already shows the episode as played or further along |

## Future work

### Additional providers
The `Provider` interface makes adding new services straightforward. Planned:
- **Pocket Casts** — has a documented sync API
- **Castro**
- **RSS readers / generic OPML** — subscription-only, no play state

### Automated / scheduled sync
A `sync` subcommand that runs on a schedule (cron or a background agent) and incrementally syncs only changes since the last run, using a state file to track what was last seen.

### Richer episode matching
The current cascade can fail when the same episode has different titles or pub dates across providers (common with older feeds that changed hosting). A fuzzy-match fallback using edit distance on titles would reduce unmatched episodes.

### Token management
Automatic extraction of the Apple Bearer token from the macOS Keychain (where the native Podcasts app caches it), and automatic renewal when it expires, to avoid the manual DevTools capture step. The `media-user-token` is harder to extract automatically since it lives in a browser cookie rather than the system Keychain.

### Packaged release
Signed macOS binary via GitHub Actions, distributed through Homebrew.

## Project structure

```
cmd/                  CLI entry points (migrate, export, import, mark-played, observe)
internal/
  model/              Shared types: Library, Podcast, EpisodeState
  provider/           Provider interface and WriteOptions
  apple/              Apple Podcasts adapter (SQLite read; catalog API + web API write)
  overcast/           Overcast adapter (OPML read/write + web API)
  sync/               Merge engine and conflict resolution
main.go
```

## Tests

```sh
go test ./...
```

Tests: `apple` ~90%, `overcast` 95%, `sync` 99%, `model` 100%.

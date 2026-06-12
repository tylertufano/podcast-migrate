# podcast-migrate

A command-line tool for migrating podcast subscriptions and episode play state between podcast applications. Supports Apple Podcasts, Overcast, and Pocket Casts.

## How it works

podcast-migrate reads your library directly from the source app's local data, merges it with whatever the destination already knows, and writes the result. Each podcast service is an interchangeable adapter behind a common `Provider` interface, so adding new services doesn't require changes to the core migration logic.

## Current status

### What's working

**Apple Podcasts (source and destination)**

- Reads subscriptions and episode play state directly from `MTLibrary.sqlite` — no export step needed
- Falls back to a manually exported OPML file if the database isn't accessible
- **Play state write** via the Apple Podcasts web API (`amp-api.podcasts.apple.com`) — syncs both fully played and in-progress episodes to Apple's backend, which propagates automatically to iPhone, iPad, Mac, and the web player. Episode IDs are resolved through the Apple catalog API (iTunes Search + amp-api catalog with full pagination), so no local database or Full Disk Access is required for the write path. Before each write the server's current position is checked and the episode is skipped if Apple is already at or ahead of the source. See [Overcast → Apple Podcasts](#overcast--apple-podcasts-sync-play-state-to-iphone) for setup.
- Detects and reports two categories of content that can't be migrated:
  - **`internal://` feeds** — Apple-exclusive shows with no public RSS feed (skipped)
  - **PSUB / PLUS episodes** — paywalled Apple Podcasts Subscriptions; episodes are matched against the destination by podcast title + pub date, so if you have the equivalent feed subscribed on the destination they will be picked up automatically

**Overcast (source and destination)**

- Generates an OPML file ready to import via Overcast › Settings › Import OPML
- Reads an Overcast OPML export for inspection or two-way merging
- **Play state write** via the unofficial Overcast web API — and automatically subscribes to any source podcast not yet in your Overcast library before writing its episodes (Overcast silently drops play-state updates for unsubscribed podcasts). Use `--subscribed-only` to skip the subscribe step and only write state for feeds already in your account. See [Apple Podcasts → Overcast](#apple-podcasts--overcast-sync-play-state) for details.

**Pocket Casts (source and destination)**

- Reads subscriptions and **complete play history** via the Pocket Casts protobuf sync endpoint (`/user/sync/update`) — the same endpoint the mobile apps use. Unlike the REST history endpoint (capped at ~100 entries), this returns every episode Pocket Casts has ever recorded play state for.
- Subscriber and private feed URLs (Slate Plus, NPR Plus, and similar) are resolved in a single batch call to the PC export service (`/import/export_feed_urls`) — the same endpoint the iOS app uses for its built-in OPML export. This recovers the exact subscriber URL the account was subscribed with, so private feeds are correctly included in exports.
- **Play state write** via the same unofficial web API the Pocket Casts web player uses — propagates to iPhone, Android, and all devices through Pocket Casts' own sync. Also automatically subscribes to any source podcast not yet in your Pocket Casts library before writing its episodes, so a full cross-app migration works in a single run. Use `--subscribed-only` to skip the subscribe step.
- Two-phase episode matching: Phase A indexes in-progress and recently-played episodes (fast); Phase B fetches per-podcast episode pages for any episodes not found in Phase A, handling episodes you've never started in Pocket Casts

**Sync engine**

- Three conflict resolution strategies when both sides have state for the same episode:
  - `furthest` *(default)* — whichever side is further along wins; fully-played always beats in-progress
  - `source` — source data always wins
  - `target` — existing destination data is never overwritten
- Episode matching across providers uses a four-strategy cascade: feed URL + pub date → feed URL + title → podcast title + pub date → podcast title + title (the last two handle feeds that differ between apps)
- **Fuzzy title matching** — season markers (`S01`, `S1`, `Season 1`, …) and punctuation are stripped before comparing episode titles, so "The Retrievals - Ep. 4" and "The Retrievals S01 - Ep. 4" are recognised as the same episode. Applied in all matching paths (Overcast, Pocket Casts, cross-feed).
- **Automatic subscriber feed remapping** — before merging, each source podcast that isn't directly subscribed at the destination is matched against the destination's subscription list by fuzzy-normalised title (Plus/tier-suffix stripping + punctuation normalization). If a match is found, the source feed URL is silently remapped to the destination's feed URL so all matching strategies work correctly. This handles Apple `internal://` and PSUB subscriber feeds without any flags — subscribe to the analog feed on the destination first and the migration handles the rest.
- `--dry-run` previews what would change without writing anything

### Supported providers

| Provider | Read subscriptions | Read play state | Write subscriptions | Write play state |
|---|:---:|:---:|:---:|:---:|
| Apple Podcasts | ✅ | ✅ | — | ✅ (web API → syncs to all devices) |
| Overcast | ✅ | ✅ | ✅ (OPML + auto on play-state write¹) | ✅ (unofficial web API) |
| Pocket Casts | ✅ | ✅ complete history | ✅ (auto on play-state write¹) | ✅ (unofficial web API) |
| OPML | ✅ | ✅ (extended format) | ✅ | ✅ (extended format) |

¹ Subscriptions are written automatically during a play-state write unless `--subscribed-only` is set.

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

# Play state only — no OPML file needed
podcast-migrate migrate --from podcasts --to overcast \
  --play-state

# Play state + generate a subscription import file at the same time
podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/overcast-import.opml \
  --play-state
```

**How it works:** Authenticates with your Overcast account, automatically fetches your current library from `overcast.fm/account/export_opml/extended`, and calls the same internal API endpoint the Overcast web player uses to save episode positions. For each played episode, it fetches the episode's Overcast page to resolve its internal numeric ID, then POSTs the played position.

**Subscription handling:** Any podcast in your Apple Podcasts library that is not yet subscribed in Overcast is automatically subscribed before its episodes are updated. Overcast silently drops play-state updates for unsubscribed podcasts, so this step is required for a complete migration. To skip it and only update episodes for podcasts you're already subscribed to in Overcast, add `--subscribed-only`.

`--overcast-out` is optional — omit it to sync play state without generating an OPML file.

**Subscriber / private feeds:** If you have Apple Podcasts Subscriptions (PSUB) or other subscriber-feed episodes, subscribe to the equivalent private feed in Overcast first. The tool will automatically detect that the destination has a podcast with a matching title and route those episodes there — no extra flags needed. To override the auto-match explicitly (e.g. when titles differ between platforms), use `--feed-map`.

If you prefer to match against a specific snapshot instead of auto-fetching the live account (e.g. for reproducible dry-run previews), provide one explicitly:

```sh
podcast-migrate migrate --from podcasts --to overcast \
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

**Rate limiting:** The tool sends one API request per episode with a 1 s delay between calls by default. Override with `--request-delay` (e.g. `--request-delay 2s`) if you hit rate limits.

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

### Overcast → Overcast (restore play state from an old export)

Useful when you've cleaned up your Overcast subscriptions — e.g. removed duplicate public/paid feeds — and want to restore play state from an earlier export.

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

# Dry-run first to preview what will be restored
podcast-migrate migrate --from overcast --to overcast \
  --overcast-source-opml ~/Downloads/old-export.opml \
  --play-state --force-update --dry-run

# Live run
podcast-migrate migrate --from overcast --to overcast \
  --overcast-source-opml ~/Downloads/old-export.opml \
  --play-state --force-update
```

**How it works:** The source OPML (`--overcast-source-opml`) provides the play history from your old account state. The tool authenticates and auto-fetches your current live library as the destination — no second OPML needed. `--force-update` overwrites episodes the destination already marks as played, which is what you want when restoring from an older snapshot.

**Plus-feed matching:** If your old export has both a public feed ("Fresh Air") and a paid feed ("Fresh Air Plus") and your cleaned-up account keeps only one of them, the tool matches episodes across those feeds by normalizing the title — so play state is restored to whichever variant is currently subscribed.

If you'd rather match against a specific snapshot of the cleaned account instead of the live state:

```sh
podcast-migrate migrate --from overcast --to overcast \
  --overcast-source-opml ~/Downloads/old-export.opml \
  --overcast-match-opml ~/Downloads/cleaned-export.opml \
  --play-state --force-update --dry-run
```

### Apple Podcasts → Pocket Casts (sync play state)

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

# Dry-run first to preview what will be written
podcast-migrate migrate --from podcasts --to pocketcasts \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from podcasts --to pocketcasts \
  --play-state
```

**How it works:** Reads your Apple Podcasts play state from `MTLibrary.sqlite`, authenticates with Pocket Casts, and calls the same internal API endpoint the Pocket Casts web player uses to save positions. Any podcast in your Apple Podcasts library not yet subscribed in Pocket Casts is automatically subscribed first. Changes sync to all your Pocket Casts devices automatically. Add `--subscribed-only` to only update already-subscribed feeds without subscribing to new ones.

**Subscriber / private feeds:** If you have Apple Podcasts Subscriptions (PSUB) or other subscriber-feed episodes, subscribe to the equivalent private feed in Pocket Casts first. The tool automatically matches source podcasts to destination subscriptions by fuzzy-normalised title — no `--subscribed-only` or `--feed-map` needed in the common case.

Episode matching uses a cascade: publish date + feed URL (primary), then fuzzy-normalised title + feed URL (fallback — handles season-marker variants like "S01"), then cross-feed pub date and title matching by podcast title for subscriber/private feeds. Episodes not found in Pocket Casts are skipped and reported. Episodes already marked played or further ahead in Pocket Casts are left alone.

Use `--since` to limit to recently changed episodes when running incrementally:

```sh
# Only sync episodes whose play state changed in the last week
podcast-migrate migrate --from podcasts --to pocketcasts \
  --play-state --since 7d
```

> **Disclaimer:** Uses an undocumented Pocket Casts endpoint that Automattic has not publicly supported. It works as of the implementation date but may break without notice.

### Pocket Casts → OPML (export subscriptions and play state)

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

podcast-migrate migrate --from pocketcasts --to opml \
  --opml-out ~/Desktop/pocketcasts-export.opml \
  --play-state
```

Generates an extended OPML file containing all your subscriptions and complete play history (played and in-progress episodes). The format is compatible with Overcast's extended export, so it can be used as a `--overcast-source-opml` source for further migrations.

Subscriber and private feeds (Slate Plus, NPR Plus, etc.) are included — their URLs are resolved from the Pocket Casts export service rather than the subscription list, which doesn't expose subscriber URLs.

Pass `--pc-include-unsubscribed` to also include play history for podcasts you've since unsubscribed from (feed URL is recovered from the Pocket Casts CDN where possible).

### OPML → any destination

Use a previously exported OPML file (from Pocket Casts, Overcast, or any other tool) as the source:

```sh
# Import subscriptions from an OPML file into Overcast
podcast-migrate migrate --from opml --to overcast \
  --opml-file ~/Downloads/pocketcasts-export.opml \
  --overcast-out ~/Desktop/overcast-import.opml

# Sync play state from an extended OPML file into Pocket Casts
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

podcast-migrate migrate --from opml --to pocketcasts \
  --opml-file ~/Downloads/pocketcasts-export.opml \
  --play-state
```

The OPML provider reads both standard OPML (subscriptions only) and the extended format with episode play state outlines (as produced by Overcast's export and by `--to opml --play-state`).

### Pocket Casts → Apple Podcasts (sync play state to iPhone)

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"
export APPLE_BEARER_TOKEN="eyJhbGci..."
export APPLE_MEDIA_USER_TOKEN="0.Apgf..."

# Dry-run first
podcast-migrate migrate --from pocketcasts --to podcasts \
  --play-state --dry-run

# Live run
podcast-migrate migrate --from pocketcasts --to podcasts \
  --play-state
```

See [Overcast → Apple Podcasts](#overcast--apple-podcasts-sync-play-state-to-iphone) for how to capture the Apple tokens (same one-time DevTools step). The Pocket Casts source provides complete play history — all episodes Pocket Casts has ever recorded, not just the most recent.

### Pocket Casts → Overcast (sync play state)

```sh
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"

podcast-migrate migrate --from pocketcasts --to overcast \
  --play-state --dry-run

podcast-migrate migrate --from pocketcasts --to overcast \
  --play-state
```

Any podcast from your Pocket Casts library not yet subscribed in Overcast is automatically subscribed before its episodes are written. Add `--subscribed-only` to only update already-subscribed feeds without subscribing to new ones.

### Overcast → Pocket Casts (sync play state)

```sh
export OVERCAST_EMAIL="you@example.com"
export OVERCAST_PASSWORD="yourpassword"
export POCKETCASTS_EMAIL="you@example.com"
export POCKETCASTS_PASSWORD="yourpassword"

# Export your Overcast library first (or let the tool auto-fetch it):
podcast-migrate migrate --from overcast --to pocketcasts \
  --play-state --dry-run

podcast-migrate migrate --from overcast --to pocketcasts \
  --play-state
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
| `--only-subscriptions` | Subscribe to podcasts only, skip episode play state |
| `--subscribed-only` | Only write play state for podcasts already subscribed at the destination; skip auto-subscribe for new feeds (Overcast and Pocket Casts destinations) |
| `--conflict` | Conflict resolution: `furthest` (default), `source`, `target` |
| `--sqlite` | Custom path to `MTLibrary.sqlite` (auto-detected by default) |
| `--opml-fallback` | Apple Podcasts OPML export to use if SQLite is inaccessible |
| `--play-state` | Write episode play state |
| `--podcast` | Limit play-state sync to podcasts matching this word/phrase (repeatable) |
| `--podcast-list` | Path to a file with one podcast title/word per line |
| `--request-delay` | Delay between API requests (default 1s; increase if you hit rate limits) |
| `--log-file` | Write per-episode CSV detail log (columns: status, podcast, episode, pub_date, source_state, target_state, note) |
| `--overcast-source-opml` | Path to Overcast extended OPML export used as the migration source (`--from overcast`) |
| `--overcast-match-opml` | Path to Overcast OPML used for destination episode matching when writing play state (optional; if omitted and credentials are set, the live account library is fetched automatically) |
| `--overcast-email` | Overcast account email (or `OVERCAST_EMAIL` env var) |
| `--overcast-password` | Overcast account password (or `OVERCAST_PASSWORD` env var) |
| `--pocketcasts-email` | Pocket Casts account email (or `POCKETCASTS_EMAIL` env var) |
| `--pocketcasts-password` | Pocket Casts account password (or `POCKETCASTS_PASSWORD` env var) |
| `--pc-include-unsubscribed` | When `--from pocketcasts`: also export play history for podcasts no longer subscribed to; feed URL is recovered via the Pocket Casts CDN |
| `--opml-file` | Path to source OPML file (required when `--from opml`); supports standard and extended OPML with episode play state |
| `--opml-out` | Path for generated OPML output (required when `--to opml`); writes extended OPML with play state when `--play-state` is set |
| `--apple-bearer-token` | Apple web API Bearer token (or `APPLE_BEARER_TOKEN` env var) |
| `--apple-media-user-token` | Apple media-user-token (or `APPLE_MEDIA_USER_TOKEN` env var) |
| `--strict-feed-match` | Only match episodes using feed-URL-anchored strategies; skips cross-feed title fallbacks |
| `--force-update` | Write source play state even if the destination already shows the episode as played or further along |
| `--feed-map` | Explicitly map a source feed URL to a destination feed URL (`SRC_URL=DST_URL`, repeatable). Use when title-based auto-matching isn't sufficient — for example when the podcast has a different title on each platform. Auto-matching handles the common case without this flag. |
| `--since` | Delta sync: only process Apple Podcasts episodes whose play state changed after this cutoff. Accepts a duration (`24h`, `7d`) or a date (`2026-06-01`). Only effective when `--from podcasts`. |

## Future work

### Additional providers
The `Provider` interface makes adding new services straightforward. Candidates:
- **Castro**

### Automated / scheduled sync
A `sync` subcommand that runs on a schedule (cron or a background agent) and incrementally syncs only changes since the last run, using a state file to track what was last seen.

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
  migrate/            Shared utilities: log helpers, feed URL normalisation, fuzzy title matching, skip-reason logic
  apple/              Apple Podcasts adapter (SQLite read; catalog API + web API write)
  overcast/           Overcast adapter (OPML read/write + web API)
  opml/               OPML adapter (standard and extended OPML read/write)
  pocketcasts/        Pocket Casts adapter (web API read/write)
  sync/               Merge engine, conflict resolution, and automatic subscriber feed remapping
main.go
```

## Tests

```sh
go test ./...
```

Tests: `apple` ~90%, `overcast` 95%, `pocketcasts` ~95%, `sync` 99%, `model` 100%.

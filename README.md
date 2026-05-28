# podcast-migrate

A command-line tool for migrating podcast subscriptions and episode play state between podcast applications. Currently supports Apple Podcasts → Overcast, with the architecture designed for bidirectional sync and additional services.

## How it works

podcast-migrate reads your library directly from the source app's local data, merges it with whatever the destination already knows, and writes the result in a format the destination app can import. Each podcast service is an interchangeable adapter behind a common `Provider` interface, so adding new services doesn't require changes to the core migration logic.

## Current status

### What's working

**Apple Podcasts (source)**

- Reads subscriptions and episode play state directly from `MTLibrary.sqlite` — no export step needed
- Falls back to a manually exported OPML file if the database isn't accessible
- Detects and reports two categories of content that can't be migrated:
  - **`internal://` feeds** — Apple-exclusive shows with no public RSS feed. Excluded from the subscription list entirely and reported by name so you know what was skipped.
  - **PSUB / PLUS episodes** — paywalled episodes from Apple Podcasts Subscriptions. These use Apple-internal GUIDs and DRM-only audio streams that no other app can match or play. The parent podcast subscription is still migrated; only the per-episode play state is dropped.

**Overcast (destination)**

- Generates an OPML file ready to import via Overcast › Settings › Import OPML
- Can also read an Overcast OPML export for inspection or two-way merging
- **Play state write is not yet implemented.** Overcast has no public API for setting episode positions. The groundwork is in place; see [Future work](#future-work).

**Sync engine**

- Three conflict resolution strategies when both sides have state for the same episode:
  - `furthest` *(default)* — whichever side is further along wins; fully-played always beats in-progress
  - `source` — source data always wins
  - `target` — existing destination data is never overwritten
- Episode matching across providers uses a priority chain: RSS `<guid>` → feed URL + pub date → feed URL + normalized title
- `--dry-run` previews what would change without writing anything

### Supported providers

| Provider | Read subscriptions | Read play state | Write subscriptions | Write play state |
|---|:---:|:---:|:---:|:---:|
| Apple Podcasts | ✅ | ✅ | — | — |
| Overcast | ✅ | ✅ | ✅ (OPML) | 🚧 |

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

### Migrate Apple Podcasts → Overcast

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

> **Note:** Play state is not written to Overcast yet — only subscriptions are imported. See [Future work](#future-work).

### Export your library to JSON

Snapshots your library as a portable JSON file. Useful for inspection, backup, or staging a migration.

```sh
# Print to stdout
podcast-migrate export --from podcasts

# Save to file
podcast-migrate export --from podcasts --out ~/Desktop/my-library.json

# Export from Overcast (requires a manual OPML export from overcast.fm/account/export_opml)
podcast-migrate export --from overcast \
  --overcast-export ~/Downloads/overcast.opml \
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

## Future work

### Overcast play state write
Overcast has no documented public API for setting episode positions or played status. The plan is to reverse-engineer the web endpoints used by the Overcast Mac app and implement them behind an `--play-state` flag with a clear disclaimer that the approach is unofficial and may break without notice.

### Additional providers
The `Provider` interface makes adding new services straightforward. Planned:
- **Pocket Casts** — has a documented sync API
- **Castro**
- **Pocketcasts** (Android)
- **RSS readers / generic OPML** — subscription-only, no play state

### Overcast → Apple Podcasts direction
Apple Podcasts has no import API. The only viable path is scripting the app via AppleScript or, on iOS, a Shortcut. This needs investigation.

### Automated / scheduled sync
A `sync` subcommand that runs on a schedule (cron or a background agent) and incrementally syncs only changes since the last run, using a state file to track what was last seen.

### Richer episode matching
The current GUID → pub date → title chain can fail when the same episode has different GUIDs across providers (common with older feeds that changed hosting). A fuzzy-match fallback using pub date proximity and edit distance on titles would reduce unmatched episodes.

### Packaged release
Signed macOS binary via GitHub Actions, distributed through Homebrew.

## Project structure

```
cmd/                  CLI entry points (migrate, export, import)
internal/
  model/              Shared types: Library, Podcast, EpisodeState
  provider/           Provider interface and WriteOptions
  apple/              Apple Podcasts adapter (SQLite + OPML)
  overcast/           Overcast adapter (OPML read/write)
  sync/               Merge engine and conflict resolution
main.go
```

## Tests

```sh
go test ./...
```

97 tests; coverage: `apple` 90%, `overcast` 95%, `sync` 99%.

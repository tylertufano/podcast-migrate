---
layout: default
title: Architecture
nav_order: 3
---

# Architecture

## Overview

`podcast-migrate` is built around a **provider interface** that abstracts each podcast platform. The sync engine reads from a source provider, merges the two libraries, and writes to a destination provider. A canonical intermediate model (`model.Library`) carries all state between providers.

```
┌─────────────────────────────────────────────────────────────────┐
│                         CLI (cmd/)                              │
│  migrate | export | import | mark-played | observe              │
└────────────────────────────┬────────────────────────────────────┘
                             │
                    ┌────────▼────────┐
                    │  sync.Engine    │  internal/sync/engine.go
                    │  Run(ctx, opts) │
                    └──┬──────────┬──┘
                       │          │
              ┌────────▼───┐  ┌───▼────────┐
              │  Source    │  │  Dest      │  provider.Provider
              │  Provider  │  │  Provider  │  internal/provider/
              └────────────┘  └────────────┘
                    ▲               ▲
        ┌───────────┼───────────────┼──────────────┐
        │           │               │              │
   ┌────▼────┐ ┌────▼─────┐ ┌──────▼─────┐ ┌─────▼────┐
   │  Apple  │ │ Overcast │ │ PocketCasts│ │  OPML    │
   │Podcasts │ │          │ │            │ │          │
   └─────────┘ └──────────┘ └────────────┘ └──────────┘
   internal/apple  internal/overcast  internal/pocketcasts  internal/opml
```

## Package Layout

```
podcast-migrate/
├── main.go                       entry point
├── cmd/                          cobra commands
│   ├── root.go                   Root() → adds all subcommands
│   ├── migrate.go                migrate command (main workflow)
│   ├── export.go                 export command (library → JSON)
│   ├── import.go                 import command (JSON → provider)
│   ├── markplayed.go             mark-played command (single episode)
│   ├── observe.go                observe command (SQLite watcher)
│   └── version.go                version string (set via ldflags)
├── internal/
│   ├── model/
│   │   └── library.go            Library, Podcast, EpisodeState, PlayState
│   ├── provider/
│   │   └── provider.go           Provider interface, Capabilities, WriteOptions
│   ├── sync/
│   │   └── engine.go             Engine.Run: merge + auto-feed-map + write
│   ├── migrate/
│   │   ├── match.go              NormalizeFeedURL, FuzzyNormalizeTitle, SkipReason
│   │   ├── match_strategy.go     MatchStrategy enum — canonical 6-step episode-lookup cascade
│   │   └── log.go                WriteLogHeader, WriteLogLine, PlayStateLabel
│   ├── httputil/
│   │   └── retry.go              RateLimitError, TransientError, RetryFunc (shared by all write providers)
│   ├── itunes/
│   │   └── search.go             FindByHints — shared iTunes search with URL/author hint scoring
│   ├── apple/
│   │   ├── provider.go           Provider: SQLite → web API fallback
│   │   ├── sqlite.go             SQLiteReader (MTLibrary.sqlite)
│   │   ├── sqlite_write.go       SQLite write utilities
│   │   ├── webapi.go             WebAPIWriter (amp-api.podcasts.apple.com)
│   │   ├── kvs.go                KVSWriter (bookkeeper.itunes.apple.com, private feeds)
│   │   ├── kvs_podcasts.go       KVSWriter subscription helpers (dedup, private-feed detection)
│   │   ├── kvsreader.go          KVSReader (cross-platform, no SQLite)
│   │   ├── itunes.go             batchITunesLookup (PID → canonical feed URL)
│   │   ├── catalog.go            CatalogClient (iTunes Search + amp-api episodes)
│   │   └── opml.go               OPMLReader (subscriptions fallback)
│   ├── overcast/
│   │   ├── provider.go           Provider + doWritePlayState + augmentIndexFromPodcastPages
│   │   ├── web.go                Login, SetProgress, FetchExtendedOPML, FetchPodcastEpisodes
│   │   ├── opml.go               OPMLReader + OPMLWriter
│   │   ├── id_cache.go           Persistent episode ID + written-state cache
│   │   └── log.go                Overcast-specific log helpers
│   ├── pocketcasts/
│   │   ├── provider.go           Provider + doWritePlayState (Phase A/A_sync/B)
│   │   └── web.go                API client (JSON/protobuf endpoints)
│   └── opml/
│       ├── opml.go               Standard OPML parser
│       └── provider.go           OPML source + output providers
└── go.mod                        Go 1.26, cobra, modernc/sqlite
```

## Key Dependencies

| Dependency | Use |
|---|---|
| `github.com/spf13/cobra` | CLI framework |
| `modernc.org/sqlite` | Pure-Go SQLite driver (no CGo required) |

All other functionality is implemented without third-party libraries: HTTP clients, HTML parsing, JSON, and OPML parsing use the Go standard library only.

## Data Model

### `model.Library`

The canonical intermediate representation shared by all providers:

```go
type Library struct {
    Podcasts               []Podcast
    Episodes               []EpisodeState
    ExportedAt             time.Time
    SourceProvider         string
    PaywalledEpisodesIncluded int
    SkippedInternalPodcasts   int
}
```

### `model.Podcast`

```go
type Podcast struct {
    FeedURL    string
    Title      string
    Author     string
    ImageURL   string
    OvercastID string  // from Overcast OPML overcastId attribute
    ITunesID   string  // iTunes Store collection ID; set by KVSReader, used by Overcast writer
    IsPrivate  bool    // subscriber/private edition — skip iTunes URL substitution at destinations
}
```

`model.NormalizePlusTitle(title string) string` — strips paid-tier and subscriber suffixes (` Plus`, ` +`, `+`, ` - Subscriber Feed …`, ` - Member Feed …`, etc.) and lowercases, enabling cross-feed title matching between public and subscriber editions.

`model.IsSubscriberFeed(title, feedURL string) bool` — detects subscriber or private editions via three signals: (1) title carries a paid-tier marker (`NormalizePlusTitle` differs from lowercased title); (2) feed URL uses `internal://` scheme (Apple-exclusive); (3) feed URL is on a known subscriber-only hosting platform (supercast.com, memberful.com, supporting.cast.st, patreon.com).

### `model.EpisodeState`

```go
type EpisodeState struct {
    GUID         string        // RSS <guid>, used as primary match key
    FeedURL      string        // parent podcast's RSS feed URL
    Title        string
    PubDate      time.Time
    Duration     time.Duration
    PlayState    PlayState     // Unplayed=0, Played=1, InProgress=2
    PlayPosition time.Duration // 0 = not started
    LastPlayed   time.Time
    FromDestination bool       // episode came from destination only (not source)
}
```

### `model.PlayState`

```go
const (
    PlayStateUnplayed   PlayState = 0
    PlayStatePlayed     PlayState = 1
    PlayStateInProgress PlayState = 2
)
```

## Sync Engine Data Flow

```
1. src.GetLibrary(ctx)                 → srcLib
2. dst.GetLibrary(ctx)                 → dstLib (optional; skipped for write-only)
3. buildAutoFeedMap(srcLib, dstLib)    → auto-derive subscriber-feed remappings
4. applyFeedMap(srcLib, autoMap)       → remap feed URLs for downstream matching
5. applyFeedMap(srcLib, opts.FeedMap)  → apply explicit --feed-map overrides
6. merge(srcLib, dstLib, opts)         → merged Library
7. dst.SetLibrary(ctx, merged, opts)   → write to destination
```

### `merge()` — Episode Matching

```
Pass 1 (primary):  GUID → FeedURL+PubDate → FeedURL+Title
Pass 2 (cross-feed): NormalizePlusTitle(podTitle)+Date, with ±1-day tolerance
                     (for paid-tier feeds: "Fresh Air Plus" ↔ "Fresh Air")
Remainder:          destination-only episodes flagged with FromDestination=true
```

See [Episode Matching](episode-matching.md) for the full matching cascade.

## Provider Interface

```go
type Provider interface {
    Name() string
    Capabilities() Capabilities
    GetLibrary(ctx context.Context) (*model.Library, error)
    SetLibrary(ctx context.Context, lib *model.Library, opts WriteOptions) error
}

type Capabilities struct {
    ReadSubscriptions  bool
    WriteSubscriptions bool
    ReadPlayState      bool
    WritePlayState     bool
}
```

`SetLibrary` receives the merged library and is responsible for filtering (using `opts.PodcastFilter`), episode matching against the destination's own index, skip-reason checks, API calls, and retry logic. The sync engine does not orchestrate individual episode writes.

## WriteOptions

`provider.WriteOptions` carries every caller-configurable write behaviour:

| Field | Description |
|---|---|
| `DryRun` | Log intent without writing |
| `OnlySubscriptions` / `OnlyPlayState` | Restrict write scope |
| `ConflictStrategy` | FurthestWins (default), SourceWins, TargetWins |
| `RequestDelay` | Pause between API requests (rate limiting) |
| `PodcastFilter` | Restrict writes to matching podcast titles |
| `LogWriter` | Per-episode CSV log |
| `TitleMatchDateTolerance` | Max pub-date gap for title-based matches |
| `StrictFeedMatch` | Disable cross-feed fallback strategies |
| `ForceUpdate` | Write even if destination is already ahead |
| `SubscribedOnly` | Skip unsubscribed podcasts on destination |
| `EpisodeCacheMaxAge` | Overcast episode ID cache expiry |
| `ClearEpisodeCache` | Discard and rebuild Overcast episode ID cache |
| `FeedMap` | Explicit `SRC_URL=DST_URL` feed URL remappings |

## Shared Utilities (internal/migrate)

These functions are used consistently across all write-side providers to ensure identical behaviour:

| Function | Description |
|---|---|
| `NormalizeFeedURL(url)` | Canonical URL: lowercase host, http→https, strip trailing `/` |
| `BuildFeedToTitle(lib)` | `feedURL → lowercased title` map |
| `FilterEpisodesByPodcast(eps, map, filters)` | Substring filter on podcast title |
| `FuzzyNormalizeTitle(title)` | Lowercase, strip season markers, remove punctuation |
| `SkipReason(desired, current)` | `"already_played"`, `"already_ahead"`, or `""` |
| `WriteLogHeader(w)` | CSV header: `status,podcast,episode,pub_date,source_state,target_state,note` |
| `WriteLogLine(w, ...)` | One CSV data row |

## Apple Podcasts SQLite Schema Notes

Apple Podcasts stores its database at:
```
~/Library/Group Containers/243LU875E5.groups.com.apple.podcasts/Documents/MTLibrary.sqlite
```

Key tables: `ZMTPODCAST`, `ZMTEPISODE`, `ZMTUPPMETADATA`. CoreData epoch: **2001-01-01 UTC** (all timestamps are seconds since this date).

| Column | Meaning |
|---|---|
| `ZPLAYSTATE` | 0=unplayed, 1=in-progress (started), 2=played |
| `ZPLAYSTATESOURCE` | 1=manual, 2=auto-mark, 3=completion, 4=device-sync, 6=default |
| `ZPLAYHEAD` | Playback position in seconds |
| `ZPLAYSTATELASTMODIFIEDDATE` | Updated when `ZPLAYSTATE` changes |
| `ZPLAYHEADLASTMODIFIEDDATE` | Updated whenever playhead advances (may be absent on older macOS) |
| `ZLASTDATEPLAYED` | Set on completion or iCloud sync |
| `ZPLAYCOUNT` | Total play count (cross-device) |
| `ZPRICETYPE` | `PSUB` or `PLUS` for Apple Podcasts Subscription episodes |
| `ZMETADATAIDENTIFIER` | KVS key — hex string that links this episode to its `ZMTUPPMETADATA` row |

**macOS 27+:** `ZDURATION` was removed from `ZMTEPISODE`. The reader probes for its existence at runtime and falls back to `NULL` gracefully.

### `ZMTUPPMETADATA`

Stores UPP (User Play Progress) metadata — the local mirror of Apple's KVS sync store (`bookkeeper.itunes.apple.com`). Apple Podcasts reads play state from this table for its UI, not from `ZMTEPISODE.ZPLAYSTATE`.

The two tables are updated through independent write paths, so they can permanently disagree even on a fully synced device: episodes played via one code path may only update one table. About 22% of interacted-with episodes have a `ZMTUPPMETADATA` row; for the remaining ~78%, the row is simply absent and `ZMTEPISODE` heuristics are the only source.

`SQLiteReader` uses `ZMTUPPMETADATA` as the **primary** play state source: when a row exists for an episode (keyed on `ZMETADATAIDENTIFIER`), `ZHASBEENPLAYED` and `ZBOOKMARKTIME` take precedence over all `ZMTEPISODE` heuristics. This eliminates false positives where `ZPLAYSTATE=2` but KVS says unplayed, and recovers plays that only KVS recorded.

`KVSWriter` uses `ZMTUPPMETADATA` to look up episode identifiers for private-feed write paths.

| Column | Meaning |
|---|---|
| `ZMETADATAIDENTIFIER` | The KVS key — a hex string unique per episode, used as the `key` field in `putAll` requests and as the join key with `ZMTEPISODE` |
| `Z_OPT` | CoreData optimistic lock counter — the local version, which may lag the server's version if other devices have synced. `KVSWriter` always fetches the live server version via `getAll` before writing; `Z_OPT` is used only as a fallback when the key is absent from the `getAll` response |
| `ZHASBEENPLAYED` | 1 = played (never NULL when a row exists) |
| `ZPLAYCOUNT` | Total play count |
| `ZBOOKMARKTIME` | Current playback position in seconds (0 = not started or fully played) |
| `ZTIMESTAMP` | CoreData epoch timestamp of the last state change |

**TCC restriction:** macOS Transparency Consent and Control blocks processes other than the user's own shell from reading the Podcasts group container. `podcast-migrate` runs as the user and therefore has access; IDE extensions and background processes may not.

---
layout: default
title: Testing
nav_order: 7
---

# Testing

## Running the test suite

```bash
go test ./...          # all packages
go test ./internal/... # library code only
go test ./cmd/...      # CLI command tests
```

All tests run offline. No live API credentials are required. Provider-to-API interactions are tested via `httptest.NewServer` fake servers in the test files themselves.

---

## Coverage by package

### `internal/sync` — sync engine

**File:** `engine_test.go` (~90 test functions)

The most heavily-tested package. Every significant code path in `engine.go` is covered:

| Area | Tests |
|---|---|
| `episodeKey` — GUID/feeddate/feedtitle priority | `TestEpisodeKey_*` (5 cases) |
| `furthestWins` — play state precedence and position | `TestFurthestWins_*` (6 cases) |
| `resolveConflict` — all three strategies | `TestResolveConflict_*` (3 cases) |
| `merge` — subscription union, episode matching, conflict | `TestMerge_*` (11 cases) |
| `merge` — cross-feed Plus/subscriber matching | `TestMerge_CrossFeed_*` (6 cases), `TestMerge_PSUBEpisode_*` |
| `buildCrossFeedIndex` | `TestBuildCrossFeedIndex` |
| `buildAutoFeedMap` — happy path, Plus normalization | `TestBuildAutoFeedMap_*` (10 cases) |
| `buildAutoFeedMap` — collision guards | `TestBuildAutoFeedMap_CollisionGuard1_*`, `TestBuildAutoFeedMap_CollisionGuard2_*` |
| `buildAutoFeedMap` — word-prefix matching | `TestBuildAutoFeedMap_SubtitleContainsMatch_*`, `TestBuildAutoFeedMap_SuffixTitle_*` |
| `applyFeedMap` — remapping, normalisation, immutability | `TestApplyFeedMap_*` (6 cases) |
| `Engine.Run` — write, error, skip paths | `TestEngine_Run_*` (9 cases) |
| `Result.String` — dry-run prefix, warnings | `TestResult_String_*` (3 cases) |

---

### `internal/apple/sqlite.go` — Apple Podcasts SQLite reader

**File:** `sqlite_test.go` (~45 test functions)

All tests use an in-memory SQLite database. No access to a real Podcasts library is required.

| Area | Tests |
|---|---|
| Podcast filtering (http/https, internal://, subscriber JWT feeds, unsubscribed) | `TestSQLiteReader_*Subscriptions*`, `*Excludes*`, `*Counts*` |
| Play state determination (ZPLAYSTATE values) | `TestSQLiteReader_PlayStateValues` |
| Play position and duration | `TestSQLiteReader_PlayPosition`, `TestSQLiteReader_Duration` |
| ZPLAYSTATESOURCE trust logic (sources 1–6) | `TestSQLiteReader_ManuallyMarkedPlayedIncluded`, `TestSQLiteReader_AutoMarkedEpisodeExcluded`, `TestSQLiteReader_iCloudSyncPlayedIncluded`, `TestSQLiteReader_UnCorroboratedSource3Excluded`, etc. |
| PSUB/PLUS paid episode inclusion | `TestSQLiteReader_IncludesPSUBEpisodes`, `TestSQLiteReader_IncludesPLUSEpisodes`, `TestSQLiteReader_ExcludesPSUBOnInternalFeed` |
| CoreData epoch timestamp conversion | `TestSQLiteReader_CoreDataTimestampConversion` |
| `ZDURATION` column probe (macOS 27+ schema) | `TestSQLiteReader_MissingDurationColumn` |
| Cache-buster `?t=` stripping | `TestSQLiteReader_EpisodeFeedURL_CacheBusterStripped` |
| Delta sync `--since` filter | `TestSQLiteReader_SinceTime_FiltersEpisodes` |
| OPML fallback on missing/corrupt SQLite | `TestAppleProvider_FallsBackToOPML_*` (2 cases) |
| Error path when both sources missing | `TestAppleProvider_ReturnsError_WhenBothMissing` |

---

### `internal/apple/catalog.go` — Apple catalog client

**File:** `catalog_test.go` (8 test functions)

All tests use `httptest.NewServer` to serve fake iTunes Search API and amp-api responses.

| Area | Tests |
|---|---|
| iTunes search: feed URL match | `TestSearchITunes_FeedURLMatch` |
| iTunes search: title fallback (case-insensitive) | `TestSearchITunes_TitleFallback`, `TestSearchITunes_TitleFallback_CaseInsensitive` |
| iTunes search: Plus-title fallback (both `Plus` and `+` suffix) | `TestSearchITunes_PlusTitleFallback`, `TestSearchITunes_PlusTitleFallback_PlusSymbol` |
| iTunes search: exact match not degraded by Plus fallback | `TestSearchITunes_PlusTitleFallback_NotUsedForExactMatch` |
| iTunes search: not found | `TestSearchITunes_NotFound` |
| `FindEpisode` end-to-end with title fallback | `TestFindEpisode_TitleFallbackEndToEnd` |

---

### `internal/apple/opml.go` — Apple Podcasts OPML reader

**File:** `opml_test.go` (8 test functions): flat and nested outlines, empty body, missing file, invalid XML, no play state returned.

---

### `internal/migrate/match.go` — shared matching utilities

**File:** `match_test.go` (~30 test functions)

| Area | Tests |
|---|---|
| `NormalizeFeedURL` | http→https, trailing slash, host case, fragment, query, already-https |
| `BuildFeedToTitle` | nil library, lowercased titles, empty feed URL skipped |
| `FilterEpisodesByPodcast` | empty filters, case-insensitive matching |
| `FuzzyNormalizeTitle` | season marker removal (S01/S1/Season), apostrophe removal, hyphen→space, "serial" not stripped, round-trip across feed variants |
| `SkipReason` | all combinations of desired vs current state |

---

### `internal/model/library.go` — data model

**File:** `library_test.go` (1 test function)

`TestNormalizePlusTitle` covers all Plus-tier and subscriber-feed suffix stripping variants.

---

### `internal/overcast/` — Overcast provider

The Overcast package has the most test files, covering every sub-component:

| File | What's covered |
|---|---|
| `augment_test.go` (12 cases) | `augmentIndexFromPodcastPages`: empty episodes, all-unplayed, already-indexed, strict-feed-match, subscribed-only, normal path, numeric-ID shortcut, deduplication of podcast pages, title fallback, ±1-day tolerance |
| `feedurl_test.go` (3 cases) | `NormalizeFeedURL`, `buildOvercastIndex` URL normalisation, `findInOvercastIndex` trailing-slash mismatch |
| `filter_test.go` (2 cases) | `FilterEpisodesByPodcast`, `BuildFeedToTitle` (local copies used in Overcast write path) |
| `id_cache_test.go` (22 cases) | Get/Set/Clear/Size/Save lifecycle, maxAge hits/misses, legacy v0 format migration, concurrent access, round-trip disk persistence |
| `log_test.go` (20 cases) | `csvField` quoting/escaping, `PlayStateLabel`, `WriteLogHeader`, `WriteLogLine` (nil writer no-ops, column count, CSV escaping) |
| `opml_test.go` (17 cases) | OPMLReader subscriptions, played/in-progress episodes, pub-date formats (RFC1123Z, RFC3339), malformed dates, OPMLWriter XML validity, round-trip |
| `plus_test.go` (7 cases) | `buildOpmlTitleIndex` Plus-norm key generation, `+` symbol variant, collision resolution |
| `provider_test.go` (12 cases) | Provider name/capabilities, GetLibrary/SetLibrary (OPML), dry-run, credential path, missing-path errors |
| `satisfaction_test.go` (4 cases) | `OvercastSkipReason`, `findInOvercastIndex` strict-feed-match, `OvercastAlreadySatisfied`, `buildOvercastIndex` current-state storage |
| `web_test.go` (~45 cases) | Login, SetProgress (all states, sentinel value, redirect-to-login detection), `FetchEpisodeNumericID` (success, fallback to `set_progress` URL, rate limit), `SearchPodcastITunesID`, `FetchSubscribedPodcasts`, `SubscribeToPodcast`, `FetchPodcastEpisodes` (HTML parsing edge cases), `FetchExtendedOPML`, rate-limit (429) handling for all endpoints |

---

### `internal/pocketcasts/` — Pocket Casts provider

| File | What's covered |
|---|---|
| `index_test.go` (2 cases) | `findInIndex`: cross-podcast `titledate` fallback, `feeddate` key priority over `titledate` |
| `provider_test.go` (~22 cases) | GetLibrary (podcasts + in-progress + history, dedup, deleted-episode handling), SetLibrary Phase A/A_sync/B (dry-run, played write, skip-from-destination, subscriptions, filter, already-subscribed, URL-mismatch Phase B, sync overlay, sync overlay fallback), `Capabilities`, `Name` |
| `web_test.go` (~28 cases) | Login, `FetchSubscribedPodcasts`, `FetchInProgressEpisodes`, `FetchPlayedEpisodes`, `FetchPodcastEpisodes` (pagination), `UpdateEpisodeProgress` (played/in-progress, rate limit, transient error, bad request), `ResolveFeedToPodcastUUID` (immediate OK, poll-then-OK, error), `SubscribePodcast`, `FetchSyncUpdate` (including delta sync lastModified and server error) |

---

### `internal/opml/` — standard OPML parser

**File:** `opml_test.go` (12 cases): standard and extended OPML parsing, group containers, RFC1123Z pub date, malformed XML, OPMLWriter standard and extended, skip-from-destination episodes, round-trip.

---

### `cmd/` — CLI commands

**File:** `migrate_test.go` (~40 cases)

All utility functions extracted from `migrate.go` are tested:

| Area | Tests |
|---|---|
| `parseConflictStrategy` | all three strategies, empty string, unknown string |
| `buildPodcastFilter` | nil input, CLI patterns (lowercase, dedup, trim, blank skip), list file, merging CLI+file, dedup across sources, file-not-found error |
| `buildFeedMap` | nil input, parse pairs, normalises http→https, multiple entries, missing `=`, empty src, empty dst |
| `buildProvider` | apple (name and alias), overcast (various flag combos), pocketcasts (with/without credentials, alias), OPML (source, output, no-path error), unknown name |
| `parseSince` | `Nd`, Go duration, date, datetime-no-zone, RFC3339, invalid |

---

## Coverage gaps

The following areas lack test coverage. These are catalogued here for future test development.

### `internal/apple/webapi.go` — WebAPIWriter *(high value, no tests)*

`WebAPIWriter` has zero unit tests. The three core functions are all testable with `httptest.NewServer`:

| Function | What to test |
|---|---|
| `getServerPosition` | 200 with position/completed, 404 (not-found → zero position), 5xx/network error |
| `markPosition` | Successful PUT; 5xx triggers retry (up to 3 attempts, exponential backoff); 4xx not retried; context cancellation stops mid-retry |
| `Write` | Already-at-played → skipped; already-at-position → skipped; `ForceUpdate` bypasses skip; episode not in catalog → logged as not-found; dry-run logs but does not call `markPosition` |

The main obstacle is that `CatalogClient.FindEpisode` would need to be injected or stubbed. The simplest approach is an interface wrapping `FindEpisode` and `getServerPosition` that can be swapped out in tests.

### `internal/apple/sqlite_write.go` — SQLite write utilities *(low priority)*

This file is a small helper for writing back to the local SQLite database. It is called from `cmd/observe.go` and is not part of the primary migration write path (which goes through `WebAPIWriter`). The functions are tightly coupled to an open `*sql.DB`, making unit tests feasible but lower-value.

### `cmd/observe.go` — observe command *(medium value)*

`observe.go` is 473 lines with no tests. The command contains pure Go functions that are unit-testable without a real database:

| Function | What to test |
|---|---|
| `diffEpisode` | No change (identical snaps); play state change; play head advance; new episode |
| `fromCoreData` | Epoch conversion (2001-01-01 + seconds = expected UTC time) |
| `cdateStr` / `nullStr` / `tsLookup` | Nil vs non-nil SQL values |
| `readAllPlayStateKeys` | Parse output of `defaults read com.apple.podcasts` (can test with a fixed string) |
| `queryEpisodes` | Use an in-memory SQLite DB (same approach as `sqlite_test.go`) |
| `runObserveLoop` | Verify that a single-iteration loop detects a row change and calls `diffEpisode` |

### `cmd/export.go`, `cmd/import.go`, `cmd/markplayed.go` *(low priority)*

These are thin command wrappers (~70–80 lines each). Their core logic delegates to provider `GetLibrary`/`SetLibrary` and to the Overcast web client, which are already tested. Integration-level tests would require either mock providers or live credentials. The main untested code paths are:

- `export.go`: JSON serialisation of `model.Library` and `--out` file write
- `import.go`: JSON deserialisation and the `--only-subscriptions` / `--conflict` flag wiring
- `markplayed.go`: `FetchEpisodeNumericID` → `SetProgress` call chain (the two underlying functions are individually tested in `overcast/web_test.go`)

### Apple catalog `paginateEpisodes` and 4-key index *(low priority)*

`catalog_test.go` tests `FindEpisode` end-to-end, which exercises `paginateEpisodes` internally. There is no dedicated test for the pagination cursor logic (the `next` link on the last page) or for the exact 4-key index structure. A test with two pages of fake catalog responses would cover this.

### Apple `buildAutoFeedMap` word-prefix false-positive guard *(already partially covered)*

`TestBuildAutoFeedMap_ShortTitleContains_NoFalsePositive` and `TestBuildAutoFeedMap_SuffixTitle_NoFalsePositive` exist. One additional case worth adding: a destination title that is a **substring** of the source title but not a word-aligned prefix (e.g. "pod" matching "podcast") to confirm `titleHasWordPrefix` rejects it.

### Retry budget interaction under concurrent Overcast rate limiting *(low priority)*

The `augmentIndexFromPodcastPages` worker pool (5 workers) shares a single `requestDelay` ticker. There is no test for what happens when multiple workers hit 429 simultaneously — specifically, that the retry delay is applied per-worker and does not cause a deadlock. This would require a mock server that returns 429 for the first N requests.

---

## Recommended additions (priority order)

1. **`internal/apple/webapi_test.go`** — inject a `positionGetter` interface so `WebAPIWriter.Write` can be tested without live tokens. Cover: skip-on-already-played, retry-on-5xx (3 attempts), no-retry-on-4xx, dry-run, `ForceUpdate`.

2. **`cmd/observe_test.go`** — test `diffEpisode`, `fromCoreData`, `readAllPlayStateKeys` (with a fixed plist-formatted string), and `queryEpisodes` against an in-memory SQLite DB.

3. **`internal/apple/catalog_test.go` — pagination** — add a test where the first catalog page contains a `next` cursor and a second page contains the episode, verifying the paginator follows the link.

4. **`cmd/export_test.go` and `cmd/import_test.go`** — round-trip test: export a fixed `model.Library` to a temp file, import it back, assert the result matches the original.

5. **`internal/overcast/augment_test.go` — rate-limit interaction** — a mock server that returns 429 for the first request to each episode page, verifying the worker respects the `Retry-After` header and eventually succeeds.

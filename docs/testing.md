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

**File:** `catalog_test.go` (9 test functions)

All tests use `httptest.NewServer` to serve fake iTunes Search API and amp-api responses.

| Area | Tests |
|---|---|
| iTunes search: feed URL match | `TestSearchITunes_FeedURLMatch` |
| iTunes search: title fallback (case-insensitive) | `TestSearchITunes_TitleFallback`, `TestSearchITunes_TitleFallback_CaseInsensitive` |
| iTunes search: Plus-title fallback (both `Plus` and `+` suffix) | `TestSearchITunes_PlusTitleFallback`, `TestSearchITunes_PlusTitleFallback_PlusSymbol` |
| iTunes search: exact match not degraded by Plus fallback | `TestSearchITunes_PlusTitleFallback_NotUsedForExactMatch` |
| iTunes search: not found | `TestSearchITunes_NotFound` |
| `FindEpisode` end-to-end with title fallback | `TestFindEpisode_TitleFallbackEndToEnd` |
| `paginateEpisodes` multi-page: episode on page 2 is found | `TestPaginateEpisodes_FindsEpisodeOnSecondPage` |

---

### `internal/apple/private_feed.go` — subscriber feed URL resolution

**File:** `private_feed_test.go` (32 test functions)

New tests added for the four-class classification and URL resolution pipeline:

| Area | Tests |
|---|---|
| `normalizeEpTitle` — lowercase, HTML entity unescape, trim | `TestNormalizeEpTitle_*` (4 cases) |
| `classifyMismatchedFeed` — all 4 class outcomes | `TestClassifyMismatchedFeed_EmptyKVS_ReturnsPrivateAuth`, `_IdenticalContent_ReturnsPublicEquivalent`, `_KVSHasExclusiveEpisode_ReturnsPublicSubscriber`, `_KVSHasOlderEpisodes_ReturnsPublicArchive` |
| `classifyMismatchedFeed` — exclusive count, items-before-floor guard | `TestClassifyMismatchedFeed_MultipleExclusiveEpisodes_CountsCorrectly`, `_KVSItemBeforeFloor_NotCountedAsExclusive` |
| `classifyMismatchedFeed` — iTunes undated items, HTML entity title matching | `TestClassifyMismatchedFeed_iTunesNoDatableItems_ReturnsPublicArchive`, `_TitleNormalizationMatchesHTMLEntities` |
| `privateFeedClass.String` — all 4 string labels | `TestPrivateFeedClass_String` |
| `resolveURL` — `public` mode always uses canonical | `TestResolveURL_PublicMode_AlwaysReturnsCanonical` |
| `resolveURL` — `kvs` mode always uses KVS URL | `TestResolveURL_KVSMode_AlwaysReturnsKVSURL` |
| `resolveURL` — `subscriber` mode × 4 class types | `TestResolveURL_SubscriberMode_PrivateAuth_ReturnsCanonical`, `_PublicEquivalent_ReturnsCanonical`, `_PublicSubscriber_ReturnsKVSURL`, `_PublicArchive_ReturnsKVSURL` |
| `resolveURL` — unknown mode falls through to canonical | `TestResolveURL_UnknownMode_ReturnsCanonical` |
| `ParsePrivateFeedMode` — all valid values, case-insensitive, trimmed | `TestParsePrivateFeedMode_ValidValues` |
| `ParsePrivateFeedMode` — invalid values, error message content | `TestParsePrivateFeedMode_InvalidValue_ReturnsError`, `_ErrorMessageContainsValidOptions` |

All functions in `private_feed.go` except `promptPrivateFeedChoice` (requires a TTY) are now at 100% statement coverage.

---

### `internal/apple/rss.go` — RSS date and duration parsing

**File:** `rss_test.go` (19 test functions)

New tests for the two pure parsing functions:

| Area | Tests |
|---|---|
| `parsePubDate` — RFC1123Z, RFC1123 UTC, single-digit day, ISO 8601 Z and offset | `TestParsePubDate_RFC1123Z`, `_RFC1123Z_WithOffset`, `_RFC1123_UTCNamedZone`, `_SingleDigitDay`, `_SingleDigitDay_WithOffset`, `_ISO8601_UTC`, `_ISO8601_WithOffset` |
| `parsePubDate` — result is always UTC, empty/invalid → zero | `TestParsePubDate_ResultIsUTC`, `_Empty_ReturnsZero`, `_Invalid_ReturnsZero`, `_WhitespaceOnly_ReturnsZero` |
| `parseItunesDuration` — HH:MM:SS, MM:SS, plain seconds (including unit check) | `TestParseItunesDuration_HHMMSS`, `_HHMMSS_Zero`, `_MMSS`, `_SecondsOnly`, `_SecondsUnit_IsSeconds` |
| `parseItunesDuration` — empty, whitespace, non-numeric string, non-numeric HH:MM:SS | `TestParseItunesDuration_Empty_ReturnsZero`, `_WhitespaceOnly_ReturnsZero`, `_InvalidString_ReturnsZero`, `_InvalidHHMMSS_ReturnsZero` |

---

### `internal/apple/webapi.go` — Apple Web API writer

**File:** `webapi_test.go` (9 test functions, `package apple` white-box)

| Area | Tests |
|---|---|
| `getServerPosition` — completed episode | `TestGetServerPosition_Completed` |
| `getServerPosition` — in-progress episode | `TestGetServerPosition_InProgress` |
| `getServerPosition` — 404 → `recorded=false` | `TestGetServerPosition_NotFound_ReturnsFalseRecorded` |
| `getServerPosition` — 200 with empty data array → `recorded=false` | `TestGetServerPosition_EmptyData_ReturnsFalseRecorded` |
| `getServerPosition` — 5xx → error | `TestGetServerPosition_ServerError_ReturnsError` |
| `getServerPosition` — malformed JSON → `recorded=false`, no error | `TestGetServerPosition_MalformedJSON_ReturnsFalseRecorded` |
| `markPosition` — 200 success | `TestMarkPosition_Success` |
| `markPosition` — 4xx → permanent error | `TestMarkPosition_ClientError_ReturnsError` |
| `markPosition` — cancelled context → error | `TestMarkPosition_ContextCancelled_ReturnsError` |
| `markPosition` — request body contains `type`, `completed`, `positionInMilliseconds` | `TestMarkPosition_RequestBodyIncludesFields` |

Tests use `rewriteHostTransport` (defined in `catalog_test.go`) and the white-box `w.httpClient` field to redirect all HTTP traffic to the `httptest.Server`.

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
| `private_feed_test.go` (5 cases) | `IsPrivate=true` feed: skips iTunes ID fast path and uses feed URL via `add_feed_url`; successful resolution → subscribe; failed resolution → skipped-feeds OPML; mixed success (one resolved, one not); public feed failure → warning only, no OPML |
| `provider_test.go` (~24 cases) | GetLibrary (podcasts + in-progress + history, dedup, deleted-episode handling), SetLibrary Phase A/A_sync/B (dry-run, played write, skip-from-destination, subscriptions, filter, already-subscribed, URL-mismatch Phase B, sync overlay, sync overlay fallback), `Capabilities`, `Name`; Phase B historical episode URL regression: no-duplicate-subscribe guard, RSS title resolution → title match |
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

**File:** `observe_test.go` (23 cases)

Pure Go helpers in `observe.go` tested without a real Podcasts database:

| Area | Tests |
|---|---|
| `fromCoreData` | Epoch (2001-01-01 + 0 s = epoch), known offset (3600 s), known absolute date (2024-01-15T12:00Z) |
| `nullStr` | Valid string, null, empty-but-valid |
| `cdateStr` | Null → "NULL", valid → raw value in output, parenthesised formatted time |
| `tsLookup` | Invalid → "NULL", found, not found, nil map |
| `diffEpisode` | No change → no output, play state change, play head advance, multiple changes (field order independence) |
| `queryEpisodes` | In-memory SQLite: subscribed HTTP feeds, unsubscribed excluded, non-HTTP feeds excluded, podcast filter, episode filter, play state fields populated |

**File:** `export_test.go` (5 cases)

| Area | Tests |
|---|---|
| `exportCmd` with OPML source → JSON file | Written file is valid JSON and contains expected podcast |
| `exportCmd` with OPML source → stdout | Output contains podcast title and feed URL |
| Missing `--from` flag | Returns error |
| Unknown provider | Returns error |
| Missing OPML file | Returns error |

**File:** `import_test.go` (6 cases)

| Area | Tests |
|---|---|
| Missing input file | Returns file-not-found error |
| Invalid JSON in input | Returns parse error |
| Missing `--to` flag | Returns cobra required-flag error |
| Missing `--in` flag | Returns cobra required-flag error |
| Unknown destination provider | Returns error |
| OPML → JSON → OPML round-trip | JSON file created; parse succeeds; library round-trips correctly |

---

## Coverage gaps

The following areas lack offline test coverage. These are catalogued here for future test development.

### `internal/apple/private_feed.go` — `promptPrivateFeedChoice` *(untestable without TTY mock)*

`promptPrivateFeedChoice` reads from `os.Stdin` and therefore cannot be tested in a standard `go test` run. It is exercised only when `--private-feed=custom` is used interactively. The remaining functions in `private_feed.go` are at 100% statement coverage.

---

### `internal/apple/webapi.go` — `WebAPIWriter.Write` end-to-end *(partially covered)*

`getServerPosition` and `markPosition` are now tested in `webapi_test.go` (9 cases) using `httptest.NewServer` and white-box `w.httpClient` injection. The remaining gap is the `Write` method itself, which creates a `CatalogClient` internally and therefore requires either:
- Adding a `catalogFinder` interface to `WebAPIWriter` (a small production-code change), or
- Constructing the full request set against a combined test server that handles both catalog and playback-position endpoints.

The uncovered paths within `Write` are: skip-when-already-played, skip-when-already-at-position, `ForceUpdate` bypass, episode-not-in-catalog logging, and dry-run.

---

### `internal/apple/sqlite_write.go` — SQLite write utilities *(low priority)*

This file is a small helper for writing back to the local SQLite database. It is called from `cmd/observe.go` and is not part of the primary migration write path (which goes through `WebAPIWriter`). The functions are tightly coupled to an open `*sql.DB`, making unit tests feasible but lower-value.

---

### `cmd/observe.go` — `readAllPlayStateKeys` and `runObserveLoop` *(low priority)*

The pure helpers (`diffEpisode`, `fromCoreData`, `cdateStr`, `nullStr`, `tsLookup`, `queryEpisodes`) are now tested in `observe_test.go`. Two functions remain without tests:

- **`readAllPlayStateKeys`** — calls `exec.Command("defaults", "read", prefPath)` and parses its output. The parsing logic is not extracted, so testing requires either a real macOS preferences file or refactoring the function into a parser + exec layer.
- **`runObserveLoop`** — the main polling loop. Testing it would require a real or fake SQLite database that mutates between polls.

---

### Retry budget interaction under concurrent Overcast rate limiting *(low priority)*

The `augmentIndexFromPodcastPages` worker pool (5 workers) shares a single `requestDelay` ticker. There is no test for what happens when multiple workers hit 429 simultaneously — specifically, that the retry delay is applied per-worker and does not cause a deadlock. This would require a mock server that returns 429 for the first N requests.

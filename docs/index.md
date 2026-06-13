---
layout: default
title: Home
nav_order: 1
---

# podcast-migrate

A Go CLI tool that migrates podcast subscriptions and episode play state between **Apple Podcasts**, **Overcast**, and **Pocket Casts** — bidirectionally, with conflict resolution and idempotent repeat runs.

## Supported directions

| Source | Destination | Subscriptions | Play State |
|--------|-------------|:---:|:---:|
| Apple Podcasts | Overcast | ✓ | ✓ (web API) |
| Apple Podcasts | Pocket Casts | ✓ | ✓ (web API) |
| Overcast | Apple Podcasts | — | ✓ (web API) |
| Overcast | Pocket Casts | ✓ | ✓ (web API) |
| Pocket Casts | Overcast | ✓ | ✓ (web API) |
| Pocket Casts | Apple Podcasts | — | ✓ (web API) |
| Any | OPML file | ✓ | ✓ (extended OPML) |

## Quick start

```bash
# Build from source
make build          # produces ./podcast-migrate
make install        # installs to $GOPATH/bin

# Export Apple Podcasts library to JSON
./podcast-migrate export --from podcasts --out ~/Desktop/my-podcasts.json

# Migrate Apple Podcasts → Overcast (subscriptions only, dry run first)
./podcast-migrate migrate --from podcasts --to overcast \
  --overcast-out ~/Desktop/import-to-overcast.opml --dry-run

# Migrate Apple Podcasts → Overcast (subscriptions + play state)
export OVERCAST_EMAIL=you@example.com
export OVERCAST_PASSWORD=yourpassword
./podcast-migrate migrate --from podcasts --to overcast \
  --overcast-source-opml ~/Downloads/overcast.opml \
  --overcast-out ~/Desktop/import-to-overcast.opml \
  --play-state

# Delta sync: only episodes changed in the last 48 hours
./podcast-migrate migrate --from podcasts --to overcast \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state \
  --since 48h

# Reverse sync: Overcast → Apple Podcasts play state
export APPLE_BEARER_TOKEN="eyJ..."
export APPLE_MEDIA_USER_TOKEN="0.Apg..."
./podcast-migrate migrate --from overcast --to podcasts \
  --overcast-source-opml ~/Downloads/overcast.opml --play-state
```

## Documentation

- [Architecture](architecture.md) — system design, package layout, data flow
- [Commands](commands.md) — all CLI commands and their flags
- [Providers](providers.md) — Apple Podcasts, Overcast, Pocket Casts, OPML adapters
- [Episode Matching](episode-matching.md) — matching cascade, conflict resolution, feed mapping
- [Testing](testing.md) — test suite overview, coverage gaps, and planned improvements

## Installation

Download a pre-built binary from [GitHub Releases](https://github.com/tylertufano/podcast-migrate/releases), or build from source:

```bash
git clone https://github.com/tylertufano/podcast-migrate
cd podcast-migrate
make build
```

**Requirements:** Go 1.26+, macOS (for the Apple Podcasts SQLite reader).

## License

MIT

# Teldrive

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/tgdrive/teldrive)
<a href="https://trendshift.io/repositories/7568" target="_blank"><img src="https://trendshift.io/api/badge/repositories/7568" alt="divyam234%2Fteldrive | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/></a>

Teldrive is a powerful utility that turns your Telegram account into a cloud-storage drive. It lets you **browse, organise and stream** all files stored in your Telegram channels, and exposes a standard REST API so you can mount it with **Rclone/WebDAV** just like Google Drive or S3.

---

## Table of Contents

- [Features](#features)
- [Prerequisites](#prerequisites)
- [Deploy with Docker (recommended)](#deploy-with-docker-recommended)
  - [1. Create the Docker network](#1-create-the-docker-network)
  - [2. Start PostgreSQL](#2-start-postgresql)
  - [3. Create your config file](#3-create-your-config-file)
  - [4. Start Teldrive](#4-start-teldrive)
  - [5. All-in-one compose file](#5-all-in-one-compose-file)
- [Configuration Reference](#configuration-reference)
- [Browsing & Filtering Channel Files](#browsing--filtering-channel-files)
  - [Listing your channels via the REST API](#listing-your-channels-via-the-rest-api)
  - [Filtering files by channel](#filtering-files-by-channel)
  - [Finding your channel ID](#finding-your-channel-id)
- [Mount with Rclone / WebDAV](#mount-with-rclone--webdav)
  - [Rclone configuration](#rclone-configuration)
  - [Mount a full drive](#mount-a-full-drive)
  - [Mount a single channel](#mount-a-single-channel)
- [Manual / Development Setup](#manual--development-setup)
- [Best Practices](#best-practices)
- [Contributing](#contributing)
- [Donate](#donate)
- [Star History](#star-history)

---

## Features

- **Exceptional Speed** – written in Go; outperforms Python-based alternatives.
- **Web UI** – intuitive file manager with upload, download, rename, move, copy, share and search.
- **Channel File Browsing** – list and filter files by Telegram channel ID directly from the UI or API.
- **Rclone / WebDAV support** – mount any channel as a local drive or use it with any WebDAV client.
- **Multipart streaming** – stream large files with HTTP range requests.
- **Encryption** – optional client-side encryption for uploaded parts.
- **Bots** – add Telegram bot tokens to boost upload/download throughput.
- **Scheduled maintenance** – automatic cleanup of deleted files, pending uploads and folder-size recalculation.

---

## Prerequisites

| Requirement | Details |
|---|---|
| **Telegram API credentials** | `app_id` and `app_hash` from [my.telegram.org/apps](https://my.telegram.org/apps) |
| **PostgreSQL 15+** | A running Postgres instance (or use the provided Docker Compose) |
| **Docker + Docker Compose** | For the recommended deployment method |
| **A Telegram account** | The account whose channels you want to expose |

> [!IMPORTANT]
> Teldrive is a wrapper over your personal Telegram account. You **must** respect Telegram's API rate limits. Abusing the API **will get your account banned** and your channel data deleted from Telegram's servers.

---

## Deploy with Docker (recommended)

The Docker image is published at **`ghcr.io/nishok000123/teldrive`** and the custom Postgres image (with required extensions) at **`ghcr.io/tgdrive/postgres:17-alpine`**.

### 1. Create the Docker network

All services share a dedicated bridge network so they can reach each other by container name.

```bash
docker network create postgres
```

### 2. Start PostgreSQL

Save the following as `docker/compose/postgres.yml` (already included in the repo) and start it:

```yaml
# docker/compose/postgres.yml
services:
  postgres_db:
    image: ghcr.io/tgdrive/postgres:17-alpine
    container_name: postgres_db
    restart: always
    networks:
      - postgres
    environment:
      - POSTGRES_USER=teldrive
      - POSTGRES_PASSWORD=secret        # change this!
      - POSTGRES_DB=postgres
    volumes:
      - ./postgres_data:/var/lib/postgresql/data

networks:
  postgres:
    external: true
```

```bash
docker compose -f docker/compose/postgres.yml up -d
```

### 3. Create your config file

Copy the sample and fill in your values:

```bash
cp config.sample.toml config.toml
```

Minimum required fields:

```toml
# config.toml

[db]
# Replace with your actual credentials from step 2
data-source = "postgres://teldrive:secret@postgres_db/postgres?sslmode=disable"

[jwt]
# Generate a strong random string: openssl rand -hex 32
secret = "CHANGE_ME_TO_A_RANDOM_SECRET"

[tg]
# Get these from https://my.telegram.org/apps
app-id   = 2496                               # replace with your app_id
app-hash = "8da85b0d5bfe62527e5b244c209159c3" # replace with your app_hash
```

> **YAML alternative:** you can also use `config.yml` (see `config.sample.yml` for the full YAML version).

### 4. Start Teldrive

```yaml
# docker/compose/teldrive.yml
services:
  teldrive:
    image: ghcr.io/nishok000123/teldrive
    restart: always
    container_name: teldrive
    networks:
      - postgres
    volumes:
      - ./config.toml:/config.toml   # mount your config
    ports:
      - 8080:8080

networks:
  postgres:
    external: true
```

```bash
docker compose -f docker/compose/teldrive.yml up -d
```

Open `http://localhost:8080` in your browser, log in with your Telegram account, and you're done.

### 5. All-in-one compose file

The repository ships a ready-to-use `docker-compose.yml` in the project root. It includes a `build: .` directive so you can always build from your **local source code** instead of pulling a pre-built image.

```yaml
services:
  postgres_db:
    image: ghcr.io/tgdrive/postgres:17-alpine
    container_name: postgres_db
    restart: always
    environment:
      POSTGRES_USER: teldrive
      POSTGRES_PASSWORD: secret        # change this!
      POSTGRES_DB: postgres
    volumes:
      - postgres_data:/var/lib/postgresql/data
    networks:
      - teldrive_net

  teldrive:
    build: .                           # build from local source (Dockerfile)
    image: ghcr.io/nishok000123/teldrive
    container_name: teldrive
    restart: always
    depends_on:
      - postgres_db
    volumes:
      - ./config.toml:/config.toml
    ports:
      - "8080:8080"
    networks:
      - teldrive_net

volumes:
  postgres_data:

networks:
  teldrive_net:
```

With this file the `config.toml` database host is `postgres_db`:

```toml
[db]
data-source = "postgres://teldrive:secret@postgres_db/postgres?sslmode=disable"
```

**Deploy the latest local changes:**

```bash
# Pull latest code
git pull

# Build the image from source and start all services
docker compose up -d --build
```

To just pull the published image without rebuilding from source:

```bash
docker compose up -d
```

---

## Configuration Reference

Below are the most important settings. See `config.sample.toml` / `config.sample.yml` for the full list with defaults.

| Section | Key | Default | Description |
|---|---|---|---|
| `[db]` | `data-source` | *(required)* | PostgreSQL connection string |
| `[jwt]` | `secret` | *(required)* | JWT signing secret (use a random 32+ char string) |
| `[jwt]` | `session-time` | `30d` | How long a login session stays valid |
| `[jwt]` | `allowed-users` | `[]` | Restrict login to specific Telegram usernames (empty = allow all) |
| `[server]` | `port` | `8080` | HTTP port |
| `[tg]` | `app-id` | `2496` | Telegram API app ID |
| `[tg]` | `app-hash` | — | Telegram API app hash |
| `[tg]` | `pool-size` | `8` | Number of concurrent Telegram connections |
| `[tg]` | `auto-channel-create` | `true` | Automatically create a new storage channel when the current one is full |
| `[tg]` | `channel-limit` | `500000` | Maximum number of parts per channel before a new one is created |
| `[tg]` | `proxy` | `""` | SOCKS5/HTTP proxy URL for Telegram connections |
| `[tg.uploads]` | `encryption-key` | `""` | Encrypt file parts (leave empty to disable) |
| `[tg.uploads]` | `threads` | `8` | Parallel upload threads |
| `[tg.uploads]` | `retention` | `7d` | How long incomplete uploads are kept before cleanup |
| `[tg.stream]` | `buffers` | `8` | Download buffer count for streaming |
| `[cache]` | `max-size` | `10485760` | In-memory cache size in bytes |
| `[cache]` | `redis-addr` | `""` | Redis address for distributed caching (optional) |
| `[cronjobs]` | `enable` | `true` | Enable background maintenance jobs |

---

## Browsing & Filtering Channel Files

Teldrive stores every uploaded file with its originating Telegram **channel ID**. You can filter the file listing to a specific channel using the `channelId` query parameter.

### Listing your channels via the REST API

```
GET /api/users/channels
```

Returns all channels associated with your account — both channels configured as TelDrive storage targets (from the database) and channels where your Telegram account has admin rights (from peer storage). Each entry includes a `selected` flag that tells you which channel is the **current active storage target**.

**Example response:**

```json
[
  { "channelName": "Archive 2024", "channelId": 111, "selected": false },
  { "channelName": "Main Storage",  "channelId": 222, "selected": true  },
  { "channelName": "Telegram-only", "channelId": 333, "selected": false }
]
```

| Field | Type | Description |
|---|---|---|
| `channelId` | `int64` | Unique Telegram channel identifier |
| `channelName` | `string` | Human-readable channel name |
| `selected` | `boolean` | `true` if this channel is the currently active TelDrive storage channel |

```bash
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8080/api/users/channels"
```

### Filtering files by channel

```
GET /api/files?channelId=<channel_id>
```

| Parameter | Type | Description |
|---|---|---|
| `channelId` | `int64` | Only return files stored in this Telegram channel |

**Examples:**

```bash
# List all files in channel 1234567890 (first page, 500 items)
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8080/api/files?channelId=1234567890"

# Search for "report" inside a specific channel
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8080/api/files?channelId=1234567890&operation=find&query=report"

# Browse a sub-folder by path inside a specific channel
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8080/api/files?channelId=1234567890&path=/documents"
```

`channelId` can be combined with any other supported parameter (`path`, `parentId`, `query`, `operation`, `category`, `sort`, `order`, `page`, `limit`, etc.).

### Finding your channel ID

1. Open Teldrive's web UI → **Settings → Channels**.
2. Your channels are listed with their IDs. The currently active storage channel is marked as selected.

Alternatively, call `GET /api/users/channels` (see above) — the entry with `"selected": true` is your current storage channel.

You can also forward any message from the channel to [@userinfobot](https://t.me/userinfobot) on Telegram.

---

## Mount with Rclone / WebDAV

Teldrive exposes a WebDAV-compatible API, so any WebDAV client (Rclone, Cyberduck, Mountain Duck, Windows Explorer, macOS Finder, …) can mount it as a drive.

### Rclone configuration

Add the following to your Rclone config (`~/.config/rclone/rclone.conf`):

```ini
[teldrive]
type = webdav
url = http://localhost:8080/webdav
vendor = other
user = <your-telegram-username>
pass = <rclone-obscured-jwt-token>
```

Get an API token from **Settings → API Keys** in the web UI, then obscure it:

```bash
rclone obscure <your-jwt-token>
```

### Mount a full drive

```bash
rclone mount teldrive: /mnt/teldrive --daemon
```

### Mount a single channel

Pass the `channelId` query parameter through the WebDAV URL or use rclone's `--webdav-url` flag (exact syntax depends on your rclone version):

```bash
# Via a URL-based config entry
rclone config create teldrive-mychannel webdav \
  url "http://localhost:8080/webdav?channelId=1234567890" \
  vendor other user myuser pass <obscured-token>

rclone mount teldrive-mychannel: /mnt/mychannel --daemon
```

---

## Manual / Development Setup

### Prerequisites

- Go 1.22+
- [Task](https://taskfile.dev) (`brew install go-task` / `go install github.com/go-task/task/v3/cmd/task@latest`)
- PostgreSQL 15+

### Steps

```bash
# 1. Clone
git clone https://github.com/tgdrive/teldrive.git
cd teldrive

# 2. Install Go dependencies
task deps

# 3. Download pre-built frontend assets
task ui

# 4. Generate API code from the local OpenAPI spec
task gen

# 5. Build the binary
task server
# binary is written to bin/teldrive

# 6. Copy and edit the config
cp config.sample.toml config.toml
#  → fill in db.data-source, jwt.secret, tg.app-id, tg.app-hash

# 7. Run
task run
# or: ./bin/teldrive run
```

Open `http://localhost:8080`.

### Re-generating the API after spec changes

The `internal/api/` directory is **generated** (gitignored). After editing `openapi/openapi.json` regenerate with:

```bash
task gen
# equivalent: GOFLAGS='' go run github.com/ogen-go/ogen/cmd/ogen@v1.18.0 \
#   --clean --package api --target internal/api openapi/openapi.json
```

---

## Best Practices

### Do

- **Respect Telegram API limits.** Stay within the rate limits to avoid account bans.
- **Use your own API credentials.** Register your own `app_id` / `app_hash` at [my.telegram.org/apps](https://my.telegram.org/apps) for production use.
- **Set a strong JWT secret.** Use at least 32 random characters (`openssl rand -hex 32`).
- **Restrict access** with `jwt.allowed-users` if this instance is shared.
- **Back up your database.** Use the provided `docker/compose/db-backup.yml` Compose snippet to schedule automatic Postgres backups.

### Don't

- **Don't hoard data** in violation of Telegram's Terms of Service.
- **Don't share your JWT secret** or API credentials publicly.
- **Don't run multiple instances** without a shared Redis / advisory-lock setup.

---

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow and pull-request guidelines.

---

## Donate

If you find Teldrive useful, a small contribution is appreciated → [PayPal](https://paypal.me/redux234).

---

## Star History

<a href="https://www.star-history.com/#tgdrive/teldrive&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=tgdrive/teldrive&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=tgdrive/teldrive&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=tgdrive/teldrive&type=Date" />
 </picture>
</a>

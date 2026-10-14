# stackwire

Self-hosted apps on Docker, installed **and connected to each other** with one command.

App stores like Umbrel or CasaOS start the containers and leave the rest to you: creating logins, copying API keys between apps, setting up folders. stackwire does that part too.

Status: early. The first stack is media (Jellyfin, Sonarr, Radarr, Prowlarr, qBittorrent).

## What `stackwire up` does

1. Generates `docker-compose.yml`, a `.env` with random secrets, and the folder layout.
2. Starts the containers and waits until each one answers.
3. Configures them through their APIs:
   - one admin login for every app
   - Sonarr and Radarr: media folder, qBittorrent as download client, Jellyfin rescan after each import
   - Prowlarr: Sonarr and Radarr registered, so indexers sync to both
   - Jellyfin: setup wizard completed, Movies and Shows libraries added
4. With `--domain`: starts Caddy with HTTPS on `https://<app>.<domain>`.

Every step checks the current state first. Running `up` or `wire` again changes nothing that is already set.

The only thing left for you is adding your indexers in Prowlarr.

## Usage

Requires Docker with the compose plugin (Docker Engine on Linux, Docker Desktop on Windows and macOS).

At home, apps reachable by port:

```bash
stackwire init --dir ~/media
stackwire up --dir ~/media
```

On a server with a domain (`*.media.example.com` must point to it, ports 80 and 443 open):

```bash
stackwire init --dir /opt/media --domain media.example.com --email you@example.com
stackwire up --dir /opt/media
```

Other commands: `apps`, `render`, `wire`, `status`, `down`.

## Web interface

```bash
stackwire serve --dir ~/media
```

Open http://localhost:8900 and sign in as `admin` with `ADMIN_PASSWORD` from the stack's `.env`. In a new directory `serve` creates an empty stack, so you can do everything in the browser:

- install and remove apps, then apply the changes with one click and watch the progress
- see which apps run, open them, restart them, read their logs
- change domain, timezone, data folder and ports
- look up the shared login, stop everything

The page only listens on this machine by default. To reach it from other devices on your network, start it with `--addr 0.0.0.0:8900`. The connection is not encrypted, so do not expose that port to the internet; on a remote server use an SSH tunnel (`ssh -L 8900:localhost:8900 you@server`).

## Files in the stack directory

| File | Purpose |
|---|---|
| `stackwire.yaml` | Your settings: apps, domain, folders, timezone, port overrides. Edit this. |
| `.env` | Generated secrets, including `ADMIN_PASSWORD`. Keep private. |
| `docker-compose.yml`, `caddy/` | Generated. Overwritten on every run. |
| `config/<app>/` | Each app's own configuration. |
| `data/` | `media/{movies,tv}` and `torrents/{movies,tv}` on one mount, so imports are hardlinks instead of copies. |

If a port is taken, override it in `stackwire.yaml`:

```yaml
ports:
  qbittorrent: 8081
```

## Adding an app

Apps are manifests in [internal/catalog/apps](internal/catalog/apps). A manifest declares the image, port, volumes, secrets and which driver configures the app. Drivers live in [internal/wire](internal/wire).

## Build

```bash
go build -o bin/stackwire ./cmd/stackwire
go test ./...
```

## Note

stackwire installs and configures software. What you download with it is your responsibility.

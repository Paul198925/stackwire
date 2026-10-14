# stackwire

Self-hosted apps on Docker, installed **and connected to each other** with one command.

App stores like Umbrel or CasaOS start the containers and leave the rest to you: creating logins, copying API keys between apps, setting up folders. stackwire does that part too.

Status: early. The first stack is media.

## Apps

| App | What it is | What stackwire sets up |
|---|---|---|
| Jellyfin | Media server | Admin account, libraries for movies, shows and (with Lidarr) music |
| Sonarr, Radarr, Lidarr | Managers for shows, movies, music | Login, media folder, qBittorrent and SABnzbd as download clients, Jellyfin rescan after each import |
| Prowlarr | Indexer manager | Login, Sonarr, Radarr and Lidarr registered so indexers sync to them |
| qBittorrent | Torrent download client | Login, download folders |
| SABnzbd | Usenet download client | Login, folders, a category per media type, connected to Sonarr, Radarr and Lidarr; you add your usenet provider |
| Bazarr | Subtitles | Login, connections to Sonarr and Radarr |
| Seerr | Request page for family and friends | Sign-in through Jellyfin, libraries, Sonarr and Radarr as targets for requests |
| FlareSolverr | Helper for indexers behind Cloudflare | Registered in Prowlarr under the tag `flaresolverr`; not reachable from outside |
| Immich | Photo and video backup with phone apps | Admin account (sign-in by email, shown after setup), database, cache and machine learning |
| Paperless-ngx | Document archive with text recognition | Admin account, database and queue; files dropped into `documents/inbox` in the data folder are read in |

Apps that need another one bring it along: installing Seerr also installs Jellyfin.

Apps that need their own database or cache get them as extra containers, which only the app can reach. Databases live in Docker volumes (`docker volume ls`), everything else in the stack folder.

## What `stackwire up` does

1. Generates `docker-compose.yml`, a `.env` with random secrets, and the folder layout.
2. Starts the containers and waits until each one answers.
3. Configures them through their APIs:
   - one admin login for every app
   - the connections between them, as listed under [Apps](#apps)
4. With `--domain`: starts Caddy with HTTPS on `https://<app>.<domain>`.

Every step checks the current state first. Running `up` or `wire` again changes nothing that is already set.

What is left for you are the choices only you can make: your indexers in Prowlarr and, with Bazarr, your subtitle languages. `up` lists them at the end.

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

- see at a glance what runs and what it uses: processor, memory and disk space, per app and in total, plus numbers from the apps themselves (series, movies, missing items, indexers, downloads)
- install and remove apps, then apply the changes with one click and watch the progress
- see which apps run, open them, restart them, read their logs
- change domain, timezone, data folder and ports, and settings of single apps such as the document language of Paperless-ngx
- look up the shared login, stop everything

The page only listens on this machine by default. To reach it from other devices on your network, start it with `--addr 0.0.0.0:8900`. That connection is not encrypted, so do not expose the port to the internet.

With a domain, the page is served over HTTPS at `https://stackwire.<domain>`, next to the apps. On a fresh server this is all it takes:

```bash
stackwire init --dir /opt/media --domain media.example.com --email you@example.com --apps none
stackwire serve --dir /opt/media
```

`serve` starts the proxy by itself, so the page is reachable before any app is installed. It has to keep running (as a service, for example) and must use the default port 8900.

On Linux the proxy container reaches the page through the Docker bridge. A host firewall that blocks traffic from containers to the host (ufw does by default) has to allow port 8900 from the Docker networks.

## Files in the stack directory

| File | Purpose |
|---|---|
| `stackwire.yaml` | Your settings: apps, domain, folders, timezone, port overrides. Edit this. |
| `.env` | Generated secrets, including `ADMIN_PASSWORD`. Keep private. |
| `docker-compose.yml`, `caddy/` | Generated. Overwritten on every run. |
| `config/<app>/` | Each app's own configuration. |
| `data/` | `media/{movies,tv}` and `torrents/{movies,tv}` on one mount, so imports are hardlinks instead of copies. |

Some apps have settings of their own, such as the document language of Paperless-ngx. Change them in the web interface (Settings on the app's card) or in `stackwire.yaml`:

```yaml
appSettings:
  paperless:
    ocrLanguage: deu+eng
```

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

# stackwire

Self-hosted apps on Docker, installed **and connected to each other** with one command.

App stores like Umbrel or CasaOS start the containers and leave the rest to you: creating logins, copying API keys between apps, setting up folders. stackwire does that part too.

Status: early. The first stack is media.

## Install

On a Linux server (Debian, Ubuntu and others with systemd):

```bash
curl -fsSL https://raw.githubusercontent.com/Paul198925/stackwire/main/install.sh | sudo sh
```

The script installs Docker if it is missing, puts `stackwire` in `/usr/local/bin` and runs its web interface as a service on port 8900. At the end it prints a link like `http://192.168.1.20:8900/?setup=…`. Open it and the setup walks you through the rest:

1. your login, the same for every app
2. how you reach the apps: by port on your network, or under your own domain with HTTPS
3. which apps you want
4. installing them, with the progress shown live

The code in the link is needed once, so nobody else on the network can claim the server before you do. Running the script again updates stackwire and keeps your stack. Options: `STACKWIRE_DIR` (default `/opt/stackwire`), `STACKWIRE_VERSION`, `STACKWIRE_PORT`.

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
| Navidrome | Music streaming, also to Subsonic apps on your phone | Account `admin` with the stack's password, reads the music folder |
| Audiobookshelf | Audiobooks and podcasts with phone apps | Root account, libraries for audiobooks and podcasts |
| Syncthing | Folder sync between your devices | Login, new shared folders land in `sync` in the data folder |
| Linkding | Bookmarks | Admin account |
| Stirling-PDF | PDF tools | Login with the admin account |
| Immich | Photo and video backup with phone apps | Admin account (sign-in by email, shown after setup), database, cache and machine learning |
| Paperless-ngx | Document archive with text recognition | Admin account, database and queue; files dropped into `documents/inbox` in the data folder are read in |

Apps that need another one bring it along: installing Seerr also installs Jellyfin.

Each stack gets its own name, taken from its folder (`name` in `stackwire.yaml`). Containers belong to a stack by that name, so two stacks on one machine need different names; stackwire refuses to touch containers that another folder started.

Apps that need their own database or cache get them as extra containers, which only the app can reach. Databases live in Docker volumes (`docker volume ls`), everything else in the stack folder.

## What `stackwire up` does

1. Checks that the ports it publishes are free. If another program holds one, it stops before anything starts and suggests a free port for `ports` in `stackwire.yaml`.
2. Generates `docker-compose.yml`, a `.env` with random secrets, and the folder layout.
3. Starts the containers and waits until each one answers.
4. Configures them through their APIs:
   - one admin login for every app
   - the connections between them, as listed under [Apps](#apps)
5. With `--domain`: starts Caddy with HTTPS on `https://<app>.<domain>`.

Every step checks the current state first. Running `up` or `wire` again changes nothing that is already set.

What is left for you are the choices only you can make: your indexers in Prowlarr and, with Bazarr, your subtitle languages. `up` lists them at the end.

## Command line

Everything the web interface does also works from the command line. It needs Docker with the compose plugin (Docker Engine on Linux, Docker Desktop on Windows and macOS). Binaries for Linux, macOS and Windows are on the [releases page](https://github.com/Paul198925/stackwire/releases).

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

In a new directory `serve` creates an empty stack and prints a setup link; the first visit walks through the same setup as after the [install](#install). Later visits sign in as `admin` with the password chosen there (also in the stack's `.env` as `ADMIN_PASSWORD`).

The page looks like a home screen: your apps as icons to open, and an app store to add more. You can:

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
| `data/` | `media/`, `torrents/`, `usenet/`, `photos/`, `documents/`, `sync/`, unless they are on [other storage](#storage). Media and downloads share one mount, so imports are hardlinks instead of copies. |

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

## Storage

Everything starts in the data folder. Under Settings > Storage you can put parts of it elsewhere: on a USB disk or a second drive of the server, or on a share of your NAS (SMB or NFS). stackwire lists the drives it finds and checks a storage before you use it: it mounts it the way the apps will and writes a test file as their user, so a wrong password, a missing share or a drive the apps may not write to shows up right away.

Each area can live in its own place:

| Area | Folders | Used by |
|---|---|---|
| Media | `media/` | Jellyfin, Sonarr, Radarr, Lidarr, Navidrome, Audiobookshelf |
| Downloads | `torrents/`, `usenet/` | qBittorrent, SABnzbd |
| Photos | `photos/` | Immich |
| Documents | `documents/` | Paperless-ngx |
| Synced folders | `sync/` | Syncthing |

Downloads go with the media unless you give them a place of their own. Together, Sonarr and Radarr move finished downloads into the library instantly (a hardlink). Apart, for example downloads on a fast local disk and the library on the NAS, they copy them; torrents keep seeding from the downloads folder until they are removed.

Shares are mounted by Docker, so nothing needs to be set up on the server, and their passwords stay in `.env`. If a drive is not connected, stackwire refuses to start the apps instead of quietly filling the system disk where the drive should be. Moving an area does not move the files that are already there.

```yaml
storage:
  - id: nas
    type: smb            # or nfs, or folder with path: /mnt/usb
    server: 192.168.1.10
    share: media
    user: paul           # password: STORAGE_NAS_PASSWORD in .env
areas:
  media: {storage: nas, folder: stackwire}
  downloads: {storage: data}   # the data folder
```

## Adding an app

Apps are manifests in [internal/catalog/apps](internal/catalog/apps). A manifest declares the image, port, volumes, secrets and which driver configures the app. Drivers live in [internal/wire](internal/wire).

## Build

```bash
go build -o bin/stackwire ./cmd/stackwire
go test ./...
```

A release is published by pushing a tag like `v0.1.0`: the `release` workflow builds the binaries and attaches them with their checksums. `install.sh` installs the latest release.

## Note

stackwire installs and configures software. What you download with it is your responsibility.

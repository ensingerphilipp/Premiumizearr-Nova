# Premiumizearr-Nova
## Build 1.5.5

[![Build](https://github.com/ensingerphilipp/premiumizearr-nova/actions/workflows/build.yml/badge.svg)](https://github.com/ensingerphilipp/premiumizearr-nova/actions/workflows/build.yml)

*BUGFIX Release:* 
* Fix transfer folder not being applied to new uploads (multipart field)
* Fix crash when the web UI queries blackhole status before startup completes
* Fix SimultaneousDownloads limit being miscounted by the download count

*NEW: Pause blackhole submissions when the Premiumize fair-use quota is exhausted* ✅

## Enjoying so far? Im running on ☕
<a href="https://ko-fi.com/K3K819CODS"><img src="https://uploads-ssl.webflow.com/5c14e387dab576fe667689cf/5cbed8a4ae2b88347c06c923_BuyMeACoffee_blue-p-500.png" width="250px"></a>

## Overview 📝
Continuation and Improvement of the Premiumizearr Arr* Bridge Download Client compatible with Sonarr and Radarr.

This project is based on code from [Jackdallas' Premiumizearr](https://github.com/jackdallas/premiumizearr). 
It aims to improve its function and fix bugs as the Original Repo has gone stale and does not respond to issues and pull requests.
The code has been reused with modifications to suit my own use case.

* Complete Downloader Revamp (Fix EOF DataStream Error, Improve Speed, Graceful Resumable Downloads, Limitable Downloadspeed)
* Added .torrent support, Add Support for Single Files, Switch from ZIP API to Direct Download
* Fix Gui Row Ordering Bugs, Improve API
* Updated base images and dependencies
* Added Lidarr Support @FuJa0815
* Added Transfer-Only-Mode
* Several minor improvements and fixes

Next Steps:
* Fix Sonarr Connection Bugs

## Features

- Monitor blackhole directory to push `.magnet`, `.torrent`  and `.nzb` to Premiumize.me
- Monitor and download Premiumize.me transfers (web ui on default port 8182)
- Mark transfers as failed in Radarr & Sonarr
- Optional direct download-client integration for Sonarr, Radarr and Lidarr,
  including progress, import paths and removal, without a blackhole mount.

New blackhole submissions pause automatically when the Premiumize fair-use quota is exhausted and no booster points are available. Existing cloud transfers are not affected. Source files remain in the blackhole directory and are processed automatically after quota replenishment or booster activation.

## Install

### Docker
It is highly recommended to use the amd64 and arm64 docker images.

1. First create data, blackhole, downloads folders that will be mounted into the docker container.
2. Make sure all Folders and are writeable and readable by UID 1000 and GID 1000 (Or the UID GID you want to set and use -> see env variables in Docker Run)
3. Create or choose a network for the docker container to run in - **Important, if you have connection problems try explicitly disabling ipv6 for your docker network or docker daemon as ipv6 might break some things - see https://github.com/ensingerphilipp/Premiumizearr-Nova/issues/12**
5. Adapt the command below with the correct folders and network to run
6. Do not use sudo!


[Docker images are listed here](https://github.com/ensingerphilipp/premiumizearr-nova/pkgs/container/premiumizearr-nova)

```cmd
docker run -d --name premiumizearr \
  --network=compose_default \
  -v /mount/premiumize/data:/data \
  -v /mount/premiumize/blackhole:/blackhole \
  -v /mount/premiumize/downloads:/downloads \
  -e PGID=1000 \
  -e PUID=1000 \
  -p 8182:8182 \
  --restart unless-stopped \
  ghcr.io/ensingerphilipp/premiumizearr-nova:latest
```

If you wish to increase logging (which you'll be asked to do if you submit an issue) you can add `-e PREMIUMIZEARR_LOG_LEVEL=trace` to the command

> Note: The /data mount is where the `config.yaml` and log files are kept
> You might need to run the docker command with UID GID 1000 on the host as well
> If you absolutely can not use docker, scroll to the bottom of the README for unsupported Installation-Methods, they are automatically built and untested.

## First Setup

### Premiumizearrd

Running for the first time the server will start on `http://0.0.0.0:8182`

If you already use this binding for something else you can edit them in the `config.yaml`

> WARNING: This app exposes api keys in the ui and does not have authentication, it is strongly recommended you put it behind a reverse proxy with auth and set the host to `127.0.0.1` to hide the app from the web.

### Sonarr/Radarr

#### Direct download clients (no blackhole)

1. Mount a writable downloads directory into Premiumizearr and make it visible
   at the **same path** to each *arr container (for example `/downloads`). The
   completed media is placed in `/downloads/direct/<job-id>/`. No blackhole
   mount is required. Keep Transfer-Only-Mode disabled.
2. Start Premiumizearr once and copy **Direct *arr client key** from its Config
   tab (or `DirectClientAPIKey` in `config.yaml`). A random key is generated at
   first startup. The compatibility APIs share the web server port.
3. For torrents and magnets, add a **qBittorrent** download client in each
   *arr: Host = the Premiumizearr host; Port = `8182` (or the configured port);
   URL Base = `/qbit`; Username = `premiumizearr`; Password = the direct client
   key. Set a distinct category for each *arr (for example `sonarr`, `radarr`,
   `lidarr`).
4. For NZBs, add a **SABnzbd** download client: the same host/port, URL Base
   = `/sab`, API Key = the direct client key, and a distinct category for each
   *arr. The preconfigured category names include `sonarr`, `radarr`, `lidarr`,
   `tv`, `movies`, `music`, and the names of configured *arr instances.
5. Use *arr's **Test** button and submit a release. Premiumizearr keeps direct
   jobs in `<config directory>/direct-jobs` across restarts. A job appears
   completed only once its files have been fully downloaded locally. *arr can
   then import from the reported path and remove the job through the client.

The web UI still exposes account keys without authentication. Keep the service
on a trusted network or behind an authenticated reverse proxy. Its qBittorrent
and SABnzbd endpoints require the direct client key.

For automatic failure handling of torrents, configure each *arr instance in
Premiumizearr's Config tab with its URL, type and API key. The qBittorrent
`error` state is treated as a warning by *arr, so Premiumizearr explicitly
marks the grabbed history record matching the torrent's download ID as failed.
Failed reports are retried and successful reports are retained across restarts.
NZB failures are handled through SABnzbd history without this extra setup.

#### Existing blackhole clients

- Go to your Arr's `Download Client` settings page
- Add a new Torrent Blackhole client, set the `Torrent Folder` to the previously set `BlackholeDirectory` location, set the `Watch Folder` to the previously set `DownloadsDirectory` location
- Add a new Usenet Blackhole client, set the `Nzb Folder` to the previously set `BlackholeDirectory` location, set the `Watch Folder` to the previously set `DownloadsDirectory` location
- Also: Dont forget to press the "Save" Button when editing settings inside premiumizearr-nova web-ui

### Reverse Proxy

Premiumizearr does not have authentication built in so it's strongly recommended you use a reverse proxy

#### Nginx

```nginx
location /premiumizearr/ {
    proxy_pass http://127.0.0.1:8182/;
    proxy_set_header Host $proxy_host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_redirect off;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $http_connection;
}

# The built-in *arr compat APIs are mounted at fixed root paths outside the
# WebRoot (/qbit and /sab). To reach them through this proxy, map the proxied
# paths to those root paths and use the mapped URL Base in each *arr:
# /premiumizearr/qbit (qBittorrent) and /premiumizearr/sab (SABnzbd).
location /premiumizearr/qbit/ {
    proxy_pass http://127.0.0.1:8182/qbit/;
    proxy_set_header Host $proxy_host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
}

location /premiumizearr/sab/ {
    proxy_pass http://127.0.0.1:8182/sab/;
    proxy_set_header Host $proxy_host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
}
```

> The direct *arr endpoints always live outside the WebRoot at the fixed root
> paths `/qbit` and `/sab` (WebRoots under those prefixes are rejected), so
> *arr clients on the same host can also point straight at port `8182` with
> URL Base `/qbit` or `/sab` and bypass the reverse proxy entirely.

## License

This project is licensed under the **GNU General Public License v3.0** - see the [LICENSE](./LICENSE) file for details.

### Original Code

This project reuses code from [Jackdallas' Premiumizearr](https://github.com/jackdallas/premiumizearr), which is licensed under the **GNU General Public License v3.0**.

### Modifications

The following changes have been made to the original code:
- See Commit History
  
All modifications to the original code are also licensed under the same license, i.e., **GNU GPL v3**.

## Unsupported Installation Methods

Those methods should work but are discouraged and receive no support, you should use docker!

[Grab the latest release artifact links here](https://github.com/ensingerphilipp/premiumizearr-nova/releases/)

### Binary

#### System Install

```cli
wget https://github.com/ensingerphilipp/premiumizearr-nova/releases/download/x.x.x/Premiumizearr_x.x.x_linux_amd64.tar.gz
tar xf Premiumizearr_x.x.x.x_linux_amd64.tar.gz
cd Premiumizearr_x.x.x.x_linux_amd64
sudo mkdir /opt/premiumizearrd/
sudo cp -r premiumizearrd static/ /opt/premiumizearrd/
sudo cp premiumizearrd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable premiumizearrd.service
sudo systemctl start premiumizearrd.service
```

#### User Install

```cli
wget https://github.com/ensingerphilipp/premiumizearr-nova/releases/download/x.x.x/Premiumizearr_x.x.x_linux_amd64.tar.gz
tar xf Premiumizearr_x.x.x.x_linux_amd64.tar.gz
cd Premiumizearr_x.x.x.x_linux_amd64
mkdir -p ~/.local/bin/
cp -r premiumizearrd static/ ~/.local/bin/
echo -e "export PATH=~/.local/bin/:$PATH" >> ~/.bashrc 
source ~/.bashrc
```

You're now able to run the daemon from anywhere just by typing `premiumizearrd`

### deb file

```cmd
wget https://github.com/ensingerphilipp/premiumizearr-nova/releases/download/x.x.x/premiumizearr_x.x.x._linux_amd64.deb
sudo dpkg -i premiumizearr_x.x.x.x_linux_amd64.deb
```

### Windows Installation

1. [Download the Windows Release here](https://github.com/ensingerphilipp/premiumizearr-nova/releases/)
2. Follow the Setup Instructions and try to match them to Windows Commandline where possible

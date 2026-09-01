# spotify-connect-streamer-ng

A single Go binary that turns Spotify Connect audio into a plain HTTP MP3
stream — a native-binary port of the
[spotify-connect-streamer](https://github.com/BenjiThatFoxGuy/spotify-connect-streamer)
Docker/shell POC. Same pipeline, no shell script, no container required
(though one is still available, see below).

```
Spotify App -> librespot (Connect device) -> pv (rate limiter) -> ffmpeg (MP3 encode) -> icecast -> HTTP stream
```

librespot presents itself as a Spotify Connect device and writes raw PCM to
a pipe instead of a sound card. `pv` throttles that pipe to real-time
playback speed (176400 B/s = 44.1kHz * 2ch * 2 bytes) so librespot can't
download faster than playback — which is what causes Spotify to think a
track finished early and skip ahead. `ffmpeg` reads the paced PCM and
encodes it to MP3 live, pushing it to an icecast mount as the source.

This binary replaces `entrypoint.sh` from the original project: it manages
the librespot and ffmpeg child processes directly (no `bash`, no `pv`
subshell juggling), restarts the pipeline if it dies, and handles
first-time device-auth pairing automatically.

## Install

You still need `librespot`, `ffmpeg`, and `pv` on the machine — this binary
just orchestrates them. You also need somewhere to stream *to*: an icecast
server (see `docker-compose.yml` for a quick one, or point at any existing
icecast mount).

### macOS

```bash
brew install ffmpeg pv

# librespot isn't in homebrew; build it from source (needs Rust: `brew install rust`)
cargo install librespot --features "alsa-backend"
# or, to track upstream fixes / build with only the features you need:
git clone https://github.com/librespot-org/librespot.git
cd librespot
cargo build --release --no-default-features --features "rustls-tls-webpki-roots with-libmdns"
# binary lands at target/release/librespot
```

Then grab a `spotify-connect-streamer-ng` binary from the
[releases page](../../releases) (darwin-amd64 or darwin-arm64), or build it
yourself:

```bash
make darwin-arm64   # or darwin-amd64
```

### Linux

Install `ffmpeg` and `pv` from your distro's package manager, and build
`librespot` the same way as above (or grab a prebuilt one). A prebuilt
`linux-amd64` binary of this orchestrator is on the
[releases page](../../releases).

### Docker

A `Dockerfile` and `docker-compose.yml` are included that bake librespot,
ffmpeg, pv, and this binary into one image, plus an icecast service to
stream to. This is a drop-in replacement for the original project's
compose stack — same environment variable names.

```bash
cp .env.example .env
docker compose up --build
```

A prebuilt image is also published to
`ghcr.io/benjithatfoxguy/spotify-connect-streamer-ng` on tagged releases.

## Usage

```bash
spotify-connect-streamer-ng \
  --name "Living Room" \
  --icecast-url "icecast://source:hackme@localhost:8000/stream.mp3"
```

Then:

- The Connect device shows up in the Spotify app's device picker (on the
  LAN, for the default zeroconf auth mode).
- Cast something to it.
- Open `http://<icecast-host>:8000/stream.mp3` in a browser, VLC, mpv, or
  any HTTP audio player to hear it.

## Configuration

Every option is available as a flag or an environment variable. **When
both are set, the environment variable wins** — this keeps the binary
drop-in compatible with the original Docker Compose setup, which only ever
used env vars.

| Flag                     | Env var             | Default                        | Meaning                                                        |
|--------------------------|----------------------|---------------------------------|------------------------------------------------------------------|
| `--name`                 | `DEVICE_NAME`         | `Stream Output`                 | Name shown in the Spotify Connect device picker                  |
| `--device-type`          | `DEVICE_TYPE`         | `speaker`                       | Device type shown in Spotify                                     |
| `--backend`              | `BACKEND`             | `pipe-pv`                       | Audio pipeline backend (only `pipe-pv` is implemented; anything else falls back to it with a warning) |
| `--bitrate`               | `MP3_BITRATE`         | `320k`                          | MP3 encoding bitrate                                              |
| `--icecast-url`          | `ICECAST_URL`         | *(required)*                    | Full icecast destination URL, e.g. `icecast://source:pass@host:8000/stream.mp3` |
| `--cache-dir`             | `CACHE_DIR`           | `~/.cache/spotify-streamer`     | librespot cache / credentials directory                          |
| `--auth-mode`             | `AUTH_MODE`           | `zeroconf`                      | `zeroconf`, `device-auth`, or `password`                         |
| `--spotify-username`      | `SPOTIFY_USERNAME`    | *(unset)*                       | Only used for `auth-mode=password`                                |
| `--spotify-password`      | `SPOTIFY_PASSWORD`    | *(unset)*                       | Only used for `auth-mode=password`                                |
| `--librespot-path`        | `LIBRESPOT_PATH`      | `librespot` (searched on `PATH`) | Path to the librespot binary                                      |
| `--ffmpeg-path`           | `FFMPEG_PATH`         | `ffmpeg` (searched on `PATH`)   | Path to the ffmpeg binary                                         |
| `--pv-path`                | `PV_PATH`             | `pv` (searched on `PATH`)       | Path to the pv binary                                             |
| `--librespot-extra-args`  | `LIBRESPOT_EXTRA_ARGS` | *(empty)*                      | Extra flags passed straight through to librespot                  |

For Docker Compose compatibility, `ICECAST_URL` can also be built
automatically from the discrete pieces the original project's compose file
used — `ICECAST_HOST`, `ICECAST_PORT` (default `8000`),
`ICECAST_SOURCE_PASSWORD`, and `MOUNT_POINT` (default `stream.mp3`) — if
`ICECAST_URL`/`--icecast-url` isn't set directly.

### Auth modes

- **zeroconf (default, safest)** — librespot advertises itself via mDNS
  and only shows up as a Connect target for Spotify apps on the same LAN.
  No credentials stored anywhere.
- **device-auth** — pair once at [spotify.com/pair](https://spotify.com/pair).
  On first run with no cached credentials, the binary runs librespot
  standalone to display a pairing code (visible in the logs), waits up to
  120s for you to complete pairing, then starts the real pipeline. Once
  `credentials.json` exists in `--cache-dir`, subsequent starts skip
  pairing.
- **password (deprecated by Spotify)** — set `--spotify-username` /
  `--spotify-password` (or the env equivalents). Falls back to zeroconf if
  either is missing.

## Behavior notes

- If the pipeline (librespot | pv | ffmpeg) dies for any reason, it's
  restarted automatically after 3 seconds — the process keeps running
  rather than exiting.
- SIGINT/SIGTERM triggers a graceful shutdown: all child processes are
  signaled and the binary waits for them to exit before returning.
- This is a POC port: only the `pipe-pv` backend from the original shell
  script is implemented (no `alsa`/`subprocess`/raw-`pipe` backends, no
  OAuth auth mode). `pipe-pv` was the recommended default in the original
  project and needs no kernel modules or ALSA devices.

## Building from source

```bash
go build -o spotify-connect-streamer-ng .
# or, for a specific platform:
make darwin-arm64   # darwin-amd64, darwin-arm64, linux-amd64
```

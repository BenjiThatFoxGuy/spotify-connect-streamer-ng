# --- Stage 1: build librespot from the dev branch ---
FROM rust:1-bookworm AS librespot-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    pkg-config \
    cmake \
    libasound2-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Shallow-clone the dev branch for latest Connect protocol fixes.
RUN git clone --depth 1 --branch dev https://github.com/librespot-org/librespot.git .

# Patch: allow non-premium accounts. Upstream blocks free accounts
# voluntarily (Spotify doesn't enforce it). Replace exit(1) with a warning.
RUN sed -i 's/error!("librespot does not support {account_type:?} accounts.");/warn!("Account type is {account_type:?}, not premium. Some features may be limited.");/' core/src/session.rs \
 && sed -i '/Please support Spotify and your artists/d' core/src/session.rs \
 && sed -i '/TODO: logout instead of exiting/d' core/src/session.rs \
 && sed -i '/exit(1);/d' core/src/session.rs

# Build with ALSA backend, rustls (no system OpenSSL), and pure-Rust mDNS
# (no Avahi). Pipe and subprocess backends are always included.
RUN cargo build --release \
    --no-default-features \
    --features "alsa-backend rustls-tls-webpki-roots with-libmdns"

# --- Stage 2: build the Go orchestrator ---
FROM golang:1.22-bookworm AS go-builder

WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/spotify-connect-streamer-ng .

# --- Stage 3: slim runtime image ---
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    ca-certificates \
    libasound2 \
    alsa-utils \
    && rm -rf /var/lib/apt/lists/*

COPY --from=librespot-builder /build/target/release/librespot /usr/local/bin/librespot
COPY --from=go-builder /out/spotify-connect-streamer-ng /usr/local/bin/spotify-connect-streamer-ng

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/spotify-connect-streamer-ng"]

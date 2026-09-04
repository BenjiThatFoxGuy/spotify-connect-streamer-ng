// Command spotify-connect-streamer-ng orchestrates librespot and ffmpeg to
// turn a Spotify Connect device into a plain HTTP MP3 stream.
//
// Two output modes:
//   - Embedded (default): serves the stream on a built-in HTTP server.
//     Just open http://localhost:8080/stream.mp3 in any player.
//   - Icecast: pushes to an external icecast server (backward-compatible
//     with the original docker-based project).
//
// The pv rate limiter from the original shell pipeline is replaced with a
// Go-native implementation, eliminating the pv dependency entirely.
//
//	librespot (Connect device, raw PCM) -> Go rate limiter -> ffmpeg (MP3 encode) -> embedded HTTP / icecast
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// pvRateBytesPerSec is the byte rate the rate limiter throttles the pipe to:
// 44100Hz * 2 channels * 2 bytes/sample (16-bit PCM stereo). Pacing the pipe
// at real-time playback speed keeps librespot from downloading faster than
// playback, which is what causes Spotify to think a track finished early and
// skip ahead.
const pvRateBytesPerSec = 176400

const restartDelay = 3 * time.Second

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	DeviceName      string
	DeviceType      string
	Bitrate         string
	ListenAddr      string
	IcecastURL      string
	CacheDir        string
	AuthMode        string
	SpotifyUsername string
	SpotifyPassword string
	LibrespotPath   string
	FfmpegPath      string
	LibrespotExtra  string

	// Docker-compat fallback: if IcecastURL isn't set directly, it's built
	// from these, matching the original entrypoint.sh's URL construction.
	Tunnel  bool
	NoSetup bool

	// Docker-compat fallback: if IcecastURL isn't set directly, it's built
	// from these, matching the original entrypoint.sh's URL construction.
	IcecastHost           string
	IcecastPort           string
	IcecastSourcePassword string
	MountPoint            string
}

// UseEmbedded returns true when the built-in HTTP server should be used
// instead of pushing to icecast.
func (c *Config) UseEmbedded() bool {
	return c.IcecastURL == ""
}

func parseConfig(args []string) (*Config, bool, string, error) {
	fs := flag.NewFlagSet("spotify-connect-streamer-ng", flag.ContinueOnError)

	showVersion := fs.Bool("version", false, "Print version and exit")
	handleEventMode := fs.Bool("handle-event", false, "Internal: act as librespot onevent handler")
	metadataFile := fs.String("metadata-file", "", "Internal: path to metadata JSON file")

	cfg := &Config{}
	fs.StringVar(&cfg.DeviceName, "name", "Stream Output", "Name shown in the Spotify Connect device picker")
	fs.StringVar(&cfg.DeviceType, "device-type", "speaker", "Device type shown in Spotify")
	fs.StringVar(&cfg.Bitrate, "bitrate", "320k", "MP3 encoding bitrate")
	fs.StringVar(&cfg.ListenAddr, "listen", ":8080", "Listen address for the embedded HTTP streaming server")
	fs.StringVar(&cfg.IcecastURL, "icecast-url", "", "Icecast destination URL (if set, disables embedded server)")
	fs.StringVar(&cfg.CacheDir, "cache-dir", "~/.cache/spotify-streamer", "librespot cache/credentials directory")
	fs.StringVar(&cfg.AuthMode, "auth-mode", "zeroconf", "Auth mode: zeroconf, device-auth, or password")
	fs.StringVar(&cfg.SpotifyUsername, "spotify-username", "", "Spotify username (auth-mode=password only)")
	fs.StringVar(&cfg.SpotifyPassword, "spotify-password", "", "Spotify password (auth-mode=password only)")
	fs.StringVar(&cfg.LibrespotPath, "librespot-path", "", "Path to the librespot binary (auto-detected if empty)")
	fs.StringVar(&cfg.FfmpegPath, "ffmpeg-path", "", "Path to the ffmpeg binary (auto-detected if empty)")
	fs.StringVar(&cfg.LibrespotExtra, "librespot-extra-args", "", "Extra args passed straight through to librespot")
	fs.BoolVar(&cfg.Tunnel, "tunnel", true, "Start a Cloudflare quick tunnel for public access (disable with --tunnel=false)")
	fs.BoolVar(&cfg.NoSetup, "no-setup", false, "Disable automatic dependency downloads")

	if err := fs.Parse(args); err != nil {
		return nil, false, "", err
	}

	if *showVersion {
		fmt.Println("spotify-connect-streamer-ng", version)
		return nil, false, "", flag.ErrHelp
	}

	// Onevent handler mode: read env vars and write metadata file, then exit.
	if *handleEventMode {
		return nil, true, *metadataFile, nil
	}

	// Env vars take precedence over flags, for docker compatibility.
	cfg.DeviceName = envOr("DEVICE_NAME", cfg.DeviceName)
	cfg.DeviceType = envOr("DEVICE_TYPE", cfg.DeviceType)
	cfg.Bitrate = envOr("MP3_BITRATE", cfg.Bitrate)
	cfg.ListenAddr = envOr("LISTEN_ADDR", cfg.ListenAddr)
	cfg.IcecastURL = envOr("ICECAST_URL", cfg.IcecastURL)
	cfg.CacheDir = envOr("CACHE_DIR", cfg.CacheDir)
	cfg.AuthMode = envOr("AUTH_MODE", cfg.AuthMode)
	cfg.SpotifyUsername = envOr("SPOTIFY_USERNAME", cfg.SpotifyUsername)
	cfg.SpotifyPassword = envOr("SPOTIFY_PASSWORD", cfg.SpotifyPassword)
	cfg.LibrespotPath = envOr("LIBRESPOT_PATH", cfg.LibrespotPath)
	cfg.FfmpegPath = envOr("FFMPEG_PATH", cfg.FfmpegPath)
	cfg.LibrespotExtra = envOr("LIBRESPOT_EXTRA_ARGS", cfg.LibrespotExtra)

	if envOr("TUNNEL", "") != "" {
		cfg.Tunnel = true
	}
	if envOr("NO_SETUP", "") != "" {
		cfg.NoSetup = true
	}

	cfg.IcecastHost = envOr("ICECAST_HOST", "")
	cfg.IcecastPort = envOr("ICECAST_PORT", "8000")
	cfg.IcecastSourcePassword = envOr("ICECAST_SOURCE_PASSWORD", "")
	cfg.MountPoint = envOr("MOUNT_POINT", "stream.mp3")

	// docker-compose compat: if ICECAST_URL wasn't given directly but the
	// discrete pieces were, build the URL the same way entrypoint.sh did.
	if cfg.IcecastURL == "" && cfg.IcecastHost != "" && cfg.IcecastSourcePassword != "" {
		cfg.IcecastURL = fmt.Sprintf("icecast://source:%s@%s:%s/%s",
			cfg.IcecastSourcePassword, cfg.IcecastHost, cfg.IcecastPort, cfg.MountPoint)
	}

	cfg.CacheDir = expandHome(cfg.CacheDir)

	// Auto-detect binary paths: look next to ourselves first, then PATH.
	if cfg.LibrespotPath == "" {
		cfg.LibrespotPath = findBinary("librespot")
	}
	if cfg.FfmpegPath == "" {
		cfg.FfmpegPath = findBinary("ffmpeg")
	}

	return cfg, false, "", nil
}

// findBinary looks for a binary by name: first in the same directory as our
// own executable (for portable/bundled deploys), then via PATH.
func findBinary(name string) string {
	if runtime.GOOS == "windows" {
		name = name + ".exe"
	}

	// Check next to our own executable.
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Fall back to PATH.
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	// Return bare name and let exec.Command fail with a clear error later.
	return name
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func expandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, path[2:])
	}
	return path
}

// ---------------------------------------------------------------------------
// Rate-limited pipe (replaces pv)
// ---------------------------------------------------------------------------

// rateLimitedCopy copies from src to dst at bytesPerSec, pacing writes so
// librespot doesn't race ahead of real-time playback. This replaces the pv
// binary from the shell pipeline.
func rateLimitedCopy(ctx context.Context, dst io.Writer, src io.Reader, bytesPerSec int) error {
	buf := make([]byte, 8192)
	start := time.Now()
	var totalWritten int64
	var loggedFirst bool

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		n, readErr := src.Read(buf)
		if n > 0 {
			if !loggedFirst {
				loggedFirst = true
				log.Printf("pipeline: first PCM data from librespot (%d bytes)", n)
			}
			// How many bytes should have been written by now?
			elapsed := time.Since(start)
			allowed := int64(elapsed.Seconds() * float64(bytesPerSec))
			ahead := totalWritten + int64(n) - allowed

			if ahead > 0 {
				// We're ahead of real-time. Sleep to let playback catch up.
				sleepDur := time.Duration(float64(ahead) / float64(bytesPerSec) * float64(time.Second))
				select {
				case <-time.After(sleepDur):
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			written, writeErr := dst.Write(buf[:n])
			totalWritten += int64(written)
			if writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// ---------------------------------------------------------------------------
// MP3 stream broadcaster (for embedded HTTP mode)
// ---------------------------------------------------------------------------

// Broadcaster fans out MP3 data from ffmpeg to all connected HTTP clients.
type Broadcaster struct {
	mu        sync.Mutex
	listeners map[int]chan []byte
	nextID    int
	gotData   bool
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		listeners: make(map[int]chan []byte),
	}
}

func (b *Broadcaster) Subscribe() (int, <-chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan []byte, 128)
	b.listeners[id] = ch
	return id, ch
}

func (b *Broadcaster) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.listeners[id]; ok {
		close(ch)
		delete(b.listeners, id)
	}
}

func (b *Broadcaster) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.listeners)
}

// Write implements io.Writer so ffmpeg's stdout can be piped directly here.
func (b *Broadcaster) Write(p []byte) (int, error) {
	b.mu.Lock()
	first := !b.gotData
	if first {
		b.gotData = true
	}
	b.mu.Unlock()
	if first {
		log.Printf("broadcast: first MP3 data received (%d bytes), stream is live", len(p))
	}

	// Copy the data since the caller's buffer will be reused.
	data := make([]byte, len(p))
	copy(data, p)

	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.listeners {
		select {
		case ch <- data:
		default:
			// Slow reader - drop them.
			close(ch)
			delete(b.listeners, id)
			log.Printf("http: dropped slow listener %d", id)
		}
	}
	return len(p), nil
}

// ---------------------------------------------------------------------------
// Embedded HTTP streaming server
// ---------------------------------------------------------------------------

func startHTTPServer(ctx context.Context, addr string, bc *Broadcaster, store *MetadataStore) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/stream.mp3", func(w http.ResponseWriter, r *http.Request) {
		id, ch := bc.Subscribe()
		log.Printf("http: listener %d connected from %s", id, r.RemoteAddr)
		defer func() {
			bc.Unsubscribe(id)
			log.Printf("http: listener %d disconnected", id)
		}()

		// Check if client wants ICY metadata
		wantsIcy := r.Header.Get("Icy-MetaData") == "1"

		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.Header().Set("Connection", "close")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Icy-Name", "Spotify Connect Stream")
		if wantsIcy {
			w.Header().Set("Icy-MetaInt", fmt.Sprintf("%d", icyMetaInt))
		}
		w.WriteHeader(http.StatusOK)

		flusher, canFlush := w.(http.Flusher)
		if canFlush {
			flusher.Flush()
		}

		// Use ICY writer for clients that support it
		var writer interface{ Write([]byte) (int, error) }
		if wantsIcy {
			writer = NewIcyWriter(w, store)
			log.Printf("http: listener %d using ICY metadata", id)
		} else {
			writer = w
		}

		for {
			select {
			case data, ok := <-ch:
				if !ok {
					return
				}
				if _, err := writer.Write(data); err != nil {
					return
				}
				if !wantsIcy && canFlush {
					flusher.Flush()
				}
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			}
		}
	})

	mux.HandleFunc("/now-playing", handleNowPlaying(store))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>Spotify Connect Stream</title></head>
<body style="font-family:sans-serif;max-width:600px;margin:2em auto">
<h1>Spotify Connect Stream</h1>
<p>Stream URL: <code>http://%s/stream.mp3</code></p>
<p>Listeners: %d</p>
<audio controls autoplay src="/stream.mp3"></audio>
<p style="color:#666;font-size:0.9em">spotify-connect-streamer-ng %s</p>
</body></html>`, r.Host, bc.Count(), version)
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("http: serving stream on %s/stream.mp3", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http: server error: %v", err)
		}
	}()

	return srv
}

// ---------------------------------------------------------------------------
// Auth & librespot
// ---------------------------------------------------------------------------

func logAuthMode(cfg *Config) {
	switch cfg.AuthMode {
	case "device-auth":
		log.Println("entrypoint: starting in device-auth mode")
	case "password":
		if cfg.SpotifyUsername != "" && cfg.SpotifyPassword != "" {
			log.Println("entrypoint: starting in password mode (deprecated by Spotify)")
		} else {
			log.Println("entrypoint: password mode selected but no credentials provided, falling back to zeroconf")
		}
	case "zeroconf":
		log.Println("entrypoint: starting in LAN-only zeroconf mode")
	default:
		log.Printf("entrypoint: unknown auth mode %q, falling back to zeroconf\n", cfg.AuthMode)
	}
}

func buildLibrespotArgs(cfg *Config, metaFile string) []string {
	args := []string{
		"--name", cfg.DeviceName,
		"--device-type", cfg.DeviceType,
		"--initial-volume", "100",
		"--enable-volume-normalisation",
		"--cache", cfg.CacheDir,
		"--disable-gapless",
	}

	// Wire up metadata events via our own binary as the onevent handler.
	if metaFile != "" {
		args = append(args, "--onevent", oneventCommand(metaFile))
	}

	switch cfg.AuthMode {
	case "device-auth":
		args = append(args, "--enable-device-auth")
	case "password":
		if cfg.SpotifyUsername != "" && cfg.SpotifyPassword != "" {
			args = append(args, "--username", cfg.SpotifyUsername, "--password", cfg.SpotifyPassword)
		}
	}

	if cfg.LibrespotExtra != "" {
		args = append(args, strings.Fields(cfg.LibrespotExtra)...)
	}

	return args
}

func ensureCredentialsPaired(ctx context.Context, cfg *Config) error {
	credPath := filepath.Join(cfg.CacheDir, "credentials.json")
	if _, err := os.Stat(credPath); err == nil {
		log.Println("entrypoint: cached credentials found, skipping pairing")
		return nil
	}

	log.Println("entrypoint: no cached credentials found. running initial pairing...")
	log.Println("entrypoint: visit spotify.com/pair and enter the code shown below:")

	pairCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	args := []string{
		"--name", cfg.DeviceName,
		"--device-type", cfg.DeviceType,
		"--cache", cfg.CacheDir,
		"--enable-device-auth",
		"--backend", "pipe",
	}
	cmd := exec.CommandContext(pairCtx, cfg.LibrespotPath, args...)
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting librespot for pairing: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(credPath); err == nil {
				log.Println("entrypoint: pairing successful! credentials cached.")
				killProcess(cmd)
				<-waitErr
				return nil
			}
		case <-pairCtx.Done():
			if _, err := os.Stat(credPath); err != nil {
				log.Println("entrypoint: pairing timed out after 120s. continuing anyway...")
			}
			killProcess(cmd)
			<-waitErr
			return nil
		case err := <-waitErr:
			return err
		}
	}
}

// killProcess terminates a process. On Unix this sends SIGTERM for a graceful
// exit; on Windows it kills immediately (SIGTERM is not supported).
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(os.Interrupt)
	}
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

func runPipeline(ctx context.Context, cfg *Config, bc *Broadcaster, metaFile string) error {
	librespotArgs := append(buildLibrespotArgs(cfg, metaFile), "--backend", "pipe")

	// Build ffmpeg args. Output destination depends on mode.
	ffmpegArgs := []string{
		"-loglevel", "warning",
		"-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
		"-af", "aresample=async=1",
		"-f", "mp3", "-b:a", cfg.Bitrate,
	}
	if cfg.UseEmbedded() {
		// Output to stdout - we'll read it and broadcast via HTTP.
		// flush_packets=1 is critical: without it ffmpeg buffers internally
		// and listeners get no data for several seconds, causing them to
		// disconnect and reconnect in a loop.
		ffmpegArgs = append(ffmpegArgs, "-flush_packets", "1", "pipe:1")
	} else {
		// Push to icecast directly.
		ffmpegArgs = append(ffmpegArgs,
			"-flush_packets", "1",
			"-content_type", "audio/mpeg",
			cfg.IcecastURL,
		)
	}

	librespotCmd := exec.Command(cfg.LibrespotPath, librespotArgs...)
	ffmpegCmd := exec.Command(cfg.FfmpegPath, ffmpegArgs...)

	librespotCmd.Stderr = os.Stderr
	ffmpegCmd.Stderr = os.Stderr

	// librespot stdout -> rate limiter -> ffmpeg stdin
	// We use OS pipes for librespot's output, then a Go goroutine rate-limits
	// the copy into ffmpeg's stdin pipe.
	librespotOut, librespotOutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating librespot output pipe: %w", err)
	}

	ffmpegIn, ffmpegInW, err := os.Pipe()
	if err != nil {
		librespotOut.Close()
		librespotOutW.Close()
		return fmt.Errorf("creating ffmpeg input pipe: %w", err)
	}

	librespotCmd.Stdout = librespotOutW
	ffmpegCmd.Stdin = ffmpegIn

	// In embedded mode, ffmpeg writes to stdout which we capture.
	if cfg.UseEmbedded() {
		ffmpegCmd.Stdout = bc
	}

	// Start both processes.
	if err := librespotCmd.Start(); err != nil {
		librespotOut.Close()
		librespotOutW.Close()
		ffmpegIn.Close()
		ffmpegInW.Close()
		return fmt.Errorf("starting librespot: %w", err)
	}

	if err := ffmpegCmd.Start(); err != nil {
		killProcess(librespotCmd)
		librespotOut.Close()
		librespotOutW.Close()
		ffmpegIn.Close()
		ffmpegInW.Close()
		return fmt.Errorf("starting ffmpeg: %w", err)
	}

	// Close parent-side pipe ends that the children now hold.
	librespotOutW.Close()
	ffmpegIn.Close()

	// Rate-limited copy goroutine (replaces pv).
	rateLimitDone := make(chan error, 1)
	go func() {
		err := rateLimitedCopy(ctx, ffmpegInW, librespotOut, pvRateBytesPerSec)
		ffmpegInW.Close()  // Signal EOF to ffmpeg.
		librespotOut.Close()
		rateLimitDone <- err
	}()

	// Context cancellation: kill both processes.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcess(librespotCmd)
			killProcess(ffmpegCmd)
		case <-watchDone:
		}
	}()

	// Wait for both processes.
	librespotErr := make(chan error, 1)
	ffmpegErr := make(chan error, 1)
	go func() { librespotErr <- librespotCmd.Wait() }()
	go func() { ffmpegErr <- ffmpegCmd.Wait() }()

	lErr := <-librespotErr
	fErr := <-ffmpegErr
	<-rateLimitDone
	close(watchDone)

	if lErr != nil {
		log.Printf("entrypoint: librespot exited: %v", lErr)
	}
	if fErr != nil {
		log.Printf("entrypoint: ffmpeg exited: %v", fErr)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if lErr != nil {
		return lErr
	}
	return fErr
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func run() error {
	log.SetFlags(0)

	cfg, isEventHandler, eventMetaFile, err := parseConfig(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	// Onevent handler mode: write metadata and exit.
	if isEventHandler {
		if eventMetaFile == "" {
			return fmt.Errorf("--metadata-file is required with --handle-event")
		}
		return handleEvent(eventMetaFile)
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("entrypoint: creating cache dir: %w", err)
	}

	// Auto-download missing dependencies (librespot, ffmpeg).
	if !cfg.NoSetup {
		if err := ensureDependencies(cfg); err != nil {
			return fmt.Errorf("entrypoint: setup failed: %w", err)
		}
	}

	logAuthMode(cfg)

	if cfg.UseEmbedded() {
		log.Printf("entrypoint: embedded mode, will serve stream on %s", cfg.ListenAddr)
	} else {
		log.Printf("entrypoint: icecast mode, streaming to %s", cfg.IcecastURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cfg.AuthMode == "device-auth" {
		if pairErr := ensureCredentialsPaired(ctx, cfg); pairErr != nil {
			log.Printf("entrypoint: pairing error: %v", pairErr)
		}
	}

	// Set up metadata tracking.
	metaStore := NewMetadataStore()
	metaFile := metadataFilePath(cfg.CacheDir)
	metaDone := make(chan struct{})
	go watchMetadataFile(metaStore, metaFile, metaDone)

	// Create broadcaster and start HTTP server (embedded mode).
	bc := NewBroadcaster()
	var srv *http.Server
	if cfg.UseEmbedded() {
		srv = startHTTPServer(ctx, cfg.ListenAddr, bc, metaStore)
	}

	// Start Cloudflare tunnel if requested.
	var tunnelCmd *exec.Cmd
	if cfg.Tunnel && cfg.UseEmbedded() {
		cfPath, err := ensureCloudflared()
		if err != nil {
			log.Printf("entrypoint: tunnel setup failed: %v", err)
			log.Println("entrypoint: continuing without tunnel. Stream is still available locally.")
		} else {
			cmd, tunnelURL, err := startTunnel(ctx, cfPath, cfg.ListenAddr)
			if err != nil {
				log.Printf("entrypoint: tunnel failed to start: %v", err)
			} else {
				tunnelCmd = cmd
				log.Println("========================================")
				log.Printf("  PUBLIC STREAM URL: %s/stream.mp3", tunnelURL)
				log.Println("  NOW PLAYING:      %s/now-playing", tunnelURL)
				log.Println("========================================")
				log.Println("Share this URL with anyone to let them listen!")
			}
		}
	}

	for ctx.Err() == nil {
		pipelineErr := runPipeline(ctx, cfg, bc, metaFile)
		if ctx.Err() != nil {
			break
		}
		if pipelineErr != nil {
			log.Printf("entrypoint: pipeline error: %v", pipelineErr)
		}
		log.Printf("entrypoint: pipeline exited, restarting in %s...", restartDelay)
		select {
		case <-time.After(restartDelay):
		case <-ctx.Done():
		}
	}

	close(metaDone)

	if tunnelCmd != nil {
		killProcess(tunnelCmd)
		_ = tunnelCmd.Wait()
	}

	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}

	log.Println("entrypoint: shutting down")
	return nil
}

func main() {
	if err := run(); err != nil {
		log.SetFlags(0)
		log.Println(err)
		os.Exit(1)
	}
}

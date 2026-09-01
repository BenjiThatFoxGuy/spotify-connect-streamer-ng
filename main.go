// Command spotify-connect-streamer-ng orchestrates librespot and ffmpeg to
// turn a Spotify Connect device into a plain HTTP MP3 stream pushed to an
// icecast server. It is a Go port of the entrypoint.sh from the Docker-based
// spotify-connect-streamer project: same pipeline, same restart-on-death
// behavior, minus the shell.
//
//	librespot (Connect device, raw PCM) -> pv (rate limiter) -> ffmpeg (MP3 encode) -> icecast
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// pvRateBytesPerSec is the byte rate pv throttles the pipe to: 44100Hz *
// 2 channels * 2 bytes/sample (16-bit PCM stereo). Pacing the pipe at
// real-time playback speed keeps librespot from downloading faster than
// playback, which is what causes Spotify to think a track finished early
// and skip ahead.
const pvRateBytesPerSec = "176400"

const restartDelay = 3 * time.Second

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// Config holds the fully-resolved runtime configuration: flag defaults,
// overridden by explicit flags, overridden again by environment variables
// (env wins, for docker compatibility).
type Config struct {
	DeviceName      string
	DeviceType      string
	Backend         string
	Bitrate         string
	IcecastURL      string
	CacheDir        string
	AuthMode        string
	SpotifyUsername string
	SpotifyPassword string
	LibrespotPath   string
	FfmpegPath      string
	PvPath          string
	LibrespotExtra  string

	// Docker-compat fallback: if IcecastURL isn't set directly, it's built
	// from these, matching the original entrypoint.sh's URL construction.
	IcecastHost           string
	IcecastPort           string
	IcecastSourcePassword string
	MountPoint            string
}

func parseConfig(args []string) (*Config, error) {
	fs := flag.NewFlagSet("spotify-connect-streamer-ng", flag.ContinueOnError)

	showVersion := fs.Bool("version", false, "Print version and exit")

	cfg := &Config{}
	fs.StringVar(&cfg.DeviceName, "name", "Stream Output", "Name shown in the Spotify Connect device picker")
	fs.StringVar(&cfg.DeviceType, "device-type", "speaker", "Device type shown in Spotify")
	fs.StringVar(&cfg.Backend, "backend", "pipe-pv", "Audio pipeline backend (only pipe-pv is implemented)")
	fs.StringVar(&cfg.Bitrate, "bitrate", "320k", "MP3 encoding bitrate")
	fs.StringVar(&cfg.IcecastURL, "icecast-url", "", "Full icecast destination URL, e.g. icecast://source:pass@host:8000/stream.mp3 (required)")
	fs.StringVar(&cfg.CacheDir, "cache-dir", "~/.cache/spotify-streamer", "librespot cache/credentials directory")
	fs.StringVar(&cfg.AuthMode, "auth-mode", "zeroconf", "Auth mode: zeroconf, device-auth, or password")
	fs.StringVar(&cfg.SpotifyUsername, "spotify-username", "", "Spotify username (auth-mode=password only)")
	fs.StringVar(&cfg.SpotifyPassword, "spotify-password", "", "Spotify password (auth-mode=password only)")
	fs.StringVar(&cfg.LibrespotPath, "librespot-path", "librespot", "Path to the librespot binary")
	fs.StringVar(&cfg.FfmpegPath, "ffmpeg-path", "ffmpeg", "Path to the ffmpeg binary")
	fs.StringVar(&cfg.PvPath, "pv-path", "pv", "Path to the pv binary")
	fs.StringVar(&cfg.LibrespotExtra, "librespot-extra-args", "", "Extra args passed straight through to librespot")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *showVersion {
		fmt.Println("spotify-connect-streamer-ng", version)
		return nil, flag.ErrHelp
	}

	// Env vars take precedence over flags, for docker compatibility.
	cfg.DeviceName = envOr("DEVICE_NAME", cfg.DeviceName)
	cfg.DeviceType = envOr("DEVICE_TYPE", cfg.DeviceType)
	cfg.Backend = envOr("BACKEND", cfg.Backend)
	cfg.Bitrate = envOr("MP3_BITRATE", cfg.Bitrate)
	cfg.IcecastURL = envOr("ICECAST_URL", cfg.IcecastURL)
	cfg.CacheDir = envOr("CACHE_DIR", cfg.CacheDir)
	cfg.AuthMode = envOr("AUTH_MODE", cfg.AuthMode)
	cfg.SpotifyUsername = envOr("SPOTIFY_USERNAME", cfg.SpotifyUsername)
	cfg.SpotifyPassword = envOr("SPOTIFY_PASSWORD", cfg.SpotifyPassword)
	cfg.LibrespotPath = envOr("LIBRESPOT_PATH", cfg.LibrespotPath)
	cfg.FfmpegPath = envOr("FFMPEG_PATH", cfg.FfmpegPath)
	cfg.PvPath = envOr("PV_PATH", cfg.PvPath)
	cfg.LibrespotExtra = envOr("LIBRESPOT_EXTRA_ARGS", cfg.LibrespotExtra)

	cfg.IcecastHost = envOr("ICECAST_HOST", "")
	cfg.IcecastPort = envOr("ICECAST_PORT", "8000")
	cfg.IcecastSourcePassword = envOr("ICECAST_SOURCE_PASSWORD", "")
	cfg.MountPoint = envOr("MOUNT_POINT", "stream.mp3")

	// docker-compose compat: if ICECAST_URL wasn't given directly but the
	// discrete pieces were (as in the original docker-compose.yml), build
	// the URL the same way entrypoint.sh did.
	if cfg.IcecastURL == "" && cfg.IcecastHost != "" && cfg.IcecastSourcePassword != "" {
		cfg.IcecastURL = fmt.Sprintf("icecast://source:%s@%s:%s/%s",
			cfg.IcecastSourcePassword, cfg.IcecastHost, cfg.IcecastPort, cfg.MountPoint)
	}

	cfg.CacheDir = expandHome(cfg.CacheDir)

	return cfg, nil
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
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// logAuthMode mirrors the log lines entrypoint.sh printed for each auth
// mode, so behavior watching logs sees the same story.
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

// buildLibrespotArgs assembles the librespot flags shared by the streaming
// pipeline, mirroring entrypoint.sh's librespot_base_args.
func buildLibrespotArgs(cfg *Config) []string {
	args := []string{
		"--name", cfg.DeviceName,
		"--device-type", cfg.DeviceType,
		"--initial-volume", "100",
		"--enable-volume-normalisation",
		"--cache", cfg.CacheDir,
		"--disable-gapless",
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

// ensureCredentialsPaired handles first-time device-auth pairing: if no
// cached credentials exist yet, it runs librespot standalone (discarding
// its audio output) until credentials.json appears or 120s elapse, exactly
// like the pairing block in entrypoint.sh.
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
	// Stdout left nil: raw PCM output is discarded, same as `> /dev/null`.

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
				_ = cmd.Process.Signal(syscall.SIGTERM)
				<-waitErr
				return nil
			}
		case <-pairCtx.Done():
			if _, err := os.Stat(credPath); err != nil {
				log.Println("entrypoint: pairing timed out after 120s. continuing anyway...")
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
			<-waitErr
			return nil
		case err := <-waitErr:
			// librespot exited on its own before pairing or timeout.
			return err
		}
	}
}

// runPipeline runs one pass of the pipe-pv backend:
//
//	librespot --backend pipe | pv -qL 176400 | ffmpeg -> icecast
//
// wired up with real OS pipes (not io.Copy goroutines) so behavior matches
// the shell version's pipe semantics: EOF propagates naturally when a stage
// exits, and each process gets its own fd rather than sharing one through
// Go-side buffering.
func runPipeline(ctx context.Context, cfg *Config) error {
	librespotArgs := append(buildLibrespotArgs(cfg), "--backend", "pipe")

	ffmpegArgs := []string{
		"-loglevel", "warning",
		"-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
		"-af", "aresample=async=1",
		"-f", "mp3", "-b:a", cfg.Bitrate,
		"-flush_packets", "1",
		"-content_type", "audio/mpeg",
		cfg.IcecastURL,
	}

	librespotCmd := exec.Command(cfg.LibrespotPath, librespotArgs...)
	pvCmd := exec.Command(cfg.PvPath, "-qL", pvRateBytesPerSec)
	ffmpegCmd := exec.Command(cfg.FfmpegPath, ffmpegArgs...)

	librespotCmd.Stderr = os.Stderr
	pvCmd.Stderr = os.Stderr
	ffmpegCmd.Stderr = os.Stderr

	r1, w1, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating librespot->pv pipe: %w", err)
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		r1.Close()
		w1.Close()
		return fmt.Errorf("creating pv->ffmpeg pipe: %w", err)
	}

	librespotCmd.Stdout = w1
	pvCmd.Stdin = r1
	pvCmd.Stdout = w2
	ffmpegCmd.Stdin = r2

	cmds := []*exec.Cmd{librespotCmd, pvCmd, ffmpegCmd}
	names := []string{"librespot", "pv", "ffmpeg"}

	closeParentPipeEnds := func() {
		r1.Close()
		w1.Close()
		r2.Close()
		w2.Close()
	}

	for i, cmd := range cmds {
		if startErr := cmd.Start(); startErr != nil {
			for _, started := range cmds[:i] {
				if started.Process != nil {
					_ = started.Process.Kill()
				}
			}
			closeParentPipeEnds()
			return fmt.Errorf("starting %s: %w", names[i], startErr)
		}
	}

	// Children now hold their own dup'd copies of the pipe fds; the parent
	// must close its copies so EOF propagates when a stage exits instead
	// of being held open by us.
	closeParentPipeEnds()

	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			for _, cmd := range cmds {
				if cmd.Process != nil {
					_ = cmd.Process.Signal(syscall.SIGTERM)
				}
			}
		case <-watchDone:
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, len(cmds))
	for i, cmd := range cmds {
		wg.Add(1)
		go func(i int, cmd *exec.Cmd) {
			defer wg.Done()
			errs[i] = cmd.Wait()
		}(i, cmd)
	}
	wg.Wait()
	close(watchDone)

	for i, e := range errs {
		if e != nil {
			log.Printf("entrypoint: %s exited: %v", names[i], e)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func run() error {
	log.SetFlags(0)

	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	if cfg.IcecastURL == "" {
		return fmt.Errorf("entrypoint: --icecast-url / ICECAST_URL is not set, refusing to start")
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("entrypoint: creating cache dir: %w", err)
	}

	if cfg.Backend != "pipe-pv" {
		log.Printf("entrypoint: backend %q not supported by this build, falling back to pipe-pv", cfg.Backend)
		cfg.Backend = "pipe-pv"
	}

	logAuthMode(cfg)
	log.Printf("entrypoint: backend=%s, streaming to %s", cfg.Backend, cfg.IcecastURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.AuthMode == "device-auth" {
		if pairErr := ensureCredentialsPaired(ctx, cfg); pairErr != nil {
			log.Printf("entrypoint: pairing error: %v", pairErr)
		}
	}

	for ctx.Err() == nil {
		pipelineErr := runPipeline(ctx, cfg)
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

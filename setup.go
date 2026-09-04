package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Dependency download URLs. These point to rolling/latest builds.
const (
	librespotWindowsURL   = "https://github.com/BenjiThatFoxGuy/librespot/releases/download/windows-latest/librespot.exe"
	ffmpegWindowsURL      = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"
	cloudflaredWindowsURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.exe"
	cloudflaredLinuxURL   = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64"
	cloudflaredDarwinURL  = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-amd64.tgz"
)

// installDir returns the directory where we keep downloaded binaries.
// This is the same directory as our own executable, making portable
// deployments trivial: everything lives in one folder.
func installDir() string {
	if self, err := os.Executable(); err == nil {
		return filepath.Dir(self)
	}
	// Fallback: current directory.
	dir, _ := os.Getwd()
	return dir
}

// ensureDependencies checks that librespot and ffmpeg are available.
// If either is missing and we're on a supported platform, offer to
// download them automatically.
func ensureDependencies(cfg *Config) error {
	dir := installDir()

	// Check librespot
	if _, err := exec.LookPath(cfg.LibrespotPath); err != nil {
		log.Printf("setup: librespot not found at %q", cfg.LibrespotPath)
		url := librespotDownloadURL()
		if url == "" {
			return fmt.Errorf("librespot not found and no automatic download available for %s/%s. Please install librespot manually", runtime.GOOS, runtime.GOARCH)
		}
		dest := filepath.Join(dir, binaryName("librespot"))
		log.Printf("setup: downloading librespot to %s...", dest)
		if err := downloadFile(url, dest); err != nil {
			return fmt.Errorf("downloading librespot: %w", err)
		}
		makeExecutable(dest)
		cfg.LibrespotPath = dest
		log.Println("setup: librespot downloaded successfully")
	}

	// Check ffmpeg
	if _, err := exec.LookPath(cfg.FfmpegPath); err != nil {
		log.Printf("setup: ffmpeg not found at %q", cfg.FfmpegPath)
		if runtime.GOOS == "windows" {
			dest := filepath.Join(dir, "ffmpeg.exe")
			log.Printf("setup: downloading ffmpeg to %s (this may take a moment)...", dest)
			if err := downloadFfmpegWindows(dest); err != nil {
				return fmt.Errorf("downloading ffmpeg: %w", err)
			}
			cfg.FfmpegPath = dest
			log.Println("setup: ffmpeg downloaded successfully")
		} else {
			return fmt.Errorf("ffmpeg not found. Please install it: apt install ffmpeg / brew install ffmpeg")
		}
	}

	return nil
}

// librespotDownloadURL returns the download URL for librespot on the
// current platform, or empty string if no pre-built binary is available.
func librespotDownloadURL() string {
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		return librespotWindowsURL
	}
	// Linux/macOS users should build from source or use the Docker image.
	// We could add more URLs here as CI produces more builds.
	return ""
}

func binaryName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func makeExecutable(path string) {
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o755)
	}
}

// downloadFile downloads a URL to a local file, following redirects.
func downloadFile(url, dest string) error {
	client := &http.Client{
		Timeout: 10 * time.Minute,
	}

	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp := dest + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	size, err := io.Copy(f, resp.Body)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}

	log.Printf("setup: downloaded %d bytes", size)

	return os.Rename(tmp, dest)
}

// downloadFfmpegWindows downloads the BtbN ffmpeg build zip and extracts
// just ffmpeg.exe from it.
func downloadFfmpegWindows(dest string) error {
	zipPath := dest + ".zip"
	defer os.Remove(zipPath)

	if err := downloadFile(ffmpegWindowsURL, zipPath); err != nil {
		return err
	}

	// Open the zip and find ffmpeg.exe inside.
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("opening ffmpeg zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if filepath.Base(f.Name) == "ffmpeg.exe" && strings.Contains(f.Name, "bin/") {
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("extracting ffmpeg.exe: %w", err)
			}
			defer rc.Close()

			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, rc)
			if closeErr := out.Close(); closeErr != nil && copyErr == nil {
				copyErr = closeErr
			}
			return copyErr
		}
	}

	return fmt.Errorf("ffmpeg.exe not found inside the downloaded zip")
}

// ---------------------------------------------------------------------------
// Cloudflare Tunnel (trycloudflare.com)
// ---------------------------------------------------------------------------

// ensureCloudflared checks for cloudflared and downloads it if missing.
func ensureCloudflared() (string, error) {
	// Check PATH first.
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("cloudflared.exe"); err == nil {
			return p, nil
		}
	}

	// Check next to our binary.
	dir := installDir()
	name := binaryName("cloudflared")
	local := filepath.Join(dir, name)
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}

	// Download it.
	url := cloudflaredURL()
	if url == "" {
		return "", fmt.Errorf("no cloudflared download available for %s/%s. Please install it manually: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/", runtime.GOOS, runtime.GOARCH)
	}

	log.Printf("setup: downloading cloudflared to %s...", local)
	if err := downloadFile(url, local); err != nil {
		return "", fmt.Errorf("downloading cloudflared: %w", err)
	}
	makeExecutable(local)
	log.Println("setup: cloudflared downloaded successfully")
	return local, nil
}

func cloudflaredURL() string {
	switch runtime.GOOS {
	case "windows":
		return cloudflaredWindowsURL
	case "linux":
		if runtime.GOARCH == "amd64" {
			return cloudflaredLinuxURL
		}
	}
	// macOS and other architectures: install manually.
	return ""
}

// startTunnel launches a cloudflared quick tunnel pointing at localAddr
// and returns the public URL once it's ready.
func startTunnel(ctx context.Context, cloudflaredPath, localAddr string) (*exec.Cmd, string, error) {
	if !strings.HasPrefix(localAddr, "http") {
		localAddr = "http://localhost" + localAddr
	}

	cmd := exec.CommandContext(ctx, cloudflaredPath, "tunnel", "--url", localAddr)

	// cloudflared prints the tunnel URL to stderr.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting cloudflared: %w", err)
	}

	// Read stderr line by line looking for the tunnel URL.
	urlCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var accumulated string
		for {
			n, readErr := stderrPipe.Read(buf)
			if n > 0 {
				accumulated += string(buf[:n])
				// Look for the trycloudflare.com URL
				for _, line := range strings.Split(accumulated, "\n") {
					line = strings.TrimSpace(line)
					if idx := strings.Index(line, "https://"); idx >= 0 {
						url := line[idx:]
						// Trim anything after the URL.
						if sp := strings.IndexAny(url, " \t\r\n\"'"); sp >= 0 {
							url = url[:sp]
						}
						if strings.Contains(url, "trycloudflare.com") {
							select {
							case urlCh <- url:
							default:
							}
						}
					}
				}
			}
			if readErr != nil {
				break
			}
		}
	}()

	// Wait up to 30 seconds for the tunnel URL.
	select {
	case url := <-urlCh:
		return cmd, url, nil
	case <-time.After(30 * time.Second):
		killProcess(cmd)
		return nil, "", fmt.Errorf("timed out waiting for cloudflared tunnel URL")
	case <-ctx.Done():
		killProcess(cmd)
		return nil, "", ctx.Err()
	}
}

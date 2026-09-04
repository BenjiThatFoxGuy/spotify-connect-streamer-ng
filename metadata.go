package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TrackMeta holds the currently playing track's metadata, populated by
// librespot's --onevent callback.
type TrackMeta struct {
	Event      string `json:"event"`
	Name       string `json:"name,omitempty"`
	Artists    string `json:"artists,omitempty"`
	Album      string `json:"album,omitempty"`
	URI        string `json:"uri,omitempty"`
	DurationMs string `json:"duration_ms,omitempty"`
	Covers     string `json:"covers,omitempty"`
	TrackID    string `json:"track_id,omitempty"`
	PositionMs string `json:"position_ms,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// StreamTitle returns the ICY-formatted stream title string.
func (t *TrackMeta) StreamTitle() string {
	if t == nil || t.Name == "" {
		return ""
	}
	artists := strings.ReplaceAll(t.Artists, "\n", ", ")
	if artists != "" {
		return fmt.Sprintf("%s - %s", artists, t.Name)
	}
	return t.Name
}

// MetadataStore is a thread-safe container for the current track metadata.
type MetadataStore struct {
	mu   sync.RWMutex
	meta *TrackMeta
}

func NewMetadataStore() *MetadataStore {
	return &MetadataStore{}
}

func (s *MetadataStore) Update(m *TrackMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta = m
	log.Printf("metadata: now playing: %s", m.StreamTitle())
}

func (s *MetadataStore) Get() *TrackMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta
}

func (s *MetadataStore) StreamTitle() string {
	m := s.Get()
	if m == nil {
		return ""
	}
	return m.StreamTitle()
}

// ---------------------------------------------------------------------------
// Onevent handler mode (--handle-event)
// ---------------------------------------------------------------------------

// handleEvent is called when the binary is invoked as librespot's --onevent
// handler. It reads environment variables set by librespot and writes the
// metadata to a JSON file, then exits.
func handleEvent(metadataFile string) error {
	event := os.Getenv("PLAYER_EVENT")
	if event == "" {
		return fmt.Errorf("PLAYER_EVENT not set")
	}

	meta := &TrackMeta{
		Event:     event,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	switch event {
	case "track_changed":
		meta.Name = os.Getenv("NAME")
		meta.Artists = os.Getenv("ARTISTS")
		meta.Album = os.Getenv("ALBUM")
		meta.URI = os.Getenv("URI")
		meta.DurationMs = os.Getenv("DURATION_MS")
		meta.Covers = os.Getenv("COVERS")
		meta.TrackID = os.Getenv("TRACK_ID")
	case "playing":
		meta.TrackID = os.Getenv("TRACK_ID")
		meta.PositionMs = os.Getenv("POSITION_MS")
	case "paused", "stopped":
		meta.TrackID = os.Getenv("TRACK_ID")
	default:
		// For other events, just record the event type
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write: write to temp then rename
	tmp := metadataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, metadataFile)
}

// ---------------------------------------------------------------------------
// Metadata file watcher
// ---------------------------------------------------------------------------

// watchMetadataFile polls a JSON file for changes and updates the store.
func watchMetadataFile(store *MetadataStore, path string, done <-chan struct{}) {
	var lastMod time.Time

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.ModTime().Equal(lastMod) {
				continue
			}
			lastMod = info.ModTime()

			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}

			var meta TrackMeta
			if err := json.Unmarshal(data, &meta); err != nil {
				continue
			}

			store.Update(&meta)
		case <-done:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// /now-playing JSON endpoint
// ---------------------------------------------------------------------------

func handleNowPlaying(store *MetadataStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-cache")

		meta := store.Get()
		if meta == nil {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"playing":false}`)
			return
		}

		resp := map[string]interface{}{
			"playing":     meta.Event == "playing" || meta.Event == "track_changed",
			"title":       meta.Name,
			"artists":     strings.Split(meta.Artists, "\n"),
			"album":       meta.Album,
			"uri":         meta.URI,
			"duration_ms": meta.DurationMs,
			"track_id":    meta.TrackID,
			"updated_at":  meta.UpdatedAt,
		}

		// First cover URL
		if meta.Covers != "" {
			covers := strings.Split(meta.Covers, "\n")
			if len(covers) > 0 && covers[0] != "" {
				resp["cover_url"] = covers[0]
			}
		}

		data, _ := json.Marshal(resp)
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

// ---------------------------------------------------------------------------
// ICY metadata-aware stream writer
// ---------------------------------------------------------------------------

const icyMetaInt = 8192 // Bytes of audio between metadata blocks

// IcyWriter wraps a ResponseWriter and injects ICY metadata blocks
// every icyMetaInt bytes of audio data. Create one per listener that
// requests ICY metadata (Icy-MetaData: 1 header).
type IcyWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	store   *MetadataStore
	count   int // bytes written since last metadata block
}

func NewIcyWriter(w http.ResponseWriter, store *MetadataStore) *IcyWriter {
	flusher, _ := w.(http.Flusher)
	return &IcyWriter{
		w:       w,
		flusher: flusher,
		store:   store,
	}
}

func (iw *IcyWriter) Write(audio []byte) (int, error) {
	totalWritten := 0

	for len(audio) > 0 {
		// How many bytes until the next metadata block?
		remaining := icyMetaInt - iw.count

		if remaining > len(audio) {
			// Write all remaining audio, no metadata block yet
			n, err := iw.w.Write(audio)
			iw.count += n
			totalWritten += n
			if err != nil {
				return totalWritten, err
			}
			break
		}

		// Write audio up to the metadata boundary
		n, err := iw.w.Write(audio[:remaining])
		iw.count += n
		totalWritten += n
		if err != nil {
			return totalWritten, err
		}

		// Insert metadata block
		if err := iw.writeMetaBlock(); err != nil {
			return totalWritten, err
		}

		iw.count = 0
		audio = audio[remaining:]
	}

	if iw.flusher != nil {
		iw.flusher.Flush()
	}
	return totalWritten, nil
}

func (iw *IcyWriter) writeMetaBlock() error {
	title := iw.store.StreamTitle()
	if title == "" {
		// Empty metadata: just write a zero byte (no metadata)
		_, err := iw.w.Write([]byte{0})
		return err
	}

	// Escape single quotes in the title
	title = strings.ReplaceAll(title, "'", "\\'")
	meta := fmt.Sprintf("StreamTitle='%s';", title)

	// Length must be a multiple of 16
	metaLen := len(meta)
	blocks := (metaLen + 15) / 16
	padded := make([]byte, blocks*16+1)
	padded[0] = byte(blocks)
	copy(padded[1:], meta)

	_, err := iw.w.Write(padded)
	return err
}

// ---------------------------------------------------------------------------
// Onevent script generator
// ---------------------------------------------------------------------------

// metadataFilePath returns the path where the metadata JSON will be written.
func metadataFilePath(cacheDir string) string {
	return filepath.Join(cacheDir, "metadata.json")
}

// selfPath returns the path to our own executable.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}

// oneventCommand returns the command string to pass to librespot's --onevent.
// It invokes ourselves with --handle-event.
func oneventCommand(metaFile string) string {
	self := selfPath()
	// Quote the paths for shell safety
	return fmt.Sprintf(`"%s" --handle-event --metadata-file "%s"`, self, metaFile)
}

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/audioout"
	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
	"github.com/miku0139/airplay-go/backend/internal/receiver"
)

// runtimeState is what the UI asks for over the local API. It lives beside the
// receiver rather than inside it because the numbers come from three places —
// the protocol state machine, the mirror stream and the frame sink — and the UI
// wants one answer.
type runtimeState struct {
	mu      sync.RWMutex
	status  receiver.Status
	reason  string
	sender  string
	started time.Time
}

func newRuntimeState() *runtimeState {
	return &runtimeState{started: time.Now()}
}

// set records a protocol transition. The receiver calls this from whatever
// goroutine handled the request.
func (s *runtimeState) set(status receiver.Status, reason string) {
	s.mu.Lock()
	s.status = status
	s.reason = reason
	s.mu.Unlock()
}

// setSender records which device is mirroring, so the UI can name it.
func (s *runtimeState) setSender(name string) {
	s.mu.Lock()
	s.sender = name
	s.mu.Unlock()
}

func (s *runtimeState) snapshot() (receiver.Status, string, string, time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status, s.reason, s.sender, time.Since(s.started)
}

// apiState is the JSON body the app reads. Field names are the contract with
// frontend/lib/receiver_api.dart.
type apiState struct {
	Status       string  `json:"status"`
	Reason       string  `json:"reason"`
	Name         string  `json:"name"`
	IP           string  `json:"ip"`
	Port         int     `json:"port"`
	Sender       string  `json:"sender"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	VideoFrames  uint64  `json:"videoFrames"`
	AudioFrames  uint64  `json:"audioFrames"`
	Pictures     uint64  `json:"pictures"`
	Dropped      uint64  `json:"dropped"`
	Decoder      string  `json:"decoder"`
	Muted        bool    `json:"muted"`
	Volume       float64 `json:"volume"`
	UptimeSecs   float64 `json:"uptimeSeconds"`
	Version      string  `json:"version"`
	TargetWidth  int     `json:"targetWidth"`
	TargetHeight int     `json:"targetHeight"`
	MaxFPS       int     `json:"maxFps"`
	DecoderMode  string  `json:"decoderMode"`
	ColorRange   string  `json:"colorRange"`
	ScaleFilter  string  `json:"scaleFilter"`
	AudioBuffer  string  `json:"audioBuffer"`
}

// statusName maps the protocol state onto the word the UI switches on.
func statusName(s receiver.Status) string {
	switch s {
	case receiver.StatusConnecting:
		return "connecting"
	case receiver.StatusLive:
		return "live"
	case receiver.StatusInterrupted:
		return "interrupted"
	default:
		return "idle"
	}
}

// apiHandler serves the localhost endpoints the app polls and calls. It is
// bound to 127.0.0.1 only.
func apiHandler(
	info protocol.DeviceInfo,
	port int,
	cfg *config.Config,
	state *runtimeState,
	sink *frameSink,
	recv *receiver.Receiver,
	speaker *audioout.Player,
	shotsDir string,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		status, reason, sender, uptime := state.snapshot()
		width, height := 0, 0
		var pictures, dropped uint64
		decoderMode := ""
		if sink != nil {
			width, height = sink.Size()
			pictures = sink.Frames()
			dropped = sink.Dropped()
			decoderMode = sink.Mode()
		}
		muted := false
		volume := 1.0
		if speaker != nil {
			muted = speaker.Muted()
			volume = speaker.Volume()
		}
		body := apiState{
			Status:       statusName(status),
			Reason:       reason,
			Name:         info.Name,
			IP:           protocol.LocalIPv4(),
			Port:         port,
			Sender:       sender,
			Width:        width,
			Height:       height,
			VideoFrames:  frameCount.Load(),
			AudioFrames:  audioFrames.Load(),
			Pictures:     pictures,
			Dropped:      dropped,
			Decoder:      decoderMode,
			Muted:        muted,
			Volume:       volume,
			UptimeSecs:   uptime.Seconds(),
			Version:      protocol.SourceVersion,
			TargetWidth:  cfg.Width,
			TargetHeight: cfg.Height,
			MaxFPS:       cfg.MaxFPS,
			DecoderMode:  cfg.DecoderMode,
			ColorRange:   cfg.ColorRange,
			ScaleFilter:  cfg.ScaleFilter,
			AudioBuffer:  cfg.AudioBuffer,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Printf("api: encode: %v", err)
		}
	})

	mux.HandleFunc("/api/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if recv != nil {
			recv.Disconnect()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/api/audio", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Muted  *bool    `json:"muted"`
			Volume *float64 `json:"volume"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if speaker != nil {
			if req.Muted != nil {
				speaker.SetMuted(*req.Muted)
			}
			if req.Volume != nil {
				speaker.SetVolume(*req.Volume)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if sink == nil {
			http.Error(w, "no active video sink", http.StatusServiceUnavailable)
			return
		}
		filename := fmt.Sprintf("MirrPane-%s.png", time.Now().Format("20060102-150405"))
		outPath := filepath.Join(shotsDir, filename)
		if err := sink.SnapshotPNG(outPath); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": outPath})
	})

	return mux
}

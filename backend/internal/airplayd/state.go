package airplayd

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/audio"
	"github.com/miku0139/airplay-go/backend/internal/audioout"
	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
	"github.com/miku0139/airplay-go/backend/internal/receiver"
	"github.com/miku0139/airplay-go/backend/internal/trust"
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

// apiState is the JSON representation consumed by cmd/mirrpane's ReceiverState.
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

	PendingSender *receiver.PendingSender `json:"pendingSender"`

	// Pipeline telemetry. All durations are milliseconds over the last
	// frames.StatsWindow pictures; keys are additive so older app builds keep
	// parsing the body.
	FPS                float64 `json:"fps"`
	DecodeAvgMs        float64 `json:"decodeAvgMs"`
	DecodeP95Ms        float64 `json:"decodeP95Ms"`
	DownloadAvgMs      float64 `json:"downloadAvgMs"`
	DownloadP95Ms      float64 `json:"downloadP95Ms"`
	ConvertAvgMs       float64 `json:"convertAvgMs"`
	ConvertP95Ms       float64 `json:"convertP95Ms"`
	PublishAvgMs       float64 `json:"publishAvgMs"`
	PublishP95Ms       float64 `json:"publishP95Ms"`
	LagFrames          uint64  `json:"lagFrames"`
	ConsumerAgeMs      float64 `json:"consumerAgeMs"`
	ConsumerAdopted    uint64  `json:"consumerAdopted"`
	HWMode             string  `json:"hwMode"`
	GPUZeroCopy        bool    `json:"gpuZeroCopy"`
	AudioOccupancy     float64 `json:"audioOccupancyMs"`
	AudioUnderruns     uint64  `json:"audioUnderruns"`
	AudioHighWater     float64 `json:"audioHighWaterMs"`
	AudioTrims         uint64  `json:"audioTrims"`
	AudioLateDrop      uint64  `json:"audioLateDropped"`
	AudioDeviceErrors  uint64  `json:"audioDeviceErrors"`
	AudioDeviceReopens uint64  `json:"audioDeviceReopens"`

	// Audio UDP socket counters: what each socket carried and why the data
	// path rejected packets. They distinguish "the sender streamed nothing"
	// from "packets arrived and were dropped".
	AudioDataRx          uint64 `json:"audioDataRx"`
	AudioControlRx       uint64 `json:"audioControlRx"`
	AudioPeerDrops       uint64 `json:"audioPeerDrops"`
	AudioShortDrops      uint64 `json:"audioShortDrops"`
	AudioSourceDrops     uint64 `json:"audioSourceDrops"`
	AudioControlRTPDrops uint64 `json:"audioControlRtpDrops"`
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

type receiverControl interface {
	Disconnect()
	ClearTrustedSenders() error
	PendingSenders() []receiver.PendingSender
	ApproveSender(string) error
	RejectSender(string) error
	AudioLateDropped() uint64
	AudioDiagnostics() audio.Diagnostics
}

// apiHandler serves the localhost endpoints the app polls and calls. It is
// bound to 127.0.0.1 only.
func apiHandler(
	info protocol.DeviceInfo,
	port int,
	cfg *config.Config,
	state *runtimeState,
	sink *frameSink,
	recv receiverControl,
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
		var pictures uint64
		var stats SinkStats
		decoderMode := ""
		if sink != nil {
			width, height = sink.Size()
			pictures = sink.Frames()
			stats = sink.Stats()
			decoderMode = sink.Mode()
		}
		muted := false
		volume := 1.0
		var audioStats audioout.Stats
		if speaker != nil {
			muted = speaker.Muted()
			volume = speaker.Volume()
			audioStats = speaker.Stats()
		}
		body := apiState{
			Status:      statusName(status),
			Reason:      reason,
			Name:        info.Name,
			IP:          protocol.LocalIPv4(),
			Port:        port,
			Sender:      sender,
			Width:       width,
			Height:      height,
			VideoFrames: frameCount.Load(),
			AudioFrames: audioFrames.Load(),
			Pictures:    pictures,
			// "dropped" stays the honest slot-contention counter; producer→
			// consumer lag is reported separately so a hidden window does not
			// look like dropped frames forever.
			Dropped:      stats.RawDropped,
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

			FPS:                stats.FPS,
			DecodeAvgMs:        stats.DecodeAvgMs,
			DecodeP95Ms:        stats.DecodeP95Ms,
			DownloadAvgMs:      stats.DownloadAvgMs,
			DownloadP95Ms:      stats.DownloadP95Ms,
			ConvertAvgMs:       stats.ConvertAvgMs,
			ConvertP95Ms:       stats.ConvertP95Ms,
			PublishAvgMs:       stats.PublishAvgMs,
			PublishP95Ms:       stats.PublishP95Ms,
			LagFrames:          stats.LagFrames,
			ConsumerAgeMs:      stats.ConsumerAgeMs,
			ConsumerAdopted:    stats.ConsumerAdopted,
			HWMode:             decoderMode,
			GPUZeroCopy:        stats.GPUZeroCopy,
			AudioOccupancy:     audioStats.OccupancyMs,
			AudioUnderruns:     audioStats.UnderrunCount,
			AudioHighWater:     audioStats.HighWaterMs,
			AudioTrims:         audioStats.TrimCount,
			AudioDeviceErrors:  audioStats.DeviceErrors,
			AudioDeviceReopens: audioStats.DeviceReopens,
		}
		if recv != nil {
			body.AudioLateDrop = recv.AudioLateDropped()
			diag := recv.AudioDiagnostics()
			body.AudioDataRx = diag.DataRx
			body.AudioControlRx = diag.ControlRx
			body.AudioPeerDrops = diag.PeerDrops
			body.AudioShortDrops = diag.ShortDrops
			body.AudioSourceDrops = diag.SourceDrops
			body.AudioControlRTPDrops = diag.ControlRTPDrops
			if pending := recv.PendingSenders(); len(pending) > 0 {
				body.PendingSender = &pending[0]
			}
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

	mux.HandleFunc("/api/pairing/trust/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if recv == nil {
			http.Error(w, "receiver unavailable", http.StatusInternalServerError)
			return
		}
		if err := recv.ClearTrustedSenders(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	trustDecision := func(approve bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				Fingerprint string `json:"fingerprint"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil || !trust.ValidFingerprint(req.Fingerprint) {
				http.Error(w, "invalid sender fingerprint", http.StatusBadRequest)
				return
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				http.Error(w, "invalid approval request", http.StatusBadRequest)
				return
			}
			if recv == nil {
				http.Error(w, "receiver unavailable", http.StatusInternalServerError)
				return
			}
			var err error
			if approve {
				err = recv.ApproveSender(req.Fingerprint)
			} else {
				err = recv.RejectSender(req.Fingerprint)
			}
			if err != nil {
				status := http.StatusInternalServerError
				switch {
				case errors.Is(err, receiver.ErrInvalidFingerprint):
					status = http.StatusBadRequest
				case errors.Is(err, receiver.ErrNotPending):
					status = http.StatusNotFound
				}
				http.Error(w, err.Error(), status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}
	mux.HandleFunc("/api/pairing/trust/approve", trustDecision(true))
	mux.HandleFunc("/api/pairing/trust/reject", trustDecision(false))

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
		outPath, err := sink.SnapshotPNG(shotsDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": outPath})
	})

	return requireLocalHostAndOrigin(mux)
}

// requireLocalHostAndOrigin blocks DNS rebinding and cross-origin browser
// requests against the localhost UI API.
func requireLocalHostAndOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !isAllowedAPIHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if origins := r.Header.Values("Origin"); len(origins) > 0 {
			if len(origins) > 1 || !isAllowedAPIOrigin(origins[0]) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isAllowedAPIHost(raw string) bool {
	if raw == "" {
		return false
	}
	if strings.Contains(raw, ":") {
		if raw == "[::1]" {
			return true
		}
		host, port, err := net.SplitHostPort(raw)
		if err != nil || !isValidPort(port) {
			return false
		}
		return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
	}
	return raw == "127.0.0.1" || strings.EqualFold(raw, "localhost")
}

func isAllowedAPIOrigin(origin string) bool {
	// "null" is the opaque origin of sandboxed iframes, data: and file: pages.
	// It is exactly the CSRF vector this check exists to block, so reject it.
	if origin == "null" || origin == "" || strings.ContainsAny(origin, "?#") {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || !isValidPort(port) {
		return false
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
}

func isValidPort(port string) bool {
	if port == "" {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

package airplayd

import (
	"strings"
	"sync"
	"time"

	"github.com/Miku0139oao/MirrPane/backend/internal/audio"
	"github.com/Miku0139oao/MirrPane/backend/internal/receiver"
)

// runtimeState is what the UI asks for over the local API. It lives beside the
// receiver rather than inside it because the numbers come from three places —
// the protocol state machine, the mirror stream and the frame sink — and the UI
// wants one answer.
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
	Status          string  `json:"status"`
	Reason          string  `json:"reason"`
	Source          string  `json:"source,omitempty"`
	Name            string  `json:"name"`
	IP              string  `json:"ip"`
	Port            int     `json:"port"`
	Sender          string  `json:"sender"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	VideoFrames     uint64  `json:"videoFrames"`
	AudioFrames     uint64  `json:"audioFrames"`
	Pictures        uint64  `json:"pictures"`
	Dropped         uint64  `json:"dropped"`
	Decoder         string  `json:"decoder"`
	Muted           bool    `json:"muted"`
	Volume          float64 `json:"volume"`
	UptimeSecs      float64 `json:"uptimeSeconds"`
	Version         string  `json:"version"`
	TargetWidth     int     `json:"targetWidth"`
	TargetHeight    int     `json:"targetHeight"`
	MaxFPS          int     `json:"maxFps"`
	DecoderMode     string  `json:"decoderMode"`
	ColorRange      string  `json:"colorRange"`
	ScaleFilter     string  `json:"scaleFilter"`
	AudioBuffer     string  `json:"audioBuffer"`
	RequireApproval bool    `json:"requireApproval"`

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

	// Android is the optional scrcpy session's state. Source is "android"
	// while that session owns the display.
	Android *apiAndroid `json:"android,omitempty"`
}

// firstNonEmpty returns the first value that has content.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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

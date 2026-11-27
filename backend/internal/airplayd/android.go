package airplayd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/adb"
	"github.com/miku0139/airplay-go/backend/internal/audioout"
	"github.com/miku0139/airplay-go/backend/internal/mirror"
	"github.com/miku0139/airplay-go/backend/internal/scrcpy"
)

// Android session phases, as the UI polls them.
const (
	androidIdle       = "idle"
	androidConnecting = "connecting"
	androidLive       = "live"
	androidError      = "error"
)

// Android key codes from android.view.KeyEvent.
const (
	keycodeHome      = 3
	keycodeBack      = 4
	keycodeAppSwitch = 187
)

// androidManager owns the optional scrcpy session. It publishes the device's
// H.264 access units into the same frameSink the AirPlay mirror stream uses,
// so the rest of the pipeline — decoder, shared memory, HUD — is untouched.
type androidManager struct {
	log     *log.Logger
	sink    *frameSink
	state   *runtimeState
	speaker *audioout.Player

	mu     sync.Mutex
	client *adb.Client
	sess   *scrcpy.Session
	cancel context.CancelFunc
	gen    uint64

	phase  string
	reason string
	errMsg string
	serial string
	model  string
	device string
	width  int
	height int
}

// apiAndroid is the Android half of /api/state.
type apiAndroid struct {
	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Serial string `json:"serial,omitempty"`
	Model  string `json:"model,omitempty"`
	Device string `json:"device,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

func newAndroidManager(logger *log.Logger, sink *frameSink, state *runtimeState, speaker *audioout.Player) *androidManager {
	if logger == nil {
		logger = log.Default()
	}
	return &androidManager{log: logger, sink: sink, state: state, speaker: speaker, phase: androidIdle}
}

func (m *androidManager) androidClient() (*adb.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		return m.client, nil
	}
	path, err := adb.Find()
	if err != nil {
		return nil, err
	}
	client, err := adb.New(path)
	if err != nil {
		return nil, err
	}
	m.client = client
	return client, nil
}

// Devices lists what adb sees. A missing adb is reported as an error the UI
// turns into installation guidance, not silence.
func (m *androidManager) Devices(ctx context.Context) ([]adb.Device, error) {
	client, err := m.androidClient()
	if err != nil {
		return nil, err
	}
	return client.Devices(ctx)
}

// WirelessConnects an already-paired device over TCP.
func (m *androidManager) Wireless(ctx context.Context, address string) error {
	if err := validAndroidAddress(address); err != nil {
		return err
	}
	client, err := m.androidClient()
	if err != nil {
		return err
	}
	return client.Connect(ctx, address)
}

// Pair performs adb wireless-debugging pairing with a code from the device.
func (m *androidManager) Pair(ctx context.Context, address, code string) error {
	if err := validAndroidAddress(address); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(code) {
		return errors.New("配對碼必須是 6 位數字")
	}
	client, err := m.androidClient()
	if err != nil {
		return err
	}
	return client.Pair(ctx, address, code)
}

// Connect starts a session and returns immediately; the UI follows the phase
// through /api/state. Devices reach Connect only after a successful list.
func (m *androidManager) Connect(serial string, opt scrcpy.Options) error {
	if err := validAndroidSerial(serial); err != nil {
		return err
	}
	if status, _, _, _ := m.state.snapshot(); status != 0 {
		return errors.New("AirPlay 連線進行中，請先中斷再連接 Android 裝置")
	}
	m.Stop()

	m.mu.Lock()
	m.gen++
	gen := m.gen
	m.phase = androidConnecting
	m.reason = "正在啟動 Android 連線…"
	m.errMsg = ""
	m.serial = serial
	m.device = ""
	m.model = ""
	m.width, m.height = 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	go m.run(ctx, gen, serial, opt)
	return nil
}

func (m *androidManager) run(ctx context.Context, gen uint64, serial string, opt scrcpy.Options) {
	client, err := m.androidClient()
	if err != nil {
		m.fail(gen, err)
		return
	}
	opt.Serial = serial
	startCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	sess, err := scrcpy.Start(startCtx, client, opt)
	cancel()
	if err != nil {
		m.fail(gen, screenstartError(err))
		return
	}

	info := sess.Info()
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		_ = sess.Close()
		return
	}
	m.sess = sess
	m.phase = androidLive
	m.reason = ""
	m.errMsg = ""
	m.device = info.DeviceName
	m.width, m.height = info.Width, info.Height
	m.mu.Unlock()
	m.log.Printf("android: session live (%s, %s %dx%d)", serial, info.Codec, info.Width, info.Height)

	if info, ok := sess.AudioInfo(); ok {
		m.log.Printf("android: audio %s %d Hz %d ch", info.Codec, info.SampleRate, info.Channels)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if _, ok := sess.AudioInfo(); ok && m.speaker != nil {
		go m.pumpAudio(runCtx, sess)
	}

	m.sink.ResetStream()
	for {
		pkt, err := sess.ReadVideo(runCtx)
		if err != nil {
			if runCtx.Err() == nil {
				m.fail(gen, fmt.Errorf("Android 串流結束：%w", err))
			}
			break
		}
		if pkt.Config && len(pkt.Data) == 0 {
			continue
		}
		m.mu.Lock()
		if m.gen != gen {
			m.mu.Unlock()
			break
		}
		if info := sess.Info(); info.Width > 0 && info.Height > 0 {
			m.width, m.height = info.Width, info.Height
		}
		m.mu.Unlock()
		m.sink.Handle(&mirror.Frame{
			Data:    pkt.Data,
			NTP:     pkt.PTS,
			Valid:   true,
			Codec:   mirror.CodecH264,
			Session: gen,
		})
	}
	cancelRun()
	if m.speaker != nil {
		m.speaker.Flush()
	}
	m.clear(gen)
}

// pumpAudio plays scrcpy's raw PCM (48 kHz stereo s16le) through the same
// waveOut player the AirPlay path uses, so mute/volume/anti-jitter apply.
func (m *androidManager) pumpAudio(ctx context.Context, sess *scrcpy.Session) {
	for {
		pkt, err := sess.ReadAudio(ctx)
		if err != nil {
			return
		}
		if pkt.Config || len(pkt.Data) < 4 {
			continue
		}
		m.speaker.Write(pcm16le(pkt.Data))
	}
}

// pcm16le converts little-endian signed 16-bit PCM bytes to samples.
func pcm16le(data []byte) []int16 {
	n := len(data) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return out
}

// screenstartError turns the common adb failures into what the user can act on.
func screenstartError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unauthorized"):
		return errors.New("裝置尚未授權：請在 Android 手機上允許 USB 偵錯")
	case strings.Contains(msg, "no devices"), strings.Contains(msg, "device not found"), strings.Contains(msg, "not found"):
		return fmt.Errorf("找不到指定的 Android 裝置：%w", err)
	case strings.Contains(msg, "more than one device"):
		return fmt.Errorf("連接了多台裝置，請指定序號：%w", err)
	}
	return err
}

func (m *androidManager) fail(gen uint64, err error) {
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		return
	}
	m.phase = androidError
	m.errMsg = err.Error()
	m.reason = ""
	m.mu.Unlock()
	m.log.Printf("android: %v", err)
}

func (m *androidManager) clear(gen uint64) {
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		return
	}
	m.phase = androidIdle
	m.reason = ""
	m.sess = nil
	m.width, m.height = 0, 0
	m.mu.Unlock()
}

// Stop ends the session, if any, and returns once its goroutine can no longer
// publish. It is safe to call repeatedly.
func (m *androidManager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	sess := m.sess
	m.cancel = nil
	m.sess = nil
	if m.phase != androidIdle {
		m.phase = androidIdle
		m.reason = ""
	}
	m.gen++
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if sess != nil {
		_ = sess.Close()
	}
	if m.sink != nil {
		m.sink.ResetStream()
	}
}

// Active reports whether a session is starting or running.
func (m *androidManager) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase == androidConnecting || m.phase == androidLive
}

// Status is the manager's state as /api/state reports it.
func (m *androidManager) Status() apiAndroid {
	m.mu.Lock()
	defer m.mu.Unlock()
	return apiAndroid{
		Phase:  m.phase,
		Reason: m.reason,
		Error:  m.errMsg,
		Serial: m.serial,
		Model:  m.model,
		Device: m.device,
		Width:  m.width,
		Height: m.height,
	}
}

// Input injection. The UI sends one message per pointer or key event; the
// controller serialises them onto the scrcpy control socket.

type androidInput struct {
	Kind     string  `json:"kind"` // touch | scroll | key | text | back | home | appswitch | rotate
	Action   string  `json:"action,omitempty"`
	X        int     `json:"x,omitempty"`
	Y        int     `json:"y,omitempty"`
	W        int     `json:"w,omitempty"`
	H        int     `json:"h,omitempty"`
	DX       float64 `json:"dx,omitempty"`
	DY       float64 `json:"dy,omitempty"`
	Pressure float64 `json:"pressure,omitempty"`
	Buttons  int     `json:"buttons,omitempty"`
	Keycode  int     `json:"keycode,omitempty"`
	Repeat   int     `json:"repeat,omitempty"`
	Meta     int     `json:"meta,omitempty"`
	Text     string  `json:"text,omitempty"`
}

func (m *androidManager) Input(in androidInput) error {
	m.mu.Lock()
	sess := m.sess
	live := m.phase == androidLive
	m.mu.Unlock()
	if sess == nil || !live {
		return errors.New("沒有進行中的 Android 工作階段")
	}
	ctrl := sess.Control()
	if ctrl == nil {
		return errors.New("Android 控制通道未建立")
	}
	info := sess.Info()
	w, h := info.Width, info.Height
	if w <= 0 || h <= 0 {
		return errors.New("Android 畫面尺寸未知")
	}
	clamp := func(v, lo, hi int) int {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	x, y := clamp(in.X, 0, w-1), clamp(in.Y, 0, h-1)

	switch in.Kind {
	case "touch":
		pressure := float32(in.Pressure)
		if pressure <= 0 {
			pressure = 1
		}
		switch in.Action {
		case "down":
			return ctrl.Touch(scrcpy.TouchDown, scrcpy.PointerIDMouse, x, y, w, h, pressure, 1)
		case "move":
			return ctrl.Touch(scrcpy.TouchMove, scrcpy.PointerIDMouse, x, y, w, h, pressure, 1)
		case "up":
			return ctrl.Touch(scrcpy.TouchUp, scrcpy.PointerIDMouse, x, y, w, h, 0, 0)
		}
		return fmt.Errorf("未知的觸控動作 %q", in.Action)
	case "scroll":
		// scrcpy's scroll units run from -16 to 16; one wheel notch is a few.
		clampScroll := func(v float64) float32 {
			if v > 16 {
				return 16
			}
			if v < -16 {
				return -16
			}
			return float32(v)
		}
		return ctrl.Scroll(x, y, w, h, clampScroll(in.DX), clampScroll(in.DY))
	case "key":
		action := scrcpy.KeyDown
		if in.Action == "up" {
			action = scrcpy.KeyUp
		}
		return ctrl.Key(action, in.Keycode, in.Repeat, in.Meta)
	case "text":
		return ctrl.Text(in.Text)
	case "back":
		if err := ctrl.BackOrScreenOn(scrcpy.KeyDown); err != nil {
			return err
		}
		return ctrl.BackOrScreenOn(scrcpy.KeyUp)
	case "home":
		if err := ctrl.Key(scrcpy.KeyDown, keycodeHome, 0, 0); err != nil {
			return err
		}
		return ctrl.Key(scrcpy.KeyUp, keycodeHome, 0, 0)
	case "appswitch":
		if err := ctrl.Key(scrcpy.KeyDown, keycodeAppSwitch, 0, 0); err != nil {
			return err
		}
		return ctrl.Key(scrcpy.KeyUp, keycodeAppSwitch, 0, 0)
	case "rotate":
		return ctrl.Rotate()
	}
	return fmt.Errorf("未知的輸入類型 %q", in.Kind)
}

var (
	serialPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	addressPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+:[0-9]{1,5}$`)
)

func validAndroidSerial(serial string) error {
	if !serialPattern.MatchString(serial) || strings.HasPrefix(serial, "-") {
		return errors.New("無效的裝置序號")
	}
	return nil
}

func validAndroidAddress(address string) error {
	if !addressPattern.MatchString(address) || strings.HasPrefix(address, "-") {
		return errors.New("無效的無線位址，格式應為 IP:連接埠")
	}
	_, portText, ok := strings.Cut(address, ":")
	if !ok {
		return errors.New("無效的無線位址，格式應為 IP:連接埠")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("無線位址的連接埠必須在 1 到 65535 之間")
	}
	return nil
}

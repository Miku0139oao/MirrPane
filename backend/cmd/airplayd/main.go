// airplayd is the Go sidecar process. It advertises this machine as an AirPlay
// receiver, answers the control endpoints, receives screen-mirroring streams and
// decodes them.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/audioout"
	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/decode"
	"github.com/miku0139/airplay-go/backend/internal/frames"
	"github.com/miku0139/airplay-go/backend/internal/ipc"
	"github.com/miku0139/airplay-go/backend/internal/mdns"
	"github.com/miku0139/airplay-go/backend/internal/mirror"
	"github.com/miku0139/airplay-go/backend/internal/pairing"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
	"github.com/miku0139/airplay-go/backend/internal/receiver"
)

// frameCount counts received access units, for the first-frames log line.
var frameCount atomic.Uint64

// audioFrames counts decoded audio access units.
var audioFrames atomic.Uint64

// noteLog carries the startup facts — the bound ports and the advertised name.
// These are printed whether or not verbose logging is on, because they are the
// lines a bug report needs; -log sends them to a file as well.
var noteLog = log.New(os.Stderr, "", log.LstdFlags)

// note prints one startup fact to the console and, when a log file is in use,
// to the file too.
func note(format string, args ...any) {
	noteLog.Printf(format, args...)
}

func main() {
	var (
		name    = flag.String("name", "鏡屏", "receiver name shown on iPhone/iPad")
		dataDir = flag.String("data", "", "folder for settings and keys (default: config beside the executable)")
		port    = flag.Int("port", 0, "control port (0 prefers 7000, else any free)")
		apiPort = flag.Int("api-port", 0, "localhost API port for the UI (0 picks a free one)")
		width   = flag.Int("width", 0, "virtual display width (0 uses config.json)")
		height  = flag.Int("height", 0, "virtual display height (0 uses config.json)")
		maxFPS  = flag.Int("fps", 0, "max frames per second announced to the sender (0 uses config.json)")
		record  = flag.String("record", "", "save the mirrored H.264 stream to this folder for offline replay")
		shmName = flag.String("shm", `Local\YingpingFrames`, "shared-memory section the UI reads frames from")
		logFile = flag.String("log", "", "also write the log to this file (implies -v)")
		parent  = flag.Int("parent-pid", 0, "exit when this process exits (set by the UI)")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	// The shipped build has no console (see build.ps1), so a log file is the only
	// evidence a run leaves behind: it is on by default and -log overrides where
	// it goes. Naming it on the command line also turns on the chatty lines.
	chatty := *verbose || *logFile != ""
	logPath := *logFile
	if logPath == "" {
		logPath = defaultLogPath()
	}
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
				defer f.Close()
				// A piped stderr with no reader would eventually block every
				// write, and the logger's mutex would freeze the whole process.
				var w io.Writer = f
				if hasConsole() {
					w = io.MultiWriter(f, tolerantWriter{os.Stderr})
				}
				log.SetOutput(w)
				log.SetFlags(log.LstdFlags | log.Lmicroseconds)
				noteLog.SetOutput(w)
			}
		}
	}
	if !chatty && logPath == "" {
		log.SetOutput(devNull{})
	}

	watchParent(*parent)

	ip := protocol.LocalIPv4()
	if ip == "" && !chatty {
		// Logging is off, but an unusable network is worth saying out loud.
		fmt.Println("error: no usable LAN interface")
		os.Exit(2)
	}

	dataFolder := *dataDir
	if dataFolder == "" {
		dataFolder = defaultDataDir()
	}
	cfg, err := config.Load(dataFolder, *name)
	if err != nil {
		note("error: %v", err)
		os.Exit(1)
	}
	keys, err := pairing.KeysFromSeed(mustSeed(cfg))
	if err != nil {
		note("error: %v", err)
		os.Exit(1)
	}

	info := protocol.NewDeviceInfo(cfg.Name, cfg.DeviceID)
	info.PairingID = cfg.PairingID
	info.PublicKey = keys.PublicKeyHex()

	// Optional capture for offline replay; only active with -record. The sink
	// closes it, together with the decoder and the frame section.
	var rec *streamRecorder
	if *record != "" {
		rec, err = newStreamRecorder(*record)
		if err != nil {
			note("error: %v", err)
			os.Exit(1)
		}
	}

	// Effective display & runtime settings come from config.json unless
	// explicitly overridden on the command line.
	effWidth := cfg.Width
	if *width > 0 {
		effWidth = *width
	}
	effHeight := cfg.Height
	if *height > 0 {
		effHeight = *height
	}
	effFPS := cfg.MaxFPS
	if *maxFPS > 0 {
		effFPS = *maxFPS
	}

	// The frame section is created before the UI starts so the UI can attach to
	// a name that already exists.
	pub, err := frames.NewPublisher(*shmName, effWidth, effHeight)
	if err != nil {
		note("error: %v", err)
		os.Exit(1)
	}

	// The port is advertised over mDNS, so clients follow whatever we bind. Port
	// 7000 is the AirPlay convention; any free port works when it is taken.
	controlPort, err := listenControl(*port)
	if err != nil {
		note("error: %v", err)
		os.Exit(1)
	}
	defer controlPort.Close()
	port = &controlPort.port

	srv := ipc.New(info, ipc.Display{
		Width:     effWidth,
		Height:    effHeight,
		MaxFPS:    effFPS,
		RefreshHz: float64(effFPS),
	})

	// First-run setup is reported over stdout while the localhost API is not
	// ready yet. Flutter keeps the startup screen responsive during a download.
	startup := func(message string, done, failed bool) {
		note("ffmpeg: %s", message)
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Event   string `json:"event"`
			Message string `json:"message"`
			Done    bool   `json:"done"`
			Failed  bool   `json:"failed"`
		}{"ffmpeg_setup", message, done, failed})
	}
	startup("正在偵測 FFmpeg…", false, false)
	executable, err := os.Executable()
	if err != nil {
		startup(err.Error(), true, true)
		os.Exit(1)
	}
	// The downloader enforces a 30s idle timeout per read and retries resumable
	// ranges, so the outer cap only bounds the whole provisioning step.
	setupContext, cancelSetup := context.WithTimeout(context.Background(), 30*time.Minute)
	runtimeDir, setupErr := decode.PrepareRuntime(setupContext, filepath.Join(filepath.Dir(executable), "ffmpeg"), func(message string) {
		startup(message, false, false)
	})
	cancelSetup()
	if setupErr != nil {
		startup("FFmpeg 初始化失敗："+setupErr.Error(), true, true)
		os.Exit(1)
	}
	note("ffmpeg: runtime %s", runtimeDir)
	startup("正在初始化解碼引擎與 AirPlay 廣播…", true, false)

	// Resolve version/symbol mismatches before advertising the receiver.
	dec, err := decode.NewDecoderWithOptions(decode.Options{
		PreferHardware: cfg.DecoderMode != "software",
		ColorRange:     cfg.ColorRange,
		ScaleFilter:    cfg.ScaleFilter,
	})
	if err != nil {
		startup("FFmpeg 載入失敗："+err.Error(), true, true)
		note("error: %v", err)
		os.Exit(1)
	}

	note("decode: FFmpeg %s", decode.Version())
	sink := newFrameSink(dec, pub, rec)
	state := newRuntimeState()

	// Audio goes straight to the default output device. A machine with no sound
	// card still mirrors the screen, so a failure here is reported and skipped.
	speaker, err := audioout.OpenWithProfile(cfg.AudioBuffer)
	if err != nil {
		note("audio: %v (mirroring without sound)", err)
	}
	if speaker != nil {
		defer speaker.Close()
	}

	var recv *receiver.Receiver
	recv, err = receiver.New(info, keys, log.Default(), receiver.Events{
		OnFrame: func(f *mirror.Frame) {
			n := frameCount.Add(1)
			sink.Handle(f)
			if !chatty {
				return
			}
			switch {
			case n <= 5:
				log.Printf("video: #%d ntp=%d dur=%d %s %d bytes %v",
					n, f.NTP, f.Duration, f.Codec, len(f.Data), f.Valid)
			case n%60 == 0 && recv != nil:
				b, fr := recv.Stats()
				w, h := sink.Size()
				log.Printf("video: %d frames, %.1f MiB, %s, last %d KiB %v, decoded %dx%d (%d pictures)",
					fr, float64(b)/(1<<20), f.Codec, len(f.Data)/(1<<10), f.Valid, w, h, sink.Frames())
			}
		},
		OnGeometry: func(s mirror.Size) {
			log.Printf("video: display %dx%d, source %dx%d, offset %d,%d",
				s.Width, s.Height, s.SourceWidth, s.SourceHeight, s.XOffset, s.YOffset)
		},
		OnAudio: func(f *decode.AudioFrame) {
			if speaker != nil {
				speaker.Write(f.Samples)
			}
			n := audioFrames.Add(1)
			if !chatty {
				return
			}
			if n <= 5 || n%200 == 0 {
				log.Printf("audio: #%d %d frames, %d ch, %d Hz", n, f.Frames(), f.Channels, f.SampleRate)
			}
		},
		OnState: func(s receiver.Status, reason string) {
			state.set(s, reason)
			log.Printf("state: %v (%s)", s, reason)
			if s == receiver.StatusInterrupted && recv != nil {
				// A silent session needs a verdict: were there packets at all?
				got, dropped, decoded := recv.AudioStats()
				log.Printf("audio: stream ended with %d packet(s), %d dropped, %d decoded",
					got, dropped, decoded)
			}
		},
	})
	if err != nil {
		note("error: %v", err)
		os.Exit(1)
	}
	// Deferred last-first: the receiver stops the producers, then the sink waits
	// for any frame still in flight before freeing what it touches.
	defer sink.Close()
	defer recv.Close()

	srv.OnFPSetup = recv.FPSetup
	srv.OnPairSetup = recv.PairSetup
	srv.OnPairVerify = recv.PairVerify
	srv.OnSetup = recv.Setup
	srv.OnTeardown = recv.Teardown
	srv.OnDisconnect = recv.DropSession
	srv.OnStop = recv.DropAllSessions

	if err := srv.Serve(controlPort.ln); err != nil {
		note("error: %v", err)
		os.Exit(1)
	}
	note("control: %s", srv.Addr())

	// The advertised name is the stored one, not the flag default: -name only
	// seeds config.json on first run. Advertising the flag meant a renamed
	// receiver kept announcing its old name, and a sender kept matching the
	// device it had already paired with.
	adv := mdns.New(cfg.Name, *port, info)
	if err := adv.Start(); err != nil {
		note("error: %v", err)
		os.Exit(1)
	}
	defer adv.Stop()
	// /info?txtAirPlay must answer with the record we actually published.
	srv.TXTRecord = adv.TXTRecord
	note("advertising: %s", info.Name)
	note("ip: %s", ip)

	// Localhost control API for the Flutter UI. Bound to 127.0.0.1 only.
	apiLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *apiPort))
	if err != nil {
		note("error: %v", err)
		os.Exit(1)
	}
	shotsDir := filepath.Join(filepath.Dir(dataFolder), "screenshots")
	api := &http.Server{
		Handler:           apiHandler(info, controlPort.port, cfg, state, sink, recv, speaker, shotsDir),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := api.Serve(apiLn); err != nil && err != http.ErrServerClosed {
			log.Printf("api: %v", err)
		}
	}()
	defer api.Close()
	note("api: %s", apiLn.Addr().String())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	note("stopped")
}

// boundControl is the control listener together with the port it took.
type boundControl struct {
	ln   net.Listener
	port int
}

func (b *boundControl) Close() error { return b.ln.Close() }

// listenControl binds the control port. A requested port is honoured strictly;
// when none is requested it tries the AirPlay default before falling back to an
// ephemeral one, so two receivers on one machine can still coexist.
//
// The host part is empty on purpose: that makes the socket dual-stack, and an
// iPhone on an IPv6-capable network reaches this port over either family. Every
// other port the receiver reports follows the same rule.
func listenControl(requested int) (*boundControl, error) {
	candidates := []int{requested}
	if requested == 0 {
		candidates = []int{7000, 0}
	}
	var lastErr error
	for _, p := range candidates {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			lastErr = err
			continue
		}
		return &boundControl{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}, nil
	}
	return nil, fmt.Errorf("no control port available: %w", lastErr)
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// tolerantWriter drops write errors. A GUI-subsystem build has no console, so
// os.Stderr refuses writes; io.MultiWriter stops at the first failing writer, and
// without this the log file would lose every line that also went to stderr.
type tolerantWriter struct{ w io.Writer }

func (t tolerantWriter) Write(p []byte) (int, error) {
	n, _ := t.w.Write(p)
	return n, nil
}

// defaultDataDir keeps every file the receiver writes next to the executable,
// so the product can be moved by copying its folder.
func defaultDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "config"
	}
	return filepath.Join(filepath.Dir(exe), "config")
}

// defaultLogPath is logs/airplayd.log beside the executable, matching the layout
// in the plan. An empty result means there is nowhere sensible to write, in
// which case the caller leaves logging on the console.
func defaultLogPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "logs", "airplayd.log")
}

// mustSeed decodes the persisted pairing identity or stops the process.
func mustSeed(cfg *config.Config) []byte {
	seed, err := cfg.Seed()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
	return seed
}

package scrcpy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/adb"
)

func TestEmbeddedServerJar(t *testing.T) {
	if ServerVersion != "5.0.1" {
		t.Fatalf("ServerVersion = %q, want 5.0.1", ServerVersion)
	}
	if len(ServerJar) < 10000 {
		t.Fatalf("ServerJar unexpectedly small: %d bytes", len(ServerJar))
	}
}

func TestControlMessageGoldenBytes(t *testing.T) {
	t.Run("Key", func(t *testing.T) {
		var buf bytes.Buffer
		ctrl := newController(&buf)
		if err := ctrl.Key(KeyDown, 4, 0, 0); err != nil {
			t.Fatalf("Key down: %v", err)
		}
		if err := ctrl.Key(KeyUp, 26, 2, 0x1001); err != nil {
			t.Fatalf("Key up: %v", err)
		}

		want := []byte{
			// KeyDown, keycode=4 (KEYCODE_BACK), repeat=0, meta=0
			0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			// KeyUp, keycode=26 (KEYCODE_POWER), repeat=2, meta=0x1001
			0x00, 0x01, 0x00, 0x00, 0x00, 0x1a, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x10, 0x01,
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("Key golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), want)
		}
	})

	t.Run("Text and UTF-8 truncation", func(t *testing.T) {
		var buf bytes.Buffer
		ctrl := newController(&buf)
		if err := ctrl.Text("Hello"); err != nil {
			t.Fatalf("Text: %v", err)
		}
		want := []byte{
			0x01,
			0x00, 0x00, 0x00, 0x05,
			'H', 'e', 'l', 'l', 'o',
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("Text golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), want)
		}

		// 100 4-byte emoji characters = 400 bytes -> truncated to <= 300 bytes on rune boundary (75 * 4 = 300).
		// Add a 1-byte prefix so 297 bytes fit (1 + 74*4 = 297, never splitting a 4-byte rune).
		long := "a" + strings.Repeat("🚀", 100)
		buf.Reset()
		if err := ctrl.Text(long); err != nil {
			t.Fatalf("Text long: %v", err)
		}
		got := buf.Bytes()
		n := int(binary.BigEndian.Uint32(got[1:5]))
		if n != 297 {
			t.Fatalf("truncated length = %d, want 297", n)
		}
		if len(got) != 5+297 {
			t.Fatalf("total encoded len = %d, want %d", len(got), 5+297)
		}
	})

	t.Run("Touch finger and mouse", func(t *testing.T) {
		var buf bytes.Buffer
		ctrl := newController(&buf)
		if err := ctrl.Touch(TouchDown, PointerIDGenericFinger, 540, 1200, 1080, 2400, 1.0, 0); err != nil {
			t.Fatalf("Touch down: %v", err)
		}
		wantFinger := []byte{
			0x02,                                           // type = TYPE_INJECT_TOUCH_EVENT
			0x00,                                           // action = AMOTION_EVENT_ACTION_DOWN
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe, // pointerId = -2
			0x00, 0x00, 0x02, 0x1c, // x = 540
			0x00, 0x00, 0x04, 0xb0, // y = 1200
			0x04, 0x38, // screenW = 1080
			0x09, 0x60, // screenH = 2400
			0xff, 0xff, // pressure = 1.0 -> 0xffff
			0x00, 0x00, 0x00, 0x00, // actionButton = 0
			0x00, 0x00, 0x00, 0x00, // buttons = 0
		}
		if !bytes.Equal(buf.Bytes(), wantFinger) {
			t.Fatalf("Touch finger golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), wantFinger)
		}

		buf.Reset()
		if err := ctrl.Touch(TouchMove, PointerIDMouse, 10, 20, 1080, 2400, 0.5, 1); err != nil {
			t.Fatalf("Touch mouse move: %v", err)
		}
		wantMouseMove := []byte{
			0x02,                                           // type
			0x02,                                           // action = MOVE
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // pointerId = -1
			0x00, 0x00, 0x00, 0x0a, // x = 10
			0x00, 0x00, 0x00, 0x14, // y = 20
			0x04, 0x38, // 1080
			0x09, 0x60, // 2400
			0x80, 0x00, // pressure = 0.5 -> 0x8000
			0x00, 0x00, 0x00, 0x00, // actionButton = 0 on move
			0x00, 0x00, 0x00, 0x01, // buttons = 1
		}
		if !bytes.Equal(buf.Bytes(), wantMouseMove) {
			t.Fatalf("Touch mouse move golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), wantMouseMove)
		}

		buf.Reset()
		if err := ctrl.Touch(TouchUp, PointerIDMouse, 10, 20, 1080, 2400, 0.0, 1); err != nil {
			t.Fatalf("Touch mouse up: %v", err)
		}
		wantMouseUp := []byte{
			0x02,
			0x01,
			0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
			0x00, 0x00, 0x00, 0x0a,
			0x00, 0x00, 0x00, 0x14,
			0x04, 0x38,
			0x09, 0x60,
			0x00, 0x00,
			0x00, 0x00, 0x00, 0x01, // actionButton = 1 on up
			0x00, 0x00, 0x00, 0x01, // buttons = 1
		}
		if !bytes.Equal(buf.Bytes(), wantMouseUp) {
			t.Fatalf("Touch mouse up golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), wantMouseUp)
		}
	})

	t.Run("Scroll", func(t *testing.T) {
		var buf bytes.Buffer
		ctrl := newController(&buf)
		if err := ctrl.Scroll(500, 800, 1080, 2400, 1.0, -16.0); err != nil {
			t.Fatalf("Scroll: %v", err)
		}
		want := []byte{
			0x03,                   // type = TYPE_INJECT_SCROLL_EVENT
			0x00, 0x00, 0x01, 0xf4, // x = 500
			0x00, 0x00, 0x03, 0x20, // y = 800
			0x04, 0x38, // screenW = 1080
			0x09, 0x60, // screenH = 2400
			0x08, 0x00, // dx = 1.0 -> norm 1/16 -> 0x0800
			0x80, 0x00, // dy = -16.0 -> norm -1.0 -> -32768 (0x8000)
			0x00, 0x00, 0x00, 0x00, // buttons = 0
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("Scroll golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), want)
		}

		buf.Reset()
		if err := ctrl.Scroll(0, 0, 1080, 2400, 25.0, 0); err != nil {
			t.Fatalf("Scroll clamp: %v", err)
		}
		if got := buf.Bytes()[13:15]; !bytes.Equal(got, []byte{0x7f, 0xff}) {
			t.Fatalf("clamped +dx = %x, want 7fff", got)
		}
	})

	t.Run("BackOrScreenOn Rotate Expand Collapse", func(t *testing.T) {
		var buf bytes.Buffer
		ctrl := newController(&buf)
		if err := ctrl.BackOrScreenOn(KeyDown); err != nil {
			t.Fatalf("BackOrScreenOn down: %v", err)
		}
		if err := ctrl.BackOrScreenOn(KeyUp); err != nil {
			t.Fatalf("BackOrScreenOn up: %v", err)
		}
		if err := ctrl.ExpandNotificationPanel(); err != nil {
			t.Fatalf("ExpandNotificationPanel: %v", err)
		}
		if err := ctrl.CollapsePanels(); err != nil {
			t.Fatalf("CollapsePanels: %v", err)
		}
		if err := ctrl.Rotate(); err != nil {
			t.Fatalf("Rotate: %v", err)
		}

		want := []byte{
			0x04, 0x00, // BackOrScreenOn Down
			0x04, 0x01, // BackOrScreenOn Up
			0x05, // ExpandNotificationPanel
			0x07, // CollapsePanels
			0x0b, // Rotate
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("Misc control golden mismatch:\n got: %x\nwant: %x", buf.Bytes(), want)
		}
	})
}

func TestControllerConcurrentWritesNoInterleaving(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	ctrl := newController(c1)
	const workers = 8
	const perWorker = 25

	errCh := make(chan error, 1)
	go func() {
		for i := 0; i < workers*perWorker; i++ {
			var hdr [1]byte
			if _, err := io.ReadFull(c2, hdr[:]); err != nil {
				errCh <- err
				return
			}
			switch hdr[0] {
			case controlMsgTypeInjectTouchEvent:
				var rest [31]byte
				if _, err := io.ReadFull(c2, rest[:]); err != nil {
					errCh <- err
					return
				}
			case controlMsgTypeInjectKeycode:
				var rest [13]byte
				if _, err := io.ReadFull(c2, rest[:]); err != nil {
					errCh <- err
					return
				}
			case controlMsgTypeRotateDevice:
			default:
				errCh <- fmt.Errorf("unexpected interleaved type byte 0x%02x at msg %d", hdr[0], i)
				return
			}
		}
		errCh <- nil
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				var err error
				switch (id + j) % 3 {
				case 0:
					err = ctrl.Touch(TouchMove, uint64(id), j, j*2, 1080, 2400, 0.5, 0)
				case 1:
					err = ctrl.Key(KeyDown, 66, 0, 0)
				case 2:
					err = ctrl.Rotate()
				}
				if err != nil {
					t.Errorf("worker %d write error: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := <-errCh; err != nil {
		t.Fatalf("reader failed: %v", err)
	}
}

type chunkedReader struct {
	r     io.Reader
	chunk int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(p) > c.chunk {
		p = p[:c.chunk]
	}
	return c.r.Read(p)
}

func buildHandshakeBytes(name string, codecID uint32, w, h int) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x00) // dummy byte

	var nameBuf [64]byte
	copy(nameBuf[:], name)
	buf.Write(nameBuf[:])

	var codecBuf [4]byte
	binary.BigEndian.PutUint32(codecBuf[:], codecID)
	buf.Write(codecBuf[:])

	var sessHdr [12]byte
	binary.BigEndian.PutUint32(sessHdr[0:4], uint32(packetFlagSession>>32))
	binary.BigEndian.PutUint32(sessHdr[4:8], uint32(w))
	binary.BigEndian.PutUint32(sessHdr[8:12], uint32(h))
	buf.Write(sessHdr[:])
	return buf.Bytes()
}

func appendVideoPacket(buf *bytes.Buffer, pts uint64, config, keyframe bool, payload []byte) {
	var hdr [12]byte
	ptsFlags := pts & packetPTSMask
	if config {
		ptsFlags = packetFlagConfig
	} else if keyframe {
		ptsFlags |= packetFlagKeyFrame
	}
	binary.BigEndian.PutUint64(hdr[0:8], ptsFlags)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(payload)))
	buf.Write(hdr[:])
	buf.Write(payload)
}

func appendSessionPacket(buf *bytes.Buffer, w, h int, clientResized bool) {
	var hdr [12]byte
	flags := uint32(packetFlagSession >> 32)
	if clientResized {
		flags |= 1
	}
	binary.BigEndian.PutUint32(hdr[0:4], flags)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(w))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(h))
	buf.Write(hdr[:])
}

func TestVideoParserBytesReaderSplitReads(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(buildHandshakeBytes("Pixel 8 Pro", codecIDH264, 1080, 2400))

	spsPPS := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x1e}
	idr := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x21}
	pframe := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x9a, 0x22}
	rotatedIDR := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xaa, 0xbb}

	appendVideoPacket(&stream, 99999, true, false, spsPPS)
	appendVideoPacket(&stream, 123456, false, true, idr)
	appendVideoPacket(&stream, 140123, false, false, pframe)
	appendSessionPacket(&stream, 2400, 1080, false)
	appendVideoPacket(&stream, 200000, false, true, rotatedIDR)

	// Force tiny 3-byte reads across headers and payloads.
	r := &chunkedReader{r: bytes.NewReader(stream.Bytes()), chunk: 3}

	if err := readDummyByte(r); err != nil {
		t.Fatalf("readDummyByte: %v", err)
	}
	name, err := readDeviceName(r)
	if err != nil || name != "Pixel 8 Pro" {
		t.Fatalf("readDeviceName = (%q, %v), want (Pixel 8 Pro, nil)", name, err)
	}
	codec, err := readCodecID(r)
	if err != nil || codec != "h264" {
		t.Fatalf("readCodecID = (%q, %v), want (h264, nil)", codec, err)
	}
	w, h, err := readInitialSessionMeta(r)
	if err != nil || w != 1080 || h != 2400 {
		t.Fatalf("readInitialSessionMeta = (%d, %d, %v), want (1080, 2400, nil)", w, h, err)
	}

	var updatedW, updatedH int
	onSess := func(nw, nh int) { updatedW, updatedH = nw, nh }

	p1, err := readVideoPacket(r, onSess)
	if err != nil {
		t.Fatalf("pkt 1: %v", err)
	}
	if !p1.Config || p1.Keyframe || p1.PTS != 0 || !bytes.Equal(p1.Data, spsPPS) {
		t.Fatalf("unexpected config pkt: %+v", p1)
	}

	p2, err := readVideoPacket(r, onSess)
	if err != nil {
		t.Fatalf("pkt 2: %v", err)
	}
	if p2.Config || !p2.Keyframe || p2.PTS != 123456 || !bytes.Equal(p2.Data, idr) {
		t.Fatalf("unexpected keyframe pkt: %+v", p2)
	}

	p3, err := readVideoPacket(r, onSess)
	if err != nil {
		t.Fatalf("pkt 3: %v", err)
	}
	if p3.Config || p3.Keyframe || p3.PTS != 140123 || !bytes.Equal(p3.Data, pframe) {
		t.Fatalf("unexpected p-frame pkt: %+v", p3)
	}

	p4, err := readVideoPacket(r, onSess)
	if err != nil {
		t.Fatalf("pkt 4 after resize: %v", err)
	}
	if updatedW != 2400 || updatedH != 1080 {
		t.Fatalf("mid-stream resize = %dx%d, want 2400x1080", updatedW, updatedH)
	}
	if !p4.Keyframe || p4.PTS != 200000 || !bytes.Equal(p4.Data, rotatedIDR) {
		t.Fatalf("unexpected post-resize pkt: %+v", p4)
	}

	if _, err := readVideoPacket(r, onSess); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF at end of stream, got: %v", err)
	}
}

func TestVideoCodecErrorsAndFull64ByteName(t *testing.T) {
	// Exactly 64 non-NUL bytes
	fullName := strings.Repeat("A", 64)
	got, err := readDeviceName(bytes.NewReader([]byte(fullName)))
	if err != nil || got != fullName {
		t.Fatalf("64-byte name = (%q, %v)", got, err)
	}

	for _, tc := range []struct {
		name string
		id   uint32
	}{
		{"disabled", codecIDStreamDisabled},
		{"config_error", codecIDConfigError},
		{"h265_unsupported", 0x68323635},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], tc.id)
			if _, err := readCodecID(bytes.NewReader(b[:])); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSessionPipeDrainAndConcurrentClose(t *testing.T) {
	vClient, vServer := net.Pipe()
	cClient, cServer := net.Pipe()

	sess := newSessionFromConns(vClient, cClient, Info{
		DeviceName: "TestDevice",
		Codec:      "h264",
		Width:      1080,
		Height:     1920,
	})

	// Server pushes clipboard, ack_clipboard, and uhid_output messages on control socket,
	// plus one video frame and a mid-stream resize before blocking.
	go func() {
		// Clipboard msg: type=0, len=5, "hello"
		_, _ = cServer.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'})
		// Ack clipboard: type=1, seq=42
		_, _ = cServer.Write([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2a})
		// UHID output: type=2, id=1, size=3, [1,2,3]
		_, _ = cServer.Write([]byte{0x02, 0x00, 0x01, 0x00, 0x03, 0x01, 0x02, 0x03})
		// Drain incoming client control messages until closed.
		_, _ = io.Copy(io.Discard, cServer)
	}()

	var pktBuf bytes.Buffer
	appendSessionPacket(&pktBuf, 1920, 1080, true)
	appendVideoPacket(&pktBuf, 5000, false, true, []byte{0x00, 0x00, 0x00, 0x01, 0x65})

	go func() {
		_, _ = vServer.Write(pktBuf.Bytes())
	}()

	// Send control messages while device messages arrive; must not block.
	if err := sess.Control().Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := sess.Control().Text("ping"); err != nil {
		t.Fatalf("Text: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pkt, err := sess.ReadVideo(ctx)
	if err != nil {
		t.Fatalf("ReadVideo: %v", err)
	}
	if !pkt.Keyframe || pkt.PTS != 5000 {
		t.Fatalf("unexpected pkt: %+v", pkt)
	}
	if info := sess.Info(); info.Width != 1920 || info.Height != 1080 {
		t.Fatalf("Info not updated after session packet: %+v", info)
	}

	// Start a blocked ReadVideo, then Close() concurrently.
	readErrCh := make(chan error, 1)
	go func() {
		_, err := sess.ReadVideo(context.Background())
		readErrCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent second Close.
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_ = vServer.Close()
	_ = cServer.Close()

	if err := <-readErrCh; !errors.Is(err, io.EOF) {
		t.Fatalf("blocked ReadVideo returned %v, want io.EOF", err)
	}
	if err := sess.Control().Rotate(); err == nil {
		t.Fatal("expected error writing to closed controller")
	}
}

func TestStartAndCloseWithFakeADBAndMockServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "server-args.txt")
	removedFile := filepath.Join(dir, "forward-removed.txt")
	fakeADB := buildMockScrcpyADB(t, dir, port, argsFile, removedFile)

	client, err := adb.New(fakeADB)
	if err != nil {
		t.Fatalf("adb.New: %v", err)
	}

	// Mock scrcpy server accepting video then control sockets in DesktopConnection.open order.
	serverDone := make(chan error, 1)
	go func() {
		vConn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer vConn.Close()
		// Dummy byte on first socket only.
		if _, err := vConn.Write([]byte{0x00}); err != nil {
			serverDone <- err
			return
		}

		cConn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer cConn.Close()

		// 64-byte device name + 4-byte codec ("h264") + 12-byte session meta.
		var hs bytes.Buffer
		var name [64]byte
		copy(name[:], "Mock Android Phone")
		hs.Write(name[:])
		var cid [4]byte
		binary.BigEndian.PutUint32(cid[:], codecIDH264)
		hs.Write(cid[:])
		appendSessionPacket(&hs, 1080, 2340, false)
		appendVideoPacket(&hs, 0, true, false, []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42})
		appendVideoPacket(&hs, 33333, false, true, []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88})
		if _, err := vConn.Write(hs.Bytes()); err != nil {
			serverDone <- err
			return
		}

		// Read one control message (Rotate = 0x0b) from control socket.
		var ctrlByte [1]byte
		if _, err := io.ReadFull(cConn, ctrlByte[:]); err != nil {
			serverDone <- err
			return
		}
		if ctrlByte[0] != controlMsgTypeRotateDevice {
			serverDone <- fmt.Errorf("got control byte 0x%02x, want 0x0b", ctrlByte[0])
			return
		}
		serverDone <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := Start(ctx, client, Options{
		Serial:   "MOCK123",
		MaxSize:  1920,
		MaxFPS:   60,
		BitRate:  4000000,
		LogLevel: "debug",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	info := sess.Info()
	if info.DeviceName != "Mock Android Phone" || info.Codec != "h264" || info.Width != 1080 || info.Height != 2340 {
		t.Fatalf("unexpected Info: %+v", info)
	}

	p1, err := sess.ReadVideo(ctx)
	if err != nil || !p1.Config {
		t.Fatalf("ReadVideo config = (%+v, %v)", p1, err)
	}
	p2, err := sess.ReadVideo(ctx)
	if err != nil || !p2.Keyframe || p2.PTS != 33333 {
		t.Fatalf("ReadVideo keyframe = (%+v, %v)", p2, err)
	}

	if err := sess.Control().Rotate(); err != nil {
		t.Fatalf("Control().Rotate: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("mock server error: %v", err)
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify server args matched scrcpy v5.0.1 options.
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read server args: %v", err)
	}
	argsStr := string(rawArgs)
	for _, want := range []string{
		"CLASSPATH=/data/local/tmp/scrcpy-server.jar",
		"app_process / com.genymobile.scrcpy.Server 5.0.1",
		"video_codec=h264",
		"audio=false",
		"tunnel_forward=true",
		"max_size=1920",
		"max_fps=60",
		"video_bit_rate=4000000",
		"log_level=debug",
	} {
		if !strings.Contains(argsStr, want) {
			t.Errorf("server args missing %q in:\n%s", want, argsStr)
		}
	}

	// Verify Close removed tcp:<port> forward.
	removedRaw, err := os.ReadFile(removedFile)
	if err != nil {
		t.Fatalf("forward --remove was not called: %v", err)
	}
	if !strings.Contains(string(removedRaw), fmt.Sprintf("tcp:%d", port)) {
		t.Fatalf("removed forward %q does not contain tcp:%d", string(removedRaw), port)
	}
}

func TestStartFailureSurfacesServerLogsAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	removedFile := filepath.Join(dir, "removed.txt")
	fakeADB := buildMockScrcpyADB(t, dir, 45678, argsFile, removedFile)
	t.Setenv("MOCK_SCRCPY_CRASH", "1")

	client, err := adb.New(fakeADB)
	if err != nil {
		t.Fatalf("adb.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = Start(ctx, client, Options{Serial: "MOCK123"})
	if err == nil {
		t.Fatal("expected Start error when server crashes")
	}
	if !strings.Contains(err.Error(), "ERROR: Mock server boom") {
		t.Fatalf("expected server stderr tail in error, got: %v", err)
	}
	if _, statErr := os.Stat(removedFile); statErr != nil {
		t.Fatalf("expected forward cleanup on Start failure: %v", statErr)
	}
}

func buildMockScrcpyADB(t *testing.T, dir string, port int, argsFile, removedFile string) string {
	t.Helper()
	src := filepath.Join(dir, "mock_adb.go")
	bin := filepath.Join(dir, "adb")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	code := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) >= 3 && args[0] == "-s" {
		switch args[2] {
		case "push":
			if _, err := os.Stat(args[3]); err != nil {
				os.Stderr.WriteString(err.Error())
				os.Exit(1)
			}
			os.Exit(0)
		case "forward":
			if len(args) >= 4 && args[3] == "--list" {
				os.Exit(0)
			}
			if len(args) >= 4 && args[3] == "--remove" {
				_ = os.WriteFile(%q, []byte(strings.Join(args, " ")), 0o600)
				os.Exit(0)
			}
			if len(args) >= 4 && args[3] == "tcp:0" {
				fmt.Printf("%%d\n", %d)
				os.Exit(0)
			}
		case "shell":
			_ = os.WriteFile(%q, []byte(strings.Join(args[3:], " ")), 0o600)
			if os.Getenv("MOCK_SCRCPY_CRASH") == "1" {
				os.Stderr.WriteString("ERROR: Mock server boom\n")
				os.Exit(1)
			}
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
	}
	os.Stderr.WriteString("unexpected args: " + strings.Join(args, " "))
	os.Exit(2)
}
`, removedFile, port, argsFile)
	if err := os.WriteFile(src, []byte(code), 0o600); err != nil {
		t.Fatalf("write mock adb: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock adb: %v\n%s", err, out)
	}
	return bin
}

func TestRealDeviceScrcpyIntegration(t *testing.T) {
	if os.Getenv("MIRRPANE_ADB_DEVICE") != "1" {
		t.Skip("set MIRRPANE_ADB_DEVICE=1 to run real device integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := adb.New("")
	if err != nil {
		t.Fatalf("adb.New: %v", err)
	}
	devs, err := client.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	var serial string
	for _, d := range devs {
		if d.State == "device" {
			serial = d.Serial
			break
		}
	}
	if serial == "" {
		t.Fatal("no ready ADB device found")
	}

	sess, err := Start(ctx, client, Options{Serial: serial, MaxSize: 720, MaxFPS: 30})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	info := sess.Info()
	if info.Codec != "h264" || info.Width <= 0 || info.Height <= 0 {
		t.Fatalf("unexpected Info: %+v", info)
	}
	pkt, err := sess.ReadVideo(ctx)
	if err != nil || len(pkt.Data) == 0 {
		t.Fatalf("ReadVideo: pkt=%+v err=%v", pkt, err)
	}
}

func TestAudioHeaderAndPacket(t *testing.T) {
	var buf bytes.Buffer
	var codec [4]byte
	binary.BigEndian.PutUint32(codec[:], codecIDRaw)
	buf.Write(codec[:])
	var hdr [12]byte
	binary.BigEndian.PutUint64(hdr[0:8], 1234)
	binary.BigEndian.PutUint32(hdr[8:12], 8)
	buf.Write(hdr[:])
	buf.Write([]byte{1, 0, 2, 0, 3, 0, 4, 0})

	got, err := readAudioHeader(&buf)
	if err != nil || got != "raw" {
		t.Fatalf("audio header = %q, %v", got, err)
	}
	pkt, err := readAudioPacket(&buf)
	if err != nil || pkt.PTS != 1234 || len(pkt.Data) != 8 || pkt.Config {
		t.Fatalf("audio packet = %+v, %v", pkt, err)
	}

	// Codec id 0 disables audio; 1 is a configuration error.
	var disabled bytes.Buffer
	disabled.Write(make([]byte, 4))
	if codec, err := readAudioHeader(&disabled); err != nil || codec != "" {
		t.Fatalf("disabled stream = %q, %v", codec, err)
	}
	var broken bytes.Buffer
	binary.Write(&broken, binary.BigEndian, uint32(codecIDConfigError))
	if _, err := readAudioHeader(&broken); err == nil {
		t.Fatal("configuration error went unreported")
	}
}

func TestServerArgsRequestRawAudio(t *testing.T) {
	args := strings.Join(buildServerArgs(Options{Audio: true}, "ab12cd34", "info"), " ")
	if !strings.Contains(args, "audio=true") || !strings.Contains(args, "audio_codec=raw") {
		t.Fatalf("audio not requested: %s", args)
	}
	args = strings.Join(buildServerArgs(Options{}, "ab12cd34", "info"), " ")
	if !strings.Contains(args, "audio=false") || strings.Contains(args, "audio_codec=") {
		t.Fatalf("audio requested without the option: %s", args)
	}
}

package scrcpy

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"unicode/utf8"
)

const (
	controlMsgTypeInjectKeycode            byte = 0
	controlMsgTypeInjectText               byte = 1
	controlMsgTypeInjectTouchEvent         byte = 2
	controlMsgTypeInjectScrollEvent        byte = 3
	controlMsgTypeBackOrScreenOn           byte = 4
	controlMsgTypeExpandNotificationPanel  byte = 5
	controlMsgTypeExpandSettingsPanel      byte = 6
	controlMsgTypeCollapsePanels           byte = 7
	controlMsgTypeGetClipboard             byte = 8
	controlMsgTypeSetClipboard             byte = 9
	controlMsgTypeSetDisplayPower          byte = 10
	controlMsgTypeRotateDevice             byte = 11
	controlMsgTypeUhidCreate               byte = 12
	controlMsgTypeUhidInput                byte = 13
	controlMsgTypeUhidDestroy              byte = 14
	controlMsgTypeOpenHardKeyboardSettings byte = 15
	controlMsgTypeStartApp                 byte = 16
	controlMsgTypeResetVideo               byte = 17
	controlMsgTypeCameraSetTorch           byte = 18
	controlMsgTypeCameraZoomIn             byte = 19
	controlMsgTypeCameraZoomOut            byte = 20
	controlMsgTypeResizeDisplay            byte = 21
	controlMsgTypeScanFile                 byte = 22

	injectTextMaxLength = 300
)

// TouchAction represents an Android MotionEvent action for touch injection.
type TouchAction uint8

const (
	TouchDown TouchAction = 0
	TouchUp   TouchAction = 1
	TouchMove TouchAction = 2

	TouchActionDown = TouchDown
	TouchActionUp   = TouchUp
	TouchActionMove = TouchMove
)

// KeyAction represents an Android KeyEvent action for key injection.
type KeyAction uint8

const (
	KeyDown KeyAction = 0
	KeyUp   KeyAction = 1

	KeyActionDown = KeyDown
	KeyActionUp   = KeyUp
)

// Well-known scrcpy pointer IDs from control_msg.h.
const (
	PointerIDMouse         uint64 = ^uint64(0) // -1
	PointerIDGenericFinger uint64 = ^uint64(1) // -2
	PointerIDVirtualFinger uint64 = ^uint64(2) // -3
)

// Controller sends serialized scrcpy v5.0.1 control messages over the control socket.
// All methods are safe for concurrent use.
type Controller struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func newController(w io.Writer) *Controller {
	return &Controller{w: w}
}

func (c *Controller) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *Controller) writeMsg(buf []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.w == nil {
		return errors.New("scrcpy: controller is closed")
	}

	written := 0
	for written < len(buf) {
		n, err := c.w.Write(buf[written:])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}

// Touch injects a touch or mouse pointer event.
// In scrcpy v5.0.1 the wire format carries both actionButton and buttons;
// for TouchDown and TouchUp when buttons != 0, actionButton is set to buttons.
func (c *Controller) Touch(action TouchAction, pointerID uint64, x, y, screenW, screenH int, pressure float32, buttons int) error {
	actionButton := 0
	if buttons != 0 && (action == TouchDown || action == TouchUp) {
		actionButton = buttons
	}
	msg := encodeTouch(action, pointerID, x, y, screenW, screenH, pressure, actionButton, buttons)
	return c.writeMsg(msg[:])
}

// Scroll injects a scroll event at (x, y) within a screen of (screenW, screenH).
// dx and dy follow scrcpy's [-16, 16] unit range and are clamped accordingly.
func (c *Controller) Scroll(x, y, screenW, screenH int, dx, dy float32) error {
	msg := encodeScroll(x, y, screenW, screenH, dx, dy, 0)
	return c.writeMsg(msg[:])
}

// Key injects an Android KeyEvent.
func (c *Controller) Key(action KeyAction, keycode, repeat, meta int) error {
	msg := encodeKey(action, keycode, repeat, meta)
	return c.writeMsg(msg[:])
}

// Text injects UTF-8 text (truncated at a UTF-8 codepoint boundary to 300 bytes).
func (c *Controller) Text(text string) error {
	msg := encodeText(text)
	return c.writeMsg(msg)
}

// BackOrScreenOn injects BACK if the screen is on, or powers the screen on on KeyDown.
func (c *Controller) BackOrScreenOn(action KeyAction) error {
	msg := encodeBackOrScreenOn(action)
	return c.writeMsg(msg[:])
}

// Rotate sends a rotate-device control message.
func (c *Controller) Rotate() error {
	msg := encodeEmpty(controlMsgTypeRotateDevice)
	return c.writeMsg(msg[:])
}

// ExpandNotificationPanel expands the Android notification shade.
func (c *Controller) ExpandNotificationPanel() error {
	msg := encodeEmpty(controlMsgTypeExpandNotificationPanel)
	return c.writeMsg(msg[:])
}

// CollapsePanels collapses all expanded notification/settings panels.
func (c *Controller) CollapsePanels() error {
	msg := encodeEmpty(controlMsgTypeCollapsePanels)
	return c.writeMsg(msg[:])
}

func putPosition(dst []byte, x, y, screenW, screenH int) {
	if screenW < 0 {
		screenW = 0
	} else if screenW > math.MaxUint16 {
		screenW = math.MaxUint16
	}
	if screenH < 0 {
		screenH = 0
	} else if screenH > math.MaxUint16 {
		screenH = math.MaxUint16
	}
	binary.BigEndian.PutUint32(dst[0:4], uint32(int32(x)))
	binary.BigEndian.PutUint32(dst[4:8], uint32(int32(y)))
	binary.BigEndian.PutUint16(dst[8:10], uint16(screenW))
	binary.BigEndian.PutUint16(dst[10:12], uint16(screenH))
}

func floatToU16FP(f float32) uint16 {
	if math.IsNaN(float64(f)) || f <= 0 {
		return 0
	}
	if f >= 1 {
		return 0xffff
	}
	u := uint32(float64(f) * 65536.0)
	if u >= 0xffff {
		return 0xffff
	}
	return uint16(u)
}

func floatToI16FP(f float32) int16 {
	if math.IsNaN(float64(f)) {
		return 0
	}
	if f <= -1 {
		return -0x8000
	}
	if f >= 1 {
		return 0x7fff
	}
	i := int32(float64(f) * 32768.0)
	if i < -0x8000 {
		return -0x8000
	}
	if i >= 0x7fff {
		return 0x7fff
	}
	return int16(i)
}

func encodeTouch(action TouchAction, pointerID uint64, x, y, screenW, screenH int, pressure float32, actionButton, buttons int) [32]byte {
	var buf [32]byte
	buf[0] = controlMsgTypeInjectTouchEvent
	buf[1] = byte(action)
	binary.BigEndian.PutUint64(buf[2:10], pointerID)
	putPosition(buf[10:22], x, y, screenW, screenH)
	binary.BigEndian.PutUint16(buf[22:24], floatToU16FP(pressure))
	binary.BigEndian.PutUint32(buf[24:28], uint32(int32(actionButton)))
	binary.BigEndian.PutUint32(buf[28:32], uint32(int32(buttons)))
	return buf
}

func encodeScroll(x, y, screenW, screenH int, dx, dy float32, buttons int) [21]byte {
	var buf [21]byte
	buf[0] = controlMsgTypeInjectScrollEvent
	putPosition(buf[1:13], x, y, screenW, screenH)

	hNorm := float32(0)
	if !math.IsNaN(float64(dx)) {
		hNorm = dx / 16.0
	}
	vNorm := float32(0)
	if !math.IsNaN(float64(dy)) {
		vNorm = dy / 16.0
	}

	binary.BigEndian.PutUint16(buf[13:15], uint16(floatToI16FP(hNorm)))
	binary.BigEndian.PutUint16(buf[15:17], uint16(floatToI16FP(vNorm)))
	binary.BigEndian.PutUint32(buf[17:21], uint32(int32(buttons)))
	return buf
}

func encodeKey(action KeyAction, keycode, repeat, meta int) [14]byte {
	var buf [14]byte
	buf[0] = controlMsgTypeInjectKeycode
	buf[1] = byte(action)
	binary.BigEndian.PutUint32(buf[2:6], uint32(int32(keycode)))
	binary.BigEndian.PutUint32(buf[6:10], uint32(int32(repeat)))
	binary.BigEndian.PutUint32(buf[10:14], uint32(int32(meta)))
	return buf
}

func utf8TruncateLen(s string, maxBytes int) int {
	if len(s) <= maxBytes {
		return len(s)
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

func encodeText(text string) []byte {
	n := utf8TruncateLen(text, injectTextMaxLength)
	buf := make([]byte, 1+4+n)
	buf[0] = controlMsgTypeInjectText
	binary.BigEndian.PutUint32(buf[1:5], uint32(n))
	copy(buf[5:], text[:n])
	return buf
}

func encodeBackOrScreenOn(action KeyAction) [2]byte {
	return [2]byte{controlMsgTypeBackOrScreenOn, byte(action)}
}

func encodeEmpty(msgType byte) [1]byte {
	return [1]byte{msgType}
}

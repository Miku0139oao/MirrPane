package scrcpy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	codecIDH264           uint32 = 0x68323634 // "h264"
	codecIDStreamDisabled uint32 = 0x00000000
	codecIDConfigError    uint32 = 0x00000001

	packetFlagSession  uint64 = 1 << 63
	packetFlagConfig   uint64 = 1 << 62
	packetFlagKeyFrame uint64 = 1 << 61
	packetPTSMask      uint64 = packetFlagKeyFrame - 1

	deviceNameFieldLength = 64
	packetHeaderSize      = 12
	maxVideoPacketSize    = 64 << 20 // 64 MiB safety cap

	deviceMsgTypeClipboard    = 0
	deviceMsgTypeAckClipboard = 1
	deviceMsgTypeUhidOutput   = 2
	deviceMsgMaxSize          = 1 << 18 // 256 KiB
	deviceMsgClipboardMaxLen  = deviceMsgMaxSize - 5
)

func readDummyByte(r io.Reader) error {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return fmt.Errorf("read dummy byte: %w", err)
	}
	return nil
}

func readDeviceName(r io.Reader) (string, error) {
	var buf [deviceNameFieldLength]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return "", fmt.Errorf("read device name: %w", err)
	}
	if idx := bytes.IndexByte(buf[:], 0); idx >= 0 {
		return string(buf[:idx]), nil
	}
	return string(buf[:]), nil
}

func readCodecID(r io.Reader) (string, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return "", fmt.Errorf("read codec id: %w", err)
	}
	id := binary.BigEndian.Uint32(buf[:])
	switch id {
	case codecIDH264:
		return "h264", nil
	case codecIDStreamDisabled:
		return "", errors.New("video stream disabled by device")
	case codecIDConfigError:
		return "", errors.New("video stream configuration error on device")
	default:
		return "", fmt.Errorf("unsupported video codec id 0x%08x", id)
	}
}

func readPacketHeader(r io.Reader) ([packetHeaderSize]byte, error) {
	var hdr [packetHeaderSize]byte
	_, err := io.ReadFull(r, hdr[:])
	if err != nil {
		return hdr, err
	}
	return hdr, nil
}

func isSessionHeader(hdr [packetHeaderSize]byte) bool {
	return (hdr[0] & 0x80) != 0
}

func parseSessionHeader(hdr [packetHeaderSize]byte) (width, height int, clientResized bool, err error) {
	if !isSessionHeader(hdr) {
		return 0, 0, false, errors.New("expected session metadata header")
	}
	w := binary.BigEndian.Uint32(hdr[4:8])
	h := binary.BigEndian.Uint32(hdr[8:12])
	if w == 0 || h == 0 || w > 0x7fffffff || h > 0x7fffffff {
		return 0, 0, false, fmt.Errorf("invalid video dimensions %dx%d", w, h)
	}
	return int(w), int(h), (hdr[3] & 1) != 0, nil
}

func readInitialSessionMeta(r io.Reader) (width, height int, err error) {
	hdr, err := readPacketHeader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("read session metadata: %w", err)
	}
	w, h, _, err := parseSessionHeader(hdr)
	if err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

func readVideoPacket(r io.Reader, onSession func(width, height int)) (VideoPacket, error) {
	for {
		hdr, err := readPacketHeader(r)
		if err != nil {
			return VideoPacket{}, err
		}

		if isSessionHeader(hdr) {
			w, h, _, err := parseSessionHeader(hdr)
			if err != nil {
				return VideoPacket{}, err
			}
			if onSession != nil {
				onSession(w, h)
			}
			continue
		}

		ptsFlags := binary.BigEndian.Uint64(hdr[0:8])
		pktLen := binary.BigEndian.Uint32(hdr[8:12])
		if pktLen == 0 || pktLen > maxVideoPacketSize {
			return VideoPacket{}, fmt.Errorf("scrcpy: invalid video packet size %d", pktLen)
		}

		data := make([]byte, pktLen)
		if _, err := io.ReadFull(r, data); err != nil {
			if errors.Is(err, io.EOF) {
				return VideoPacket{}, io.ErrUnexpectedEOF
			}
			return VideoPacket{}, err
		}

		isConfig := (ptsFlags & packetFlagConfig) != 0
		isKeyframe := (ptsFlags & packetFlagKeyFrame) != 0
		var pts uint64
		if !isConfig {
			pts = ptsFlags & packetPTSMask
		}

		return VideoPacket{
			Data:     data,
			PTS:      pts,
			Config:   isConfig,
			Keyframe: isKeyframe,
		}, nil
	}
}

func drainDeviceMessages(r io.Reader) error {
	var typeBuf [1]byte
	for {
		if _, err := io.ReadFull(r, typeBuf[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		switch typeBuf[0] {
		case deviceMsgTypeClipboard:
			var lenBuf [4]byte
			if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(lenBuf[:])
			if n > deviceMsgClipboardMaxLen {
				return fmt.Errorf("scrcpy: clipboard message too large (%d bytes)", n)
			}
			if n > 0 {
				if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
					return err
				}
			}

		case deviceMsgTypeAckClipboard:
			var seqBuf [8]byte
			if _, err := io.ReadFull(r, seqBuf[:]); err != nil {
				return err
			}

		case deviceMsgTypeUhidOutput:
			var hdr [4]byte // 2-byte id + 2-byte size
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return err
			}
			size := binary.BigEndian.Uint16(hdr[2:4])
			if size > 0 {
				if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
					return err
				}
			}

		default:
			return fmt.Errorf("scrcpy: unknown device message type %d", typeBuf[0])
		}
	}
}

// Audio codec identifiers scrcpy writes as the audio stream's four-byte header.
const (
	codecIDOpus uint32 = 0x6f707573 // "opus"
	codecIDAAC  uint32 = 0x00616163 // "\x00aac"
	codecIDFLAC uint32 = 0x666c6163 // "flac"
	codecIDRaw  uint32 = 0x00726177 // "\x00raw"
)

const maxAudioPacketSize = 1 << 20 // 1 MiB safety cap (raw PCM chunks are tiny)

// readAudioHeader reads the audio stream's four-byte codec id. "" means the
// device disabled the audio stream (it could not capture it); an error is
// returned only when the stream cannot be used.
func readAudioHeader(r io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("read audio codec id: %w", err)
	}
	switch id := binary.BigEndian.Uint32(b[:]); id {
	case codecIDStreamDisabled:
		return "", nil
	case codecIDConfigError:
		return "", errors.New("scrcpy: server reported an audio configuration error")
	case codecIDOpus:
		return "opus", nil
	case codecIDAAC:
		return "aac", nil
	case codecIDFLAC:
		return "flac", nil
	case codecIDRaw:
		return "raw", nil
	default:
		return "", fmt.Errorf("scrcpy: unknown audio codec id %#08x", id)
	}
}

// readAudioPacket reads one audio packet with the same 12-byte frame header
// the video stream uses. Session headers belong to video and are skipped.
func readAudioPacket(r io.Reader) (AudioPacket, error) {
	for {
		hdr, err := readPacketHeader(r)
		if err != nil {
			return AudioPacket{}, err
		}
		if isSessionHeader(hdr) {
			continue
		}
		ptsFlags := binary.BigEndian.Uint64(hdr[0:8])
		pktLen := binary.BigEndian.Uint32(hdr[8:12])
		if pktLen == 0 || pktLen > maxAudioPacketSize {
			return AudioPacket{}, fmt.Errorf("scrcpy: invalid audio packet size %d", pktLen)
		}
		data := make([]byte, pktLen)
		if _, err := io.ReadFull(r, data); err != nil {
			if errors.Is(err, io.EOF) {
				return AudioPacket{}, io.ErrUnexpectedEOF
			}
			return AudioPacket{}, err
		}
		return AudioPacket{
			Data:   data,
			PTS:    ptsFlags & packetPTSMask,
			Config: (ptsFlags & packetFlagConfig) != 0,
		}, nil
	}
}

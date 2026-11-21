package mirror

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"math"
	"testing"
)

func TestDeriveStreamKey(t *testing.T) {
	audio := []byte("0123456789abcdef")
	key, iv := DeriveStreamKey(123456789, audio)

	if len(key) != 16 || len(iv) != 16 {
		t.Fatalf("key/iv lengths = %d/%d, want 16/16", len(key), len(iv))
	}
	if bytes.Equal(key[:], iv[:]) {
		t.Fatal("key and iv must differ")
	}
	want := sha512.Sum512(append([]byte("AirPlayStreamKey123456789"), audio...))
	if !bytes.Equal(key[:], want[:16]) {
		t.Fatal("key does not match the documented derivation")
	}

	key2, _ := DeriveStreamKey(123456789, audio)
	if key != key2 {
		t.Fatal("derivation is not deterministic")
	}
	if key3, _ := DeriveStreamKey(123456790, audio); key3 == key {
		t.Fatal("key must depend on the stream connection id")
	}
}

func TestToAnnexB(t *testing.T) {
	nal1 := []byte{0x65, 0xaa, 0xbb}
	nal2 := []byte{0x41, 0xcc}
	payload := make([]byte, 0, 4+len(nal1)+4+len(nal2))
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(nal1)))
	payload = append(payload, nal1...)
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(nal2)))
	payload = append(payload, nal2...)

	st := &streamState{}
	if !st.toAnnexB(payload) {
		t.Fatal("well-formed payload rejected")
	}

	want := append([]byte{}, AnnexBStartCode...)
	want = append(want, nal1...)
	want = append(want, AnnexBStartCode...)
	want = append(want, nal2...)
	if !bytes.Equal(payload, want) {
		t.Fatalf("got % x\nwant % x", payload, want)
	}
}

func TestToAnnexBRejectsBadLength(t *testing.T) {
	// Claims 16 bytes, only 3 follow.
	payload := binary.BigEndian.AppendUint32(nil, 16)
	payload = append(payload, 0x65, 0x01, 0x02)
	if (&streamState{}).toAnnexB(payload) {
		t.Fatal("truncated payload accepted")
	}

	// Forbidden bit set on the NAL header.
	payload = binary.BigEndian.AppendUint32(nil, 1)
	payload = append(payload, 0x80)
	if (&streamState{}).toAnnexB(payload) {
		t.Fatal("payload with forbidden bit set accepted")
	}

	// Zero-length NAL would loop forever if unguarded.
	payload = binary.BigEndian.AppendUint32(nil, 0)
	if (&streamState{}).toAnnexB(payload) {
		t.Fatal("zero-length NAL accepted")
	}
}

func TestSplitAVC(t *testing.T) {
	sps := []byte{0x67, 0x42, 0x00, 0x1f}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}

	// Layout from UxPlay: SPS at 8, one filler byte, PPS length, PPS.
	payload := make([]byte, 11+len(sps)+len(pps))
	binary.BigEndian.PutUint16(payload[6:8], uint16(len(sps)))
	copy(payload[8:], sps)
	binary.BigEndian.PutUint16(payload[9+len(sps):11+len(sps)], uint16(len(pps)))
	copy(payload[11+len(sps):], pps)

	gotSPS, gotPPS, ok := splitAVC(payload)
	if !ok {
		t.Fatal("splitAVC rejected a well-formed payload")
	}
	if !bytes.Equal(gotSPS, sps) || !bytes.Equal(gotPPS, pps) {
		t.Fatalf("got sps % x pps % x", gotSPS, gotPPS)
	}

	packed := packParameterSets(sps, pps)
	if !bytes.HasPrefix(packed, AnnexBStartCode) {
		t.Fatal("packed parameter sets lack a start code")
	}
	if len(packed) != 8+len(sps)+len(pps) {
		t.Fatalf("packed length = %d", len(packed))
	}
}

func TestSplitHEVC(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0c}
	sps := []byte{0x42, 0x01, 0x01}
	pps := []byte{0x44, 0x01, 0xc1}

	payload := make([]byte, 0x75+3*(5+4))
	copy(payload[4:8], "hvc1")
	off := 0x75
	for i, unit := range [][]byte{vps, sps, pps} {
		tag := []byte{0xa0 + byte(i), 0x00, 0x01, 0x00}
		copy(payload[off:], tag)
		binary.BigEndian.PutUint16(payload[off+3:off+5], uint16(len(unit)))
		copy(payload[off+5:], unit)
		off += 5 + len(unit)
	}

	gotVPS, gotSPS, gotPPS, ok := splitHEVC(payload)
	if !ok {
		t.Fatal("splitHEVC rejected a well-formed payload")
	}
	if !bytes.Equal(gotVPS, vps) || !bytes.Equal(gotSPS, sps) || !bytes.Equal(gotPPS, pps) {
		t.Fatalf("got % x / % x / % x", gotVPS, gotSPS, gotPPS)
	}
}

func TestGeometryParsing(t *testing.T) {
	header := make([]byte, HeaderLen)
	putF32 := func(off int, v float32) {
		binary.LittleEndian.PutUint32(header[off:off+4], mathFloat32bits(v))
	}
	putF32(16, 1920)
	putF32(20, 1080)
	putF32(48, 0)
	putF32(52, 0)
	putF32(56, 1920)
	putF32(60, 1080)

	var got Size
	st := &streamState{srv: &Server{handlers: Handlers{OnGeometry: func(s Size) { got = s }}}}
	st.handleParams(header, nil, 0)
	_ = got

	if int(geometryFloat(header[56:60])) != 1920 || int(geometryFloat(header[60:64])) != 1080 {
		t.Fatalf("geometry parse: %+v", got)
	}
}

func mathFloat32bits(f float32) uint32 {
	return math.Float32bits(f)
}

func TestHandleParamsReportsGeometry(t *testing.T) {
	sps := []byte{0x67, 0x42, 0x00, 0x1f}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	payload := make([]byte, 11+len(sps)+len(pps))
	binary.BigEndian.PutUint16(payload[6:8], uint16(len(sps)))
	copy(payload[8:], sps)
	binary.BigEndian.PutUint16(payload[9+len(sps):11+len(sps)], uint16(len(pps)))
	copy(payload[11+len(sps):], pps)

	header := make([]byte, HeaderLen)
	binary.LittleEndian.PutUint32(header[56:60], mathFloat32bits(1920))
	binary.LittleEndian.PutUint32(header[60:64], mathFloat32bits(1080))

	var got Size
	srv := &Server{handlers: Handlers{OnGeometry: func(s Size) { got = s }}}
	st := &streamState{srv: srv}
	st.handleParams(header, payload, 42)

	if got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("geometry = %+v", got)
	}
	if st.codec != CodecH264 {
		t.Fatalf("codec = %v, want h264", st.codec)
	}
	if len(st.spsPPS) != 8+len(sps)+len(pps) {
		t.Fatalf("stored parameter sets = %d bytes", len(st.spsPPS))
	}
	if st.spsNTP != 42 {
		t.Fatalf("sps timestamp = %d", st.spsNTP)
	}
}

func TestVideoPrependsParameterSets(t *testing.T) {
	nal := []byte{0x65, 1, 2, 3, 4}
	payload := binary.BigEndian.AppendUint32(nil, uint32(len(nal)))
	payload = append(payload, nal...)

	var frame *Frame
	srv := &Server{handlers: Handlers{OnFrame: func(f *Frame) { frame = f }}}
	block, err := aes.NewCipher(make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	st := &streamState{
		srv:     srv,
		cands:   []cipher.Stream{cipher.NewCTR(block, make([]byte, 16))},
		names:   []string{"test"},
		active:  0,
		spsPPS:  packParameterSets([]byte{0x67, 0x00}, []byte{0x68, 0x00}),
		haveSPS: true,
		spsNTP:  7,
	}
	// The sender encrypts with its own keystream; the receiver holds a separate
	// stream starting at the same offset.
	encrypted := make([]byte, len(payload))
	sender := cipher.NewCTR(block, make([]byte, 16))
	sender.XORKeyStream(encrypted, payload)
	st.handleVideo(encrypted, 7, 0)

	if frame == nil {
		t.Fatal("no frame reported")
	}
	if !bytes.HasPrefix(frame.Data, AnnexBStartCode) {
		t.Fatal("frame does not start with a start code")
	}
	wantLen := (4 + 2) + (4 + 2) + len(payload)
	if len(frame.Data) != wantLen {
		t.Fatalf("frame length = %d, want %d", len(frame.Data), wantLen)
	}
	if !frame.Valid {
		t.Fatal("frame marked invalid")
	}
	if st.haveSPS {
		t.Fatal("parameter sets were not consumed")
	}
}

func TestVideoDropsStaleParameterSets(t *testing.T) {
	nal := []byte{0x65, 9}
	payload := binary.BigEndian.AppendUint32(nil, uint32(len(nal)))
	payload = append(payload, nal...)

	var frame *Frame
	srv := &Server{handlers: Handlers{OnFrame: func(f *Frame) { frame = f }}}
	st := &streamState{srv: srv, spsPPS: []byte{9, 9}, haveSPS: true, spsNTP: 7}
	st.handleVideo(payload, 8, 0)

	if frame == nil || len(frame.Data) != len(payload) {
		t.Fatalf("stale parameter sets were not dropped: %d bytes", len(frame.Data))
	}
}

func TestVideoWithoutKeyIsInvalid(t *testing.T) {
	nal := []byte{0x65, 1}
	payload := binary.BigEndian.AppendUint32(nil, uint32(len(nal)))
	payload = append(payload, nal...)

	var frame *Frame
	srv := &Server{handlers: Handlers{OnFrame: func(f *Frame) { frame = f }}}
	st := &streamState{srv: srv} // no cipher
	st.handleVideo(payload, 1, 0)

	if frame.Valid {
		t.Fatal("frame without a key must be marked invalid")
	}
}

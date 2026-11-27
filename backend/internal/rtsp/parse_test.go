package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func parse(t *testing.T, raw string) (*Request, error) {
	t.Helper()
	return readRequest(bufio.NewReader(strings.NewReader(raw)))
}

func TestReadRequestParsesAirPlayControlMessage(t *testing.T) {
	body := []byte{0x62, 0x70, 0x6c, 0x69, 0x73, 0x74, 0x00, 0xff}
	raw := "SETUP rtsp://192.168.1.20/1?session=abc&x=1 RTSP/1.0\r\n" +
		"CSeq: 7\r\n" +
		"Content-Type: application/x-apple-binary-plist\r\n" +
		"content-length: 8\r\n" +
		"Connection: close\r\n\r\n" + string(body)

	req, err := parse(t, raw)
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if req.Method != "SETUP" || req.Target != "rtsp://192.168.1.20/1?session=abc&x=1" || req.Protocol != "RTSP/1.0" {
		t.Fatalf("request = %+v", req)
	}
	if got := req.Header("cseq"); got != "7" {
		t.Fatalf("case-insensitive header = %q", got)
	}
	if got := req.Header("missing"); got != "" {
		t.Fatalf("missing header = %q", got)
	}
	if req.Path() != "rtsp://192.168.1.20/1" || req.Query() != "session=abc&x=1" {
		t.Fatalf("path/query = %q, %q", req.Path(), req.Query())
	}
	if !req.Close {
		t.Fatal("Connection: close was not recorded")
	}
	if !bytes.Equal(req.Body, body) {
		t.Fatalf("body = %x, want %x", req.Body, body)
	}
}

func TestReadRequestAcceptsBareLF(t *testing.T) {
	req, err := parse(t, "OPTIONS /info RTSP/1.0\nCSeq: 1\n\n")
	if err != nil {
		t.Fatalf("bare LF request: %v", err)
	}
	if req.Method != "OPTIONS" || req.Header("CSeq") != "1" {
		t.Fatalf("request = %+v", req)
	}
}

func TestReadRequestRejectsMalformedMessages(t *testing.T) {
	tooLong := strings.Repeat("A", maxHeaderBytes+1) + "\r\n"
	cases := []struct {
		name string
		raw  string
	}{
		{"blank first line", "\r\n\r\n"},
		{"request line with two fields", "OPTIONS /info\r\n\r\n"},
		{"header without a colon", "OPTIONS /info RTSP/1.0\r\nNoColonHere\r\n\r\n"},
		{"negative length", "POST /x RTSP/1.0\r\nContent-Length: -1\r\n\r\n"},
		{"non-numeric length", "POST /x RTSP/1.0\r\nContent-Length: many\r\n\r\n"},
		{"length over the body cap", "POST /x RTSP/1.0\r\nContent-Length: 4194305\r\n\r\n"},
		{"header line over the cap", "OPTIONS /info RTSP/1.0\r\n" + tooLong + "\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parse(t, tc.raw); !errors.Is(err, errBadRequest) {
				t.Fatalf("error = %v, want errBadRequest", err)
			}
		})
	}
}

func TestReadRequestPropagatesShortBody(t *testing.T) {
	_, err := parse(t, "POST /x RTSP/1.0\r\nContent-Length: 10\r\n\r\nabc")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadRequestEmptyInputIsEOF(t *testing.T) {
	if _, err := parse(t, ""); !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want io.EOF", err)
	}
}

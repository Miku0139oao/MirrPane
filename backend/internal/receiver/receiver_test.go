package receiver

import (
	"sync"
	"testing"

	"howett.net/plist"

	"github.com/Miku0139oao/MirrPane/backend/internal/audio"
)

func TestTeardownRetainsOnlyAudioSessions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		streams []any
		keep    bool
	}{
		{name: "audio only", streams: []any{map[string]any{"type": uint64(96)}}, keep: true},
		{name: "video only", streams: []any{map[string]any{"type": uint64(110)}}},
		{name: "audio and video", streams: []any{map[string]any{"type": uint64(96)}, map[string]any{"type": uint64(110)}}},
		{name: "whole session"},
		{name: "empty streams", streams: []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.streams != nil {
				var err error
				body, err = plist.Marshal(map[string]any{"streams": tc.streams}, plist.BinaryFormat)
				if err != nil {
					t.Fatal(err)
				}
			}
			const remote = "192.168.1.20"
			cs := &clientSession{aes: []byte("stream key"), ecdh: []byte("pairing secret"), haveEKey: true}
			other := &clientSession{}
			r := &Receiver{
				// A zero-value audio receiver resets its tracking without sockets
				// or a decoder, keeping the test independent of FFmpeg and the LAN.
				audioRx:  &audio.Receiver{},
				sessions: map[string]*clientSession{remote: cs, "2001:db8::20": other},
			}
			states := 0
			r.events.OnState = func(status Status, reason string) {
				states++
				if status != StatusIdle {
					t.Errorf("full teardown state = %v, want idle", status)
				}
				// The callback can re-enter the receiver after session cleanup.
				r.mu.Lock()
				defer r.mu.Unlock()
				if _, ok := r.sessions[remote]; ok {
					t.Error("idle callback ran before the session was dropped")
				}
			}
			for range 2 {
				r.Teardown(remote, body)
				r.mu.Lock()
				got, otherGot := r.sessions[remote], r.sessions["2001:db8::20"]
				r.mu.Unlock()
				if tc.keep && got != cs {
					t.Fatal("audio-only TEARDOWN dropped pairing and key state")
				}
				if !tc.keep && got != nil {
					t.Fatal("full TEARDOWN retained the sender session")
				}
				if otherGot != other {
					t.Fatal("TEARDOWN dropped another sender's session")
				}
			}
			if tc.keep && (states != 0 || r.cleanStop.Load()) {
				t.Fatalf("audio-only TEARDOWN ended mirroring: %d state changes, clean stop %v", states, r.cleanStop.Load())
			}
			if !tc.keep && (states != 2 || !r.cleanStop.Load()) {
				t.Fatalf("full TEARDOWN did not end mirroring: %d state changes, clean stop %v", states, r.cleanStop.Load())
			}
			// Connection close after either kind of TEARDOWN safely forgets it.
			r.DropSession(remote)
			r.DropSession(remote)
		})
	}
}

func TestDropAllSessions(t *testing.T) {
	r := &Receiver{sessions: map[string]*clientSession{
		"192.168.1.20": {},
		"2001:db8::20": {},
	}}
	r.DropAllSessions()
	r.DropAllSessions()
	r.DropSession("192.168.1.20")
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) != 0 {
		t.Fatalf("drop all left %d sessions", len(r.sessions))
	}
}

func TestConcurrentSessionCleanup(t *testing.T) {
	r := &Receiver{
		audioRx: &audio.Receiver{},
		sessions: map[string]*clientSession{
			"192.168.1.20": {},
			"2001:db8::20": {},
		},
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r.Teardown("192.168.1.20", nil)
			r.DropSession("192.168.1.20")
			r.DropAllSessions()
		})
	}
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) != 0 {
		t.Fatalf("concurrent cleanup left %d sessions", len(r.sessions))
	}
}

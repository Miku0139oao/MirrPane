package audio

import "time"

const (
	// Mirrored AAC-ELD RTP timestamps use the sender's 44.1 kHz clock, not
	// the decoder's resampled 48 kHz PCM output clock.
	rtpClockRate = 44100
	// Tolerate ordinary receipt-time jitter while rejecting old catch-up
	// bursts before they consume decoder time or fill the PCM ring.
	latePacketMargin      = 40 * time.Millisecond
	latePacketMarginTicks = int32(latePacketMargin * rtpClockRate / time.Second)
	maxSyncAge            = 5 * time.Second
	lateDropLogInterval   = 5 * time.Second
)

// rtpSync is protected by Receiver.mu. NTP is retained as the sender's raw
// mapping value; extrapolation uses local monotonic elapsed time so host and
// sender wall clocks need not agree, and no NTP epoch conversion is needed.
type rtpSync struct {
	rtp       uint32
	ntp       uint64
	localTime time.Time
	valid     bool
}

func (s rtpSync) expected(now time.Time) (uint32, bool) {
	if !s.valid {
		return 0, false
	}
	age := now.Sub(s.localTime)
	if age < 0 || age > maxSyncAge {
		return 0, false
	}
	return s.rtp + uint32(age*rtpClockRate/time.Second), true
}

func (r *Receiver) clockNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

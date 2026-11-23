# AirPlay audio quality investigation — 2026-11-23

## Finding

The reproducible bottleneck is the Windows waveOut device queue. Three 10 ms
headers do not cover the device's header-return and pump-scheduling delay on
this machine. The player cannot reuse a header until WHDR_DONE is set, even
when decoded PCM is waiting in the ring. This throttles playout, grows the
ring and eventually forces audible catch-up splices. Increasing ring capacity
alone postpones those splices; it does not fix the device's throughput.

Two related jitter policies also needed correction: starting a whole arriving
burst immediately leaves no time cushion before the next burst; treating every
burst peak as clock drift unnecessarily speeds up a correctly paced source.

The running receiver was not restarted, stopped or overwritten. Read-only
inspection used its existing log and localhost state API. Separate tests opened
the shared default output device and submitted only zero samples; no volume or
system audio settings were changed. iPhone/Spotify listening with a rebuilt
receiver is still needed.

## Evidence

The existing log advertises `spf=480`; the AAC-ELD AudioSpecificConfig specifies
44.1 kHz. Resampling 480 source frames to 48 kHz correctly produces alternating
522/523-frame output blocks. For example, access units #1400–#1600 took
2.176951 s, corresponding to approximately 47,998 output frames/s. That rate
does not explain a large sustained overflow.

The first callback log lines also show burst delivery: units #1/#2 share the
same timestamp, as do #3/#4 roughly 20.7 ms later. Those sampled log entries
support clumped delivery but do not establish the maximum burst size. Audio
was paused during read-only API inspection, so no fresh maximum-burst claim is
made from that inactive interval.

The new FFmpeg regression processes exactly 32 s of 480-frame/44.1 kHz blocks
through one persistent resampler: 1,535,983 output frames versus 1,536,000
expected. The 17-frame difference is the retained filter delay, or 47,999.4688
frames/s without draining the filter. It fails on per-block rounding/reset or
an incorrect source frequency. A separate RTP regression sends three copies
of every packet plus late previous-packet copies across sequence-number wrap:
1,000 unique packets cause exactly 1,000 decodes and 522,000 output frames.

Silent native-device measurements, using the same 48 kHz stereo format and
`waveOutGetPosition(TIME_SAMPLES)`:

| Device queue / test | Input | Device progress | Ring observation |
| --- | ---: | ---: | --- |
| Legacy low: 3 x 10 ms, already enlarged 200 ms ring; manual 3 ms pump for 3.002 s | 144,000 frames | 107,031 frames | 188.375 ms queued, 198.5 ms peak |
| Intermediate: 4 x 10 ms, production pump, 5.001 s | 240,000 frames | 231,141 frames | 135.167 ms queued, 147.708 ms peak |
| Intermediate balanced: 4 x 10 ms, production pump, 5.002 s | 240,000 frames | 230,375 frames | 149.021 ms queued, 177.208 ms peak |
| Final low: 5 x 10 ms, production pump, 5.000 s | 240,000 frames | 237,737 frames | 0 ms queued, 30 ms peak |
| Final balanced: 5 x 10 ms, production pump, 5.003 s | 240,000 frames | 237,935 frames | 0 ms queued, 40 ms peak |
| Final stable: 6 x 10 ms, production pump, 5.002 s | 240,000 frames | 238,085 frames | 0 ms queued, 30 ms peak |

All three final runs had zero overflow splices, underruns and drift episodes.
The final progress deficit is bounded startup/device/reporting delay rather
than a growing ring. The opt-in hardware regression also checks the playback
rate after the first second, allowing a 1% clock/reporting tolerance. Measured
steady rates were 48,000.57 Hz (low), 48,001.04 Hz (balanced), and 47,999.23 Hz
(stable).

The old `TrimCount` combined actual overflow splices with gentle drift episodes.
Thus the original `audioTrims` increase cannot be multiplied by a guessed drop
size to estimate missing audio. It also means zero software underruns alone
does not prove the device stayed fed: native header-return delays can leave
software buffers marked in flight after their samples have played. The new
deterministic device model measures actual gaps as well as software counters.

## Final policy

| Profile | Device queue | Ring capacity | Catch-up target | High-water |
| --- | ---: | ---: | ---: | ---: |
| low | 5 x 10 ms | 200 ms | 20 ms | 60 ms |
| balanced | 5 x 10 ms | 250 ms | 30 ms | 70 ms |
| stable | 6 x 10 ms | 350 ms | 40 ms | 80 ms |

- Wait for both 20 ms of decoded data and a 20 ms time cushion on startup or
  resumption after an empty queue. This preserves jitter headroom even when
  several packets arrive together.
- Start gentle catch-up only when post-take occupancy remains above high-water
  for 100 ms. A transient burst peak does not qualify. Catch-up consumes up to
  0.5% extra source frames, with continuous fractional interpolation and shared
  stereo positions. Preserve the fraction when returning to normal speed;
  copy straight when the interpolation neighbour is unavailable.
- On a genuine overflow, crossfade the retained queue down to high-water and
  append the incoming packet. A write larger than the entire capacity retains
  only its newest tail. Both cases count as one overflow splice.
- `Stats.TrimCount`, and therefore the existing `audioTrims` API field, now
  count only actual overflow splices. `Stats.DriftCount` tracks gentle catch-up
  episodes separately. The API wiring added by the coordinator is retained.

No RTP/NTP timestamp-driven playout was introduced. The evidence identifies a
local device-queue bottleneck and inadequate jitter headroom, which these
bounded changes address. Timestamp scheduling would require a sender clock
mapping, pacing, discontinuity/loss handling and A/V alignment policy; that
complexity is not needed for this reproduction.

## Regression coverage and verification

- Crossfade overflow tests derive input sizes from each profile's capacity.
- Periodic six-packet bursts run for 20 simulated seconds with no overflow,
  correction, underrun or device gap: 480-frame packets every 60 ms and
  522-frame packets every 65.25 ms. Six 522-frame packets every 60 ms would
  over-supply audio by 8.75% and is not a valid equal-rate jitter test.
- A delayed-header model reproduces starvation/overflow with three and four
  buffers despite the enlarged ring; five buffers preserve device coverage.
- Sustained sub-capacity backlog returns to the target without a hard splice.
- Wrapped-ring tests verify sample ordering, counts and fractional phase across
  unequal buffer lengths, drift exit, direct copying and missing lookahead.
- Native-device testing is opt-in: set `MIRRPANE_AUDIO_DEVICE_TEST=1` and run
  `go test -run '^TestSilentDeviceMaintainsSampleRate$' -count=1 -v ./internal/audioout`.
  Normal tests skip hardware access.

Verified with Go 1.26.11 on Windows/amd64 and the pinned runtime at
`D:\new\airplay-go\third_party\ffmpeg\bin`:

- `CGO_ENABLED=0`: `go build ./...`, `go vet ./...`, and
  `go test -count=1 ./...` all passed.
- `CGO_ENABLED=1`, `CC=D:\DevTools\mingw64\bin\gcc.exe`:
  `go test -race -count=1 ./internal/audioout ./internal/audio ./cmd/airplayd`
  passed.
- The opt-in native-device test passed all three profiles, with the measured
  rates above. Normal full/race test runs skipped hardware access.
- `git diff --check` passed. No commit or push was made. No receiver, trust,
  security, config or frontend file was edited. The coordinator's existing
  two-line `state.go` change exposing `audioTrims` was preserved.

Implementation/test files: `backend/internal/audioout/player_windows.go`,
`player_test.go`, `backlog_test.go`, `burst_test.go`, `device_windows_test.go`,
`backend/internal/audio/sequence_test.go`, and
`backend/internal/decode/audio_rate_test.go`.

## Listening follow-up and trade-offs

Rebuild/restart is owned by the coordinator. Play Spotify over iPhone screen
mirroring for several minutes, including pause/resume and ordinary Wi-Fi load.
Watch deltas in `audioTrims` and `audioUnderruns`, and the trend in
`audioOccupancyMs`. After rebuild, trims should stay zero during normal
continuous playback, underruns should not rise during playback, and occupancy
should fluctuate with delivery rather than climb monotonically to capacity.
Intentional pauses can count one underrun. `audioHighWaterMs` is a lifetime
peak; a single burst can leave it elevated after the queue drains.

The low profile gains 20 ms of maximum device queue versus its former three
buffers, balanced gains 10 ms, and startup/resumption gains a 20 ms time
cushion. Ring capacity is an upper bound on additional burst backlog, not a
fixed delay; occupancy below high-water may remain above the nominal target
until sustained backlog warrants correction. Large bursts, long OS scheduling
stalls or a sustained source/output clock mismatch exceeding 0.5% can still
force an audible bounded splice. These tests do not establish end-to-end A/V
sync or rule out device-specific stalls on other Windows drivers.

Microsoft documents that a submitted header becomes reusable when WHDR_DONE
is set: [waveOutWrite](https://learn.microsoft.com/en-us/windows/win32/api/mmeapi/nf-mmeapi-waveoutwrite).
Playback-position verification uses the native
[waveOutGetPosition](https://learn.microsoft.com/en-us/windows/win32/multimedia/retrieving-the-current-playback-position)
API, independently of ring statistics.

## Late-packet drop at the RTP deadline (2026-11-24)

After the waveOut throughput fix the ring stayed balanced (underruns near zero)
but `audioTrims` still grew about 1.45/s on a live 1440p session, with
occupancy oscillating between 100 and 200 ms. The sender stalls briefly and
then delivers a catch-up burst of already-stale audio in one UDP read; those
packets were queued even though their playout time had passed, filling the ring
until a hard trim spliced the stream.

`backend/internal/audio/jitter.go` now keeps the newest RTP/NTP sync as a local
anchor (`rtp`, `ntp`, receipt time) and extrapolates the sender's playout
timestamp as `syncRTP + elapsed × 44,100/s` using the monotonic receipt clock.
The NTP value is retained but never needed: extrapolation uses local elapsed
time, so host and sender wall clocks need not agree and no NTP epoch conversion
is performed. Data packets older than that estimate by more than a 40 ms margin
are counted and dropped before decryption and decoding, which also keeps them
out of the decoder's timing state. The comparison uses `int32(rtp-expected)`,
so it wraps with the 32-bit RTP clock. The check fails open: without a sync at
most 5 s old (such a sync is re-anchored every ~1 s by the sender) everything
is accepted exactly as before. Sequence dedupe still runs first, and the
sampled log line reports the count, how far behind the packet was, and the age
of the sync anchor. `audioLateDropped` in `/api/state` exposes the cumulative
count; `audioTrims` should stop increasing, occupancy should settle near the
target, and underruns should not rise because only audio that would otherwise
be trimmed is discarded.

Residual risk: the 40 ms margin assumes the sender's nominal RTP clock matches
its arrival timeline within that window. A sender whose sync mapping is
deliberately offset ahead of its packets (large internal buffering) would have
its early packets treated as late; the 5 s freshness window and fail-open
behavior bound the damage, but a device that emits sync at a fixed offset from
playout could see unnecessary drops. This is not verified against a real
Apple sender's sync schedule beyond the measured session.

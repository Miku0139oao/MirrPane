# Audio backend evaluation: waveOut now, event-driven WASAPI next

Status: evaluation, no code change. The shipping backend stays waveOut.

## What we run today

`internal/audioout` plays 48 kHz stereo s16 PCM through winmm `waveOut`:

- one 10 ms device buffer per header, up to eight headers (80 ms) queued;
- a ring buffer with gentle drift correction, crossfade trims and a low/balanced/stable profile;
- a pump thread promoted to MMCSS "Pro Audio" (or `THREAD_PRIORITY_TIME_CRITICAL` when AvSetMmThreadCharacteristics is unavailable);
- endpoint supervision by probing the wave mapper every two seconds, plus rejected writes and stalled headers; the device is reopened with backoff.

It is reliable, needs no DLL beyond system winmm/avrt, and has survived real 2K/60 mirroring load. Its limits are structural: polling for endpoint changes (up to 2 s), no per-device event stream, and no way to ask for a shorter engine period than the shared-mode 10 ms.

## Why consider an event-driven WASAPI backend

1. **Latency headroom.** WASAPI shared mode still runs at the mix engine period (10 ms on most systems), but `IAudioClient3` (Windows 10 1703+) exposes the engine's minimum period, often 2.67–10 ms. Event-driven rendering wakes on an audio event instead of a 3 ms polling tick, so a smaller queue can stay fed under the same decode load.
2. **Hot-plug fidelity.** `IMMNotificationClient` reports default-device changes, device removal and format changes as callbacks, replacing the 2 s wave-mapper probe and making recovery feel instant.
3. **Telemetry.** `IAudioClock`/`GetPosition` give precise play cursors, which lets the HUD report true output latency rather than queue occupancy.

## Proposed shape

```
internal/audioout
├── player.go            ring, profiles, drift/trim, telemetry (backend-agnostic)
├── backend.go           interface: Open, Write, Reset, Close, Signature, Events
├── backend_waveout.go   current winmm implementation (Win 7+ fallback)
└── backend_wasapi.go    event-driven WASAPI (Win 10 1703+)
```

`Player` keeps the jitter logic it has today; only the device layer moves behind the interface. The preference `audioBackend: auto | waveout | wasapi` (default `auto`) selects WASAPI when initialization succeeds and falls back to waveOut on any error, so a driver quirk can never make the receiver silent.

### WASAPI details

- **COM without cgo.** Interfaces are vtable calls through `syscall.SyscallN`, same style as the existing D3D11 and winmm bindings. `IMMNotificationClient` needs a Go-implemented COM object: a struct whose first field is a vtable of function pointers from `syscall.NewCallback`; the callbacks must not allocate unboundedly or block, so they only post to a channel.
- **Format.** Request the mix format with `AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY` and offer 48 kHz stereo s16; Windows converts to the endpoint's float mix format. No resampler of our own.
- **Event-driven loop.** `SetEventHandle` + `WaitForSingleObject`; on each wake, fill the render client's available frames from the ring in one `GetBuffer/ReleaseBuffer`. No polling tick. Keep the MMCSS promotion.
- **Low-latency mode.** Try `IAudioClient3::InitializeSharedAudioStream` with the minimum period; if it fails, fall back to `IAudioClient::Initialize` at the default period. Never exclusive mode by default: it locks the device and breaks other apps' audio.
- **Hot-plug.** Register `IMMNotificationClient`; on default-device or removal events, stop and reopen. Keep the current 2 s probe as a safety net for drivers that do not notify.

## Risks and mitigations

| Risk | Mitigation |
| :--- | :--- |
| COM lifetime/refcount bugs crash the process | One owner, explicit `Release` on close; race-detector runs; fuzz the wrapper with a fake vtable |
| Callback reentrancy or GC of Go callbacks | Pinned callbacks created once at init; callbacks only enqueue |
| Driver-specific period failures | Try minimum period, then default, then waveOut |
| Device removal mid-write | Same stall/backoff policy as today; reopen on notification or timeout |
| Regression in the shipping path | `auto` keeps waveOut unless WASAPI passes a soak; opt-in `wasapi` first |

## Milestones

1. **M1** — extract the backend interface; waveOut becomes one implementation. No behaviour change. Unit tests stay on the ring and drift logic; the backend gets a fake for pump tests.
2. **M2** — event-driven WASAPI render loop with `auto` fallback and the existing profiles. Compare underruns/trims against waveOut over a 2-hour mirroring soak.
3. **M3** — `IMMNotificationClient` hot-plug and true play-cursor telemetry in the HUD.
4. **M4** — `IAudioClient3` minimum-period mode behind the `wasapi` option; measure end-to-end latency with a loopback capture and the existing 1 kHz tone harness.
5. **M5** — decide the default. Flip `auto` to WASAPI only if it matches waveOut's underrun rate and latency is measurably lower across at least three endpoints (USB DAC, Realtek onboard, Bluetooth).

Until M5, waveOut remains the compatibility path and the default. The evaluation is deliberately staged so every step ships or reverts independently.

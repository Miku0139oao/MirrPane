# MirrPane (鏡屏)

[繁體中文](README.md) | **English**

**An AirPlay screen mirroring receiver for Windows**, built with **Go + FFmpeg D3D11VA + Flutter**. Turn your Windows PC into a wireless display for an iPhone or iPad. Extract the release archive and run the app, with no installer, registry writes, or additional runtime installation required.

- **License**: [GPL-3.0 License](https://github.com/Miku0139oao/MirrPane/blob/main/LICENSE)
- **Repository**: [Miku0139oao/MirrPane](https://github.com/Miku0139oao/MirrPane)
- **Latest release**: `v0.3.4`

---

## Features

- **Low-latency hardware decoding**: FFmpeg **D3D11VA** provides H.264 hardware acceleration. Complete Annex-B access units go directly to the decoder. Software decoding is available when hardware acceleration is unavailable and can also be selected in settings.
- **Go backend without CGO**: Built with `CGO_ENABLED=0`. The backend binds FFmpeg at runtime through `purego` and calls Windows system APIs from Go. No MinGW compiler or FFmpeg development headers are needed for a normal backend build.
- **Shared-memory frame rendering**: The backend publishes RGBA frames to Windows shared memory (`Local\YingpingFrames`). A three-slot mailbox and Windows event notifications deliver frames to the native C++ texture bridge (`yingping_frames`) for Flutter, avoiding image serialization between processes.
- **Direct-to-slot conversion with idle gating**: `sws_scale` writes straight into the claimed shared-memory slot (no full-frame copy per picture). When the UI is minimised or stops adopting frames for a second, the download, conversion and publish are skipped while decoding continues (reference frames), with a 1 Hz probe so the UI can re-adopt instantly on restore.
- **Low-latency audio playback**: AAC-ELD audio is decoded and played through Windows `winmm waveOut`. Supports live volume control, mute, and three jitter-buffer profiles: ultra-low latency (`30 ms`), balanced (`40 ms`), and stable (`60 ms`). The ring gently catches up when a burst raises occupancy, so latency does not stay elevated for the rest of the session.
- **Live pipeline telemetry**: `/api/state` reports fps, mean/p95 for decode, GPU download, colour conversion and publish, consumer adoption progress, and audio buffer metrics.
- **Automatic FFmpeg management**: Pins a dated upstream build (SHA256 and ABI checked), detects existing runtimes, and supports resumable first-run downloads. DLLs are resolved only from explicit trusted locations — never `PATH` or the working directory.
- **Security hardening**: stream setup requires a completed pair-verify, mirror connections are bound to the sender IP and stream ID, AES keys are no longer logged, HEVC parameter parsing is bounds-checked, Go 1.26.11 is required, the local API validates Host/Origin, and `/stop` is loopback-only. Details and residual risk: [`analysis/MirrPane/SECURITY_FINDINGS.md`](analysis/MirrPane/SECURITY_FINDINGS.md).
- **Portable distribution**: Settings (`config.json`) and pairing keys stay in the application directory. The app does not write to the Windows registry.

---

## Architecture

```text
iPhone / iPad (AirPlay / mDNS)
        │
        ▼
airplayd (Go sidecar, CGO_ENABLED=0)
  ├── mDNS / Bonjour advertisements (zeroconf)
  ├── FairPlay key exchange and AirPlay pairing (pair-setup / pair-verify)
  ├── Video: purego bindings to FFmpeg D3D11VA (H.264 Annex-B)
  └── Audio: AAC-ELD → Windows winmm waveOut (30 / 40 / 60 ms buffers)
        │
        ▼  Video: three-slot shared-memory mailbox (Local\YingpingFrames)
        │         + Windows event notifications
        │  State and controls: local HTTP API (IPC)
        ▼
Flutter Desktop UI (yingping)
  └── yingping_frames (C++ Windows texture bridge)
```

---

## Project Layout

```text
MirrPane/
├── .github/workflows/          # CI and release workflows
├── backend/                    # Go module: github.com/miku0139/airplay-go/backend
│   ├── cmd/
│   │   ├── airplayd/           # Main sidecar and local HTTP API
│   │   └── ffmpeg-setup/       # Shared FFmpeg DLL download, verification, and installation
│   ├── internal/
│   │   ├── audio/              # Audio stream reception, decryption, and AAC-ELD decoding flow
│   │   ├── audioout/           # Windows waveOut playback and jitter buffering
│   │   ├── config/             # config.json persistence
│   │   ├── decode/             # purego FFmpeg bindings and video/audio decoding
│   │   ├── fairplay/           # FairPlay key exchange and decryption
│   │   ├── ffmpeg/             # Runtime discovery, download, verification, and caching
│   │   ├── frames/             # Frame publication and shared-memory layout
│   │   ├── ipc/                # AirPlay RTSP control endpoints and packet parsing
│   │   ├── mdns/               # AirPlay mDNS / Bonjour advertisements and discovery
│   │   ├── mirror/             # Screen mirroring stream reception and Annex-B processing
│   │   ├── pairing/            # AirPlay pair-setup / pair-verify
│   │   ├── protocol/           # Device information, feature flags, and LAN interface selection
│   │   └── receiver/           # Receiver coordination
│   ├── build.ps1               # Backend build script
│   ├── go.mod
│   └── go.sum
├── frontend/                   # Flutter desktop application (yingping)
│   ├── assets/                 # Static assets, including logo.png
│   ├── lib/
│   │   ├── main.dart           # Main UI, window controls, and four settings tabs
│   │   ├── receiver_api.dart   # Client for the backend's local HTTP API
│   │   └── ui_prefs.dart       # UI preferences
│   ├── packages/
│   │   └── yingping_frames/    # Local C++ texture and shared-memory bridge plugin
│   ├── windows/                # Windows runner and CMake configuration
│   └── pubspec.yaml
├── scripts/
│   ├── fetch-ffmpeg.ps1        # Detect or download the pinned Windows x64 shared build (SHA256)
│   └── generate-ffmpeg-abi.ps1 # Generate FFmpeg ABI definitions
├── third_party/                # Local third-party dependencies, such as FFmpeg
├── analysis/                   # Security scan report and remediation patch (MirrPane/)
├── LICENSE                     # GPL-3.0 license
├── README.md                   # Traditional Chinese documentation
├── README.en.md                # English documentation
└── package.ps1                 # Build and package the portable distribution
```

---

## Quick Start

1. Download the latest release, such as `v0.3.4`, from [Releases](https://github.com/Miku0139oao/MirrPane/releases). Extract it and run `MirrPane.exe`. No installation or registry changes are required.
2. Connect the Windows PC and your iPhone or iPad to the same local network. Allow the receiver on private networks when Windows Firewall prompts you.
3. On your iPhone or iPad, open **Control Center → Screen Mirroring**, then select the receiver. Its default name is **鏡屏**.
4. Double-click the video or press `F11` to toggle full screen. The app includes keyboard shortcuts and four settings tabs for general preferences, connection information, live statistics, pairing identity reset, graphics, and audio.

---

## Local Development and Builds

### Requirements

- **Go**: `1.26.11+` (with the patched standard library; `go.mod` selects it via `GOTOOLCHAIN=auto`)
- **Flutter**: `3.47+` with Dart SDK `^3.13.5` (CI pins 3.47.7)
- **Build tools**: Visual Studio C++ Build Tools, CMake, and PowerShell

The desktop application targets Windows x64. Visual Studio C++ tools are needed for Flutter's Windows runner and texture plugin; the Go backend builds with CGO disabled.

### Build Steps

1. **Prepare the shared FFmpeg libraries** from the repository root. Downloads are verified using SHA256:

   ```powershell
   .\scripts\fetch-ffmpeg.ps1 -Pinned
   ```

2. **Build and test the Go backend**, starting from the repository root and returning there when finished:

   ```powershell
   $env:CGO_ENABLED = '0'
   # Point tests at the repository's pinned FFmpeg (Find no longer searches CWD or PATH)
   $env:MIRRPANE_FFMPEG_DIR = (Resolve-Path third_party\ffmpeg\bin).Path
   cd backend
   go build ./...
   go vet ./...
   go test ./...
   cd ..
   ```

3. **Build and package the complete application** from the repository root:

   ```powershell
   .\package.ps1
   ```

### Validation

- **Windows builds**: CI builds and tests the backend with `CGO_ENABLED=0` and packages the Flutter application.
- **Portable protocol tests**: Linux CI runs the Go protocol and cryptographic tests, including a FairPlay corpus of 1,024 cases checked against the original C implementation, and cross-compiles the complete Windows backend.

---

## `airplayd` Command-Line Options

The Flutter application normally starts and manages `airplayd` as a sidecar. It can also run independently for testing or recording a stream for offline replay and debugging.

| Option | Default | Description |
| :--- | :--- | :--- |
| `-name` | `"鏡屏"` | Receiver name used when the configuration is first created. Later launches use the stored name. |
| `-data` | `config` beside the executable | Directory for `config.json` and pairing keys. |
| `-port` | `0` (prefers `7000`) | AirPlay control port. With `0`, tries `7000` first, then selects an available port. |
| `-api-port` | `0` (automatic) | Local HTTP API port used by the Flutter UI. |
| `-width` | `0` | Virtual display width. With `0`, reads the value from `config.json`. |
| `-height` | `0` | Virtual display height. With `0`, reads the value from `config.json`. |
| `-fps` | `0` | Maximum advertised frame rate. With `0`, reads the value from `config.json`. |
| `-record` | Empty string | Save the raw H.264 mirroring stream to this directory for offline replay and debugging. |
| `-shm` | `Local\YingpingFrames` | Windows shared-memory section used to publish video frames to the UI. |
| `-log` | Empty string | Specify a log file and enable verbose logging. Without this option, logs are still written to the default `logs/airplayd.log`. |
| `-parent-pid` | `0` | Parent process ID, supplied by the UI. The backend exits when that process exits. |
| `-v` | `false` | Enable verbose logging. |

---

## FFmpeg Discovery and Network Troubleshooting

### FFmpeg Search Order

The backend resolves shared FFmpeg DLLs only from explicit trusted locations. It **never** searches `PATH` or the current working directory (DLL-planting defence). If no runtime is found, first-run setup downloads and verifies one; you can also use `backend/cmd/ffmpeg-setup` or `scripts/fetch-ffmpeg.ps1 -Pinned`.

1. The directory named by `MIRRPANE_FFMPEG_DIR` (authoritative — a bad path is an error, never a silent fallback).
2. The executable's directory, its `ffmpeg/` subdirectory, and the verified cache inside that `ffmpeg/` directory.
3. (Development) point `MIRRPANE_FFMPEG_DIR` explicitly at the repository's `third_party/ffmpeg/`; tests and CI do exactly this.

A compatible Windows x64 **shared build** is required. A statically linked `ffmpeg.exe` alone does not provide the DLLs the backend needs. Portable releases include the shared libraries and can start offline. See the [third-party runtime guide](third_party/README.md) for supported DLL versions and ABI maintenance.

### TUN / VPN and Device Discovery

If an iPhone or iPad cannot find the receiver, check the local network and firewall settings. TUN mode or VPN adapters can also affect the interface used for mDNS advertisements.

- MirrPane prefers an Ethernet or Wi-Fi LAN interface with an IPv4 gateway, including Hyper-V external switches. It excludes TUN interfaces, the `198.18.0.0/15` benchmarking range, and CGNAT addresses from LAN selection.
- If discovery still fails, confirm that both devices are on the same LAN and that Windows Firewall allows `airplayd` and local mDNS traffic on UDP port `5353`. Temporarily disabling the VPN's TUN mode can also help isolate routing problems.

---

## Security

An independent security scan and hardening round completed on 2026-11-22 (five finder classes with per-finding adversarial verification; 44% false-positive rate). This release includes:

- **Fixed**: stream keys are no longer written to logs; short HEVC parameter-set payloads can no longer panic the receiver; Go 1.26.11 removes 20 reachable standard-library CVEs; FFmpeg DLLs load only from trusted locations; the local API validates `Host`/`Origin` (including rejecting `Origin: null`); `POST /stop` is loopback-only; RTSP request fields are escaped in logs; new installations use a CSPRNG seed and a random `PairingID`, and `config.json` is written with mode 0600.
- **Hardened, residual risk**: `/setup` requires a completed pair-verify, mirror connections are bound to the sender IP and stream ID, half-open connections are reaped after 4 s, and audio keys belong to the owning sender. Legacy pair-verify proves possession of a self-selected key, not owner approval; a PIN/allowlist trust policy remains a follow-up. See SEC-001 and the follow-up list in [`analysis/MirrPane/SECURITY_FINDINGS.md`](analysis/MirrPane/SECURITY_FINDINGS.md).
- **Reporting**: please report vulnerabilities privately via GitHub *Security → Report a vulnerability* rather than a public issue.

---

## Dependencies and Acknowledgements

### Main Dependencies

- **Backend**: `github.com/ebitengine/purego`, `github.com/grandcat/zeroconf`, `howett.net/plist`, `golang.org/x/sys`, and `github.com/google/uuid`.
- **Frontend**: Flutter Windows Desktop and the project's C++ `yingping_frames` plugin.

### Acknowledgements

This project draws from and benefits from the following open-source projects:

- [UxPlay](https://github.com/FDH2/UxPlay)
- [FFmpeg](https://ffmpeg.org/)
- [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds)

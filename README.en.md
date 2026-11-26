# MirrPane (鏡屏)

<img src="assets/logo.png" width="80" alt="">

[繁體中文](README.md) | **English**

**An AirPlay screen mirroring receiver for Windows**, built with **Go + FFmpeg D3D11VA + MyGo**. Turn your Windows PC into a wireless display for an iPhone or iPad with a native desktop UI. Extract the release archive and run the app, with no installer, registry writes, or additional runtime installation required.

- **License**: [GPL-3.0 License](https://github.com/Miku0139oao/MirrPane/blob/main/LICENSE)
- **Repository**: [Miku0139oao/MirrPane](https://github.com/Miku0139oao/MirrPane)
- **Stable release v0.4.2** · [Download for Windows x64][win64] · [SHA-256 checksum][checksum] · [Release notes][release] · [Tagged source][source-tag]

---

## Features

- **Low-latency hardware decoding**: FFmpeg **D3D11VA** provides H.264 hardware acceleration. Complete Annex-B access units go directly to the decoder. Software decoding is available when hardware acceleration is unavailable and can also be selected in settings.
- **No CGO in the UI or backend**: Both the desktop UI (MyGo) and `airplayd` are built with `CGO_ENABLED=0`. The backend binds FFmpeg at runtime through `purego` and calls Windows system APIs from Go. No Visual Studio C++ compiler, MinGW, or FFmpeg development headers are needed.
- **Shared-memory frame rendering**: The backend publishes RGBA frames to a Windows shared-memory section unique to each UI instance (`Local\MirrPaneFrames-<pid>`). A three-slot mailbox and Windows event notifications deliver frames to the native MyGo UI (`internal/framefeed`), which maps the section directly and uploads it as a texture. This avoids image serialization between processes and keeps two instances from fighting over one section; a standalone `airplayd` still defaults to `Local\YingpingFrames`.
- **Direct-to-slot conversion with idle gating**: `sws_scale` writes straight into the claimed shared-memory slot (no full-frame copy per picture). When the UI is minimised or stops adopting frames for a second, the download, conversion and publish are skipped while decoding continues (reference frames), with a 1 Hz probe so the UI can re-adopt instantly on restore.
- **Low-latency audio playback**: AAC-ELD audio is decoded and played through Windows `winmm waveOut`. Supports live volume control, mute, and three latency/jitter profiles (low, balanced, stable) backed by a larger ring that absorbs Wi-Fi bursts. The ring gently catches up when a burst raises occupancy, so latency does not stay elevated for the rest of the session.
- **Live pipeline telemetry**: `/api/state` reports fps, mean/p95 for decode, GPU download, colour conversion and publish, consumer adoption progress, and audio buffer metrics.
- **Automatic FFmpeg management**: Pins a dated upstream build (SHA256 and ABI checked), detects existing runtimes, and supports resumable first-run downloads. DLLs are resolved only from explicit trusted locations — never `PATH` or the working directory.
- Mirroring and audio streams use the approved device’s pairing identity and are bound to the sender. The local management API validates `Host` and `Origin`. See [Security](#security).
- **Portable distribution**: Settings (`config.json`), UI preferences, and pairing keys stay in the application directory. The app does not write to the Windows registry and needs no administrator rights. The one exception is the MyGo shader cache that the UI creates under `%LOCALAPPDATA%\MyGo\ShaderCache` on first launch.

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
  └── Audio: AAC-ELD → Windows winmm waveOut (low / balanced / stable)
        │
        ▼  Video: three-slot shared-memory mailbox (Local\MirrPaneFrames-<pid>)
        │         + Windows event notifications
        │  State and controls: local HTTP API (IPC)
        ▼
MyGo native desktop UI (backend/cmd/mirrpane, CGO_ENABLED=0)
  └── framefeed shared-memory reader → ui.Bitmap texture
```

---

## Project Layout

```text
MirrPane/
├── .github/workflows/          # CI and release workflows
├── backend/                    # Go module: github.com/miku0139/airplay-go/backend
│   ├── cmd/
│   │   ├── airplayd/           # Main sidecar and local HTTP API
│   │   ├── ffmpeg-setup/       # Shared FFmpeg DLL download, verification, and installation
│   │   └── mirrpane/           # Native MyGo desktop UI (main.go, app.go, sidecar.go, prefs.go, …)
│   ├── internal/
│   │   ├── audio/              # Audio stream reception, decryption, and AAC-ELD decoding flow
│   │   ├── audioout/           # Windows waveOut playback and jitter buffering
│   │   ├── config/             # config.json persistence
│   │   ├── decode/             # purego FFmpeg bindings and video/audio decoding
│   │   ├── fairplay/           # FairPlay key exchange and decryption
│   │   ├── ffmpeg/             # Runtime discovery, download, verification, and caching
│   │   ├── framefeed/          # Shared-memory frame reader used by the UI
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
├── assets/                     # Project logo and Windows icon
├── scripts/
│   ├── fetch-ffmpeg.ps1        # Detect or download the pinned Windows x64 shared build (SHA256)
│   ├── generate-ffmpeg-abi.ps1 # Generate FFmpeg ABI definitions
│   └── third-party-notices.ps1 # Collect the licences of bundled Go modules
├── third_party/                # Local third-party dependencies, such as FFmpeg
├── analysis/                   # Security scan report and remediation patch (MirrPane/)
├── LICENSE                     # GPL-3.0 license
├── README.md                   # Traditional Chinese documentation
├── README.en.md                # English documentation
├── VERSION                     # Release version read by package.ps1
└── package.ps1                 # Build, package, and SHA256 script
```

---

## Quick Start

1. Download the [Windows x64 archive][win64], extract it to a writable folder, and run `MirrPane.exe`. To verify the download, compare its SHA-256 from `Get-FileHash` with the [published checksum][checksum].
2. Connect the Windows PC and your iPhone or iPad to the same local network. Allow the receiver on private networks when Windows Firewall prompts you.
3. On your iPhone or iPad, open **Control Center → Screen Mirroring**, then select the receiver. Its default name is **鏡屏**.
4. On the first connection, check the device name on the PC and click **允許連線** (Allow connection). Select the receiver again on the iPhone or iPad. Later connections use the approved pairing identity.
5. Keyboard shortcuts include `F11` (full screen), `Ctrl+T` (always on top), `Ctrl+,` / `Ctrl+K` (settings), `Ctrl+L` (logs), and `Ctrl+S` (snapshot). The settings window has general, graphics/decoding, rendering, and audio sections.

---

## Local Development and Builds

### Requirements

- **Go**: `1.27.4+` (`go.mod` selects the toolchain via `GOTOOLCHAIN=auto`)
- Use PowerShell on Windows x64 to build the portable package. Both the MyGo UI and the receiver compile with `CGO_ENABLED=0`.

### Build Steps

1. **Prepare the shared FFmpeg libraries** from the repository root. Downloads are verified using SHA256:

   ```powershell
   .\scripts\fetch-ffmpeg.ps1 -Pinned
   ```

2. **Build and test the Go UI and backend**, starting from the repository root and returning there when finished:

   ```powershell
   $env:CGO_ENABLED = '0'
   # Point tests at the repository's pinned FFmpeg (Find no longer searches CWD or PATH)
   $env:MIRRPANE_FFMPEG_DIR = @(.\scripts\fetch-ffmpeg.ps1 -Pinned -PassThru)[-1]
   cd backend
   go build ./...
   go vet ./...
   go test ./...
   cd ..
   ```

   For UI development, run the native app and point it at any packaged folder that contains `backend\airplayd.exe`:

   ```powershell
   cd backend
   $version = (Get-Content ..\VERSION -Raw).Trim()
   go run ./cmd/mirrpane -install-dir "..\dist\鏡屏-$version"
   ```

3. **Build and package the complete application** from the repository root:

   ```powershell
   .\package.ps1
   ```

   The version defaults to `VERSION`; `-Version 0.0.0-test1` produces a separate test build. Output is the runnable folder `dist\鏡屏-<version>\` plus `dist\MirrPane-<version>-win64.zip` and its `.sha256`. Every package builds inside a unique `dist\.staging` folder (Go's content-addressed build cache skips unchanged work), so shared `backend\*.exe` and `*.dll` files are never overwritten. `-SkipBuild` is accepted for compatibility with older command lines but still runs the cached builds. Packaging never replaces a folder that is currently running; re-packaging an existing install preserves its `config\`, `logs\`, `screenshots\`, and unknown user files (including additions under `backend\`), and keeps the old install as a `dist\.staging\previous-*` backup.

### Validation

- **Windows builds**: CI builds and tests the native UI and backend with `CGO_ENABLED=0`, then runs `package.ps1` to produce the portable archive and SHA256 checksum.
- **Portable protocol tests**: Linux CI runs the Go protocol and cryptographic tests, including a FairPlay corpus of 1,024 cases checked against the original C implementation, and cross-compiles the complete Windows application.

---

## `airplayd` Command-Line Options

The native MyGo UI normally starts and manages `airplayd` as a sidecar. It can also run independently for testing or recording a stream for offline replay and debugging.

| Option | Default | Description |
| :--- | :--- | :--- |
| `-name` | `"鏡屏"` | Receiver name used when the configuration is first created. Later launches use the stored name. |
| `-data` | `config` beside the executable | Directory for `config.json` and pairing keys. |
| `-port` | `0` (prefers `7000`) | AirPlay control port. With `0`, tries `7000` first, then selects an available port. |
| `-api-port` | `0` (automatic) | Local HTTP API port used by the UI. |
| `-width` | `0` | Virtual display width. With `0`, reads the value from `config.json`. |
| `-height` | `0` | Virtual display height. With `0`, reads the value from `config.json`. |
| `-fps` | `0` | Maximum advertised frame rate. With `0`, reads the value from `config.json`. |
| `-record` | Empty string | Save the raw H.264 mirroring stream to this directory for offline replay and debugging. |
| `-shm` | `Local\YingpingFrames` | Windows shared-memory section used to publish video frames. The UI passes a per-instance `Local\MirrPaneFrames-<pid>`. |
| `-log` | Empty string | Specify a log file and enable verbose logging. Without this option, logs are still written to the default `logs/airplayd.log`. |
| `-parent-pid` | `0` | Parent process ID, supplied by the UI. The backend exits when that process exits. |
| `-v` | `false` | Enable verbose logging. |

### `MirrPane.exe` UI Options

| Option | Default | Description |
| :--- | :--- | :--- |
| `-install-dir` | The executable's directory | Override the installation root (must contain `backend\airplayd.exe`). Required when running `go run ./cmd/mirrpane`. |
| `-shm` | `Local\MirrPaneFrames-<pid>` | Shared-memory section shared with the sidecar; the UI passes the same name to its child. |
| `-demo` | `false` | Show a synthetic test picture without starting the sidecar (UI development). |
| `-demo-settings` | `false` | Open the settings window at startup (UI testing). |

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

The receiver checks the sender’s pairing public key against the approved devices in `config\trusted_senders.json`. Choosing **清除信任裝置** (Clear trusted devices) in Settings ends the current session and requires senders to obtain approval again. [Trust store implementation](backend/internal/trust/store.go)

Windows protects the receiver’s pairing key seed with DPAPI. File permissions restrict access to the configuration and trusted-device list. The management API listens on loopback.

---

## Dependencies and Acknowledgements

### Main Dependencies

- **UI**: `github.com/egoist/mygo` (native Windows UI, with transitive modules such as `github.com/go-text/typesetting` and `golang.org/x/image`; bundled licences are listed in the release's `THIRD_PARTY_LICENSES.txt`).
- **Backend**: `github.com/ebitengine/purego`, `github.com/grandcat/zeroconf`, `howett.net/plist`, `golang.org/x/sys`, and `github.com/google/uuid`.

### Acknowledgements

This project draws from and benefits from the following open-source projects:

- [UxPlay](https://github.com/FDH2/UxPlay)
- [FFmpeg](https://ffmpeg.org/)
- [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds)
- [MyGo](https://github.com/egoist/mygo)

[release]: https://github.com/Miku0139oao/MirrPane/releases/tag/v0.4.2
[win64]: https://github.com/Miku0139oao/MirrPane/releases/download/v0.4.2/MirrPane-0.4.2-win64.zip
[checksum]: https://github.com/Miku0139oao/MirrPane/releases/download/v0.4.2/MirrPane-0.4.2-win64.zip.sha256
[source-tag]: https://github.com/Miku0139oao/MirrPane/tree/v0.4.2

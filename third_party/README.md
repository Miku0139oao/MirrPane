# FFmpeg runtime

MirrPane uses FFmpeg for H.264/AAC-ELD decoding, color conversion and resampling.
The Go backend calls its shared libraries using [purego](https://github.com/ebitengine/purego)
and the Windows x64 ABI. The native MyGo UI and the receiver both build with
`CGO_ENABLED=0`. Packaging needs Go, PowerShell, and the shared runtime DLLs.

## Detecting and downloading

`scripts/fetch-ffmpeg.ps1 -Pinned` verifies the four DLLs and their licence
against the [pinned file hashes](../backend/internal/ffmpeg/pinned-files.sha256).
When the cache needs repair, the Go `cmd/ffmpeg-setup` helper downloads the
pinned Windows x64 GPL shared archive and checks its size and SHA-256 before
installing the runtime. Use `-Force` to fetch the pinned build again and
`-PassThru` to return the DLL directory to a build script.

The application performs the same setup automatically if no shared runtime can
be found beside the sidecar or in its portable cache. Set
`MIRRPANE_FFMPEG_DIR` to an explicit directory to override discovery. `PATH`
and the current working directory are never consulted. An invalid override is
reported as an error so it cannot silently use another installation. A static
`ffmpeg.exe` alone does not provide the required DLLs.

Normal release archives include the four DLLs beside `backend/airplayd.exe` and
work offline. First-run downloads, when needed, stay under `backend/ffmpeg/`.
Existing installations are reused without an automatic update on each launch.

## Supported ABI

| Library | DLL | Minimum version |
| --- | --- | --- |
| libavcodec | `avcodec-63.dll` | 63.28.100 |
| libavutil | `avutil-61.dll` | 61.21.100 |
| libswscale | `swscale-10.dll` | 10.6.100 |
| libswresample | `swresample-7.dll` | 7.7.100 |

The checked-in layouts were generated from BtbN's
`N-129166-gae9f4540cb-20261121` headers. Matching major versions are required and
older minor versions are rejected before any native structure is accessed.
Missing exports and incompatible runtimes return ordinary Go errors. DLLs are
loaded with absolute paths and scoped dependency lookup; no global PATH or
system-wide installation is modified. `decode.Version()` reports the loaded build.

FFmpeg's public API is documented in the upstream
[AVCodecContext](https://ffmpeg.org/doxygen/trunk/structAVCodecContext.html) and
[AVFrame](https://ffmpeg.org/doxygen/trunk/structAVFrame.html) references.

## Maintainer ABI upgrades

This is an optional maintenance operation, not part of a normal build:

1. Obtain the matching FFmpeg shared development headers from
   [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds/releases).
2. Run `scripts/generate-ffmpeg-abi.ps1 -Headers <include-directory> -Compiler <gcc.exe>`.
   The generator compiles a temporary layout probe and emits Go structure padding,
   constants, and compile-time assertions for every accessed offset and structure size.
3. Update the supported DLL names in the runtime manager and PowerShell helpers
   if the major versions change. Check all public function signatures against
   the new headers and run the fixture tests with both GPU and software decoding.
4. Keep rejecting newer unsupported ABIs; downloading a newer release must not
   bypass the structure-layout checks.

MirrPane and the downloaded GPL FFmpeg build are distributed under GPLv3; see the
repository's LICENSE and the upstream build's licensing information.

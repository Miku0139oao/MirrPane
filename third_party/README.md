# Third-party runtime libraries

## FFmpeg (shared, GPL build)

`airplayd.exe` decodes H.264 and AAC-ELD through FFmpeg's shared libraries
(`libavcodec`, `libavutil`, `libswscale`, `libswresample`). They are not
redistributable from this repository, so the development package lives here
untracked and the build script stages the DLLs beside the executable.

Both this project and the FFmpeg build are **GPLv3**, which is what allows the
GPL-only parts (currently none, but it keeps the licensing simple) to be used.

### Fetching the package

```powershell
mkdir third_party
cd third_party
curl.exe -L -o ffmpeg-shared.zip `
  https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl-shared.zip
tar -xf ffmpeg-shared.zip
Move-Item ffmpeg-master-latest-win64-gpl-shared ffmpeg
Remove-Item ffmpeg-shared.zip
```

Then trim what is not used to keep the tree small (the shared build also ships
`avdevice`, `avfilter` and `avformat`, which a receiver does not need):

```powershell
Remove-Item ffmpeg\bin\ffmpeg.exe, ffmpeg\bin\ffplay.exe, ffmpeg\bin\ffprobe.exe
Remove-Item ffmpeg\bin\avdevice-*.dll, ffmpeg\bin\avfilter-*.dll, ffmpeg\bin\avformat-*.dll
Remove-Item ffmpeg\lib\*.lib, ffmpeg\lib\libavdevice.dll.a, ffmpeg\lib\libavfilter.dll.a, ffmpeg\lib\libavformat.dll.a
Remove-Item ffmpeg\lib\pkgconfig -Recurse
```

### Layout the build expects

```
third_party/ffmpeg/
├─ include/    libavcodec, libavutil, libswscale, libswresample headers
├─ lib/        libavcodec.dll.a, libavutil.dll.a, libswscale.dll.a, libswresample.dll.a
└─ bin/        the DLLs that ship with the product
```

### Notes

- The import libraries are MinGW `.dll.a` files, which is what the cgo linker
  (MinGW-w64 GCC) consumes directly. No `.def` conversion is needed.
- The DLLs are versioned (`avcodec-63.dll` and so on). A FFmpeg upgrade renames
  them, so `build.ps1` copies whatever is present rather than a fixed list.
- `decode.Version()` reports the linked build; the about screen shows it, and a
  mismatch between that string and the DLLs on disk means a stale copy is being
  loaded.

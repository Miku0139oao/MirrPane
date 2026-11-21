# Fetches and trims BtbN's shared GPL FFmpeg build into third_party\ffmpeg.
# Idempotent: exits immediately when headers, import libs and DLLs already exist.

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$thirdParty = Join-Path $root 'third_party'
$ffmpegDir = Join-Path $thirdParty 'ffmpeg'
$binDir = Join-Path $ffmpegDir 'bin'
$libDir = Join-Path $ffmpegDir 'lib'

$hasDll = (Test-Path $binDir) -and ((Get-ChildItem -Path $binDir -Filter 'avcodec-*.dll' -ErrorAction SilentlyContinue).Count -gt 0)
$hasLib = Test-Path (Join-Path $libDir 'libavcodec.dll.a')
if ($hasDll -and $hasLib) {
    Write-Host "FFmpeg already present at $ffmpegDir"
    exit 0
}

$url = 'https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl-shared.zip'
$zipPath = Join-Path $thirdParty 'ffmpeg-shared.zip'
$extractDir = Join-Path $thirdParty 'extract'

New-Item -ItemType Directory -Path $thirdParty -Force | Out-Null
if (Test-Path $extractDir) { Remove-Item $extractDir -Recurse -Force }
if (Test-Path $ffmpegDir) { Remove-Item $ffmpegDir -Recurse -Force }

Write-Host "Downloading FFmpeg win64 shared build..."
& curl.exe -fsSL -o $zipPath $url
if ($LASTEXITCODE -ne 0) {
    throw "curl failed to download FFmpeg ($LASTEXITCODE)"
}

New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
& tar -xf $zipPath -C $extractDir
if ($LASTEXITCODE -ne 0) {
    throw "tar failed to extract FFmpeg ($LASTEXITCODE)"
}

$inner = Get-ChildItem -Path $extractDir -Directory | Select-Object -First 1
if ($null -eq $inner) {
    throw "unexpected archive layout in $extractDir"
}

Move-Item -Path $inner.FullName -Destination $ffmpegDir
Remove-Item -Path $zipPath -Force -ErrorAction SilentlyContinue
Remove-Item -Path $extractDir -Recurse -Force -ErrorAction SilentlyContinue

# Trim binaries and libraries the receiver does not use to keep cache/workspace small.
Remove-Item -Path (Join-Path $binDir 'ffmpeg.exe'), (Join-Path $binDir 'ffplay.exe'), (Join-Path $binDir 'ffprobe.exe') -Force -ErrorAction SilentlyContinue
Remove-Item -Path (Join-Path $binDir 'avdevice-*.dll'), (Join-Path $binDir 'avfilter-*.dll'), (Join-Path $binDir 'avformat-*.dll') -Force -ErrorAction SilentlyContinue
Remove-Item -Path (Join-Path $libDir '*.lib'), (Join-Path $libDir 'libavdevice.dll.a'), (Join-Path $libDir 'libavfilter.dll.a'), (Join-Path $libDir 'libavformat.dll.a') -Force -ErrorAction SilentlyContinue
Remove-Item -Path (Join-Path $libDir 'pkgconfig') -Recurse -Force -ErrorAction SilentlyContinue

$ready = (Get-ChildItem -Path $binDir -Filter 'avcodec-*.dll' -ErrorAction SilentlyContinue).Count -gt 0
if (-not $ready) {
    throw "FFmpeg DLL verification failed in $binDir"
}
Write-Host "FFmpeg ready at $ffmpegDir"

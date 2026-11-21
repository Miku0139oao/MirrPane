# Build the cgo-free Go sidecar, then stage the shared FFmpeg runtime beside it.
param(
    [switch]$Test,
    [string]$Output = "$PSScriptRoot\airplayd.exe"
)
$ErrorActionPreference = 'Stop'
$previousCGO = $env:CGO_ENABLED
$previousRuntime = $env:MIRRPANE_FFMPEG_DIR
Push-Location $PSScriptRoot
try {
    $env:CGO_ENABLED = '0'
    $ffmpegBin = & (Join-Path $PSScriptRoot '..\scripts\fetch-ffmpeg.ps1') -Pinned -PassThru
    $env:MIRRPANE_FFMPEG_DIR = $ffmpegBin
    if ($Test) {
        go test ./...
        if ($LASTEXITCODE -ne 0) { throw 'tests failed' }
    }
    # No console window; diagnostics go to logs/airplayd.log.
    go build -trimpath -ldflags '-s -w -H windowsgui' -o $Output ./cmd/airplayd
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }
    $destination = Split-Path -Parent ([IO.Path]::GetFullPath($Output))
    foreach ($name in 'avutil-61.dll', 'swresample-7.dll', 'swscale-10.dll', 'avcodec-63.dll') {
        Copy-Item -LiteralPath (Join-Path $ffmpegBin $name) -Destination (Join-Path $destination $name) -Force
    }
    Copy-Item -LiteralPath (Join-Path $ffmpegBin 'COPYING') -Destination (Join-Path $destination 'COPYING') -Force
    Write-Host "built $Output (CGO_ENABLED=0) with FFmpeg runtime from $ffmpegBin"
}
finally {
    Pop-Location
    $env:CGO_ENABLED = $previousCGO
    $env:MIRRPANE_FFMPEG_DIR = $previousRuntime
}

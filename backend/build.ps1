# Builds the Go sidecar and stages the runtime DLLs beside it.
#
# FFmpeg is shipped as shared libraries, so airplayd.exe cannot start without
# them. Windows resolves a DLL from the executable's own directory first, which
# is what keeps the product portable: the DLLs travel with the exe and no PATH
# or system-wide install is involved.
#
# Usage:  .\build.ps1            build and stage
#         .\build.ps1 -Test      also run the test suite

param(
    [switch]$Test,
    [string]$Output = "$PSScriptRoot\airplayd.exe"
)

$ErrorActionPreference = 'Stop'

$ffmpegBin = Join-Path $PSScriptRoot '..\third_party\ffmpeg\bin'
if (-not (Test-Path $ffmpegBin)) {
    throw "FFmpeg runtime missing at $ffmpegBin. See third_party/README.md."
}

Push-Location $PSScriptRoot
try {
    if ($Test) {
        # The test binary is built into a temp folder, so the loader finds the
        # DLLs only through PATH (they are resolved before any test code runs).
        $env:PATH = "$ffmpegBin;$env:PATH"
        go test ./internal/...
        if ($LASTEXITCODE -ne 0) { throw "tests failed" }
    }

    # -H windowsgui gives the sidecar no console window. The Flutter app is a GUI
    # process, so a console child would otherwise pop up its own black window and
    # sit there for the life of the receiver. Diagnostics go to logs/airplayd.log.
    go build -trimpath -ldflags "-H windowsgui" -o $Output ./cmd/airplayd
    if ($LASTEXITCODE -ne 0) { throw "build failed" }

    $dest = Split-Path -Parent $Output
    foreach ($dll in Get-ChildItem (Join-Path $ffmpegBin '*.dll')) {
        Copy-Item $dll.FullName (Join-Path $dest $dll.Name) -Force
    }
    Write-Host "built $Output with FFmpeg runtime from $ffmpegBin"
}
finally {
    Pop-Location
}

# Detect the local shared runtime, or download and verify the latest BtbN build.
# Only runtime DLLs are needed; no headers, import libraries, or MinGW.
param([switch]$Force, [switch]$PassThru)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$runtimeRoot = Join-Path $root 'third_party\ffmpeg'
$required = 'avutil-61.dll', 'swresample-7.dll', 'swscale-10.dll', 'avcodec-63.dll'

function Test-Runtime([string]$Directory) {
    foreach ($dll in $required) {
        $file = Join-Path $Directory $dll
        if (-not (Test-Path -LiteralPath $file -PathType Leaf) -or (Get-Item -LiteralPath $file).Length -eq 0) { return $false }
    }
    return $true
}

$runtimeDir = Join-Path $runtimeRoot 'bin'
if (-not (Test-Runtime $runtimeDir)) {
    $marker = Join-Path $runtimeRoot 'current'
    if (Test-Path -LiteralPath $marker) {
        $name = (Get-Content -LiteralPath $marker -Raw).Trim()
        if ($name -match '^runtime-[0-9a-f]{16}$') {
            $runtimeDir = Join-Path $runtimeRoot $name
        }
    }
}
if ($Force -or -not (Test-Runtime $runtimeDir)) {
    $previousCGO = $env:CGO_ENABLED
    Push-Location (Join-Path $root 'backend')
    try {
        $env:CGO_ENABLED = '0'
        $result = & go run ./cmd/ffmpeg-setup -output $runtimeRoot
        if ($LASTEXITCODE -ne 0) { throw 'FFmpeg runtime setup failed' }
        $runtimeDir = ($result | Select-Object -Last 1).Trim()
    }
    finally {
        Pop-Location
        $env:CGO_ENABLED = $previousCGO
    }
}
if (-not (Test-Runtime $runtimeDir)) { throw "FFmpeg runtime is incomplete: $runtimeDir" }
Write-Host "FFmpeg runtime ready at $runtimeDir"
if ($PassThru) { Write-Output $runtimeDir }

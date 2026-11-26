# Detect the local shared runtime, or download and verify the pinned BtbN build.
# Only runtime DLLs are needed; no headers, import libraries, or MinGW.
param([switch]$Force, [switch]$PassThru, [switch]$Pinned)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$runtimeRoot = Join-Path $root 'third_party\ffmpeg'
$required = 'avutil-61.dll', 'swresample-7.dll', 'swscale-10.dll', 'avcodec-63.dll'

function Test-Manifest([string]$Directory) {
    # New caches record every extracted file's SHA-256. A damaged DLL that is
    # still non-empty therefore fails the check and triggers a re-download.
    $sums = Join-Path $Directory 'SHA256SUMS'
    if ($Pinned) { $sums = Join-Path $root 'backend\internal\ffmpeg\pinned-files.sha256' }
    if ($Pinned -and -not (Test-Path -LiteralPath $sums -PathType Leaf)) { throw 'FFmpeg pinned file inventory is missing' }
    if (-not (Test-Path -LiteralPath $sums -PathType Leaf)) { return $true }
    $recorded = @{}
    foreach ($line in @(Get-Content -LiteralPath $sums)) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        if ($line -cnotmatch '^([0-9a-f]{64})  (\S+)$') { return $false }
        $digest = $Matches[1]
        $name = $Matches[2]
        if ($name -cne [IO.Path]::GetFileName($name) -or $name -ceq 'SHA256SUMS') { return $false }
        $file = Join-Path $Directory $name
        if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { return $false }
        if ((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant() -cne $digest) { return $false }
        $recorded[$name] = $true
    }
    foreach ($dll in $required) {
        if (-not $recorded.ContainsKey($dll)) { return $false }
    }
    return $true
}

function Test-Runtime([string]$Directory) {
    foreach ($dll in $required) {
        $file = Join-Path $Directory $dll
        if (-not (Test-Path -LiteralPath $file -PathType Leaf) -or (Get-Item -LiteralPath $file).Length -eq 0) { return $false }
    }
    return Test-Manifest $Directory
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
if ($Pinned) {
    # Read the Go pin instead of maintaining a second digest in PowerShell.
    $manager = Get-Content -LiteralPath (Join-Path $root 'backend\internal\ffmpeg\runtime.go') -Raw
    if ($manager -notmatch 'assetSHA256\s*=\s*"([0-9a-f]{64})"') { throw 'FFmpeg SHA256 pin missing' }
    $runtimeDir = Join-Path $runtimeRoot ('runtime-' + $Matches[1].Substring(0, 16))
}
if ($Force -or -not (Test-Runtime $runtimeDir) -or ($Pinned -and -not (Test-Path -LiteralPath (Join-Path $runtimeDir 'COPYING')))) {
    $previousCGO = $env:CGO_ENABLED
    Push-Location (Join-Path $root 'backend')
    try {
        $env:CGO_ENABLED = '0'
        # -Force must replace the cache even when it still looks complete;
        # otherwise a damaged or stale pinned runtime would be reused.
        $setupArgs = @('-output', $runtimeRoot)
        if ($Force) { $setupArgs += '-force' }
        $result = & go run ./cmd/ffmpeg-setup @setupArgs
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

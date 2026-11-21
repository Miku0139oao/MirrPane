# Builds the portable release folder and its zip.
#
# The deliverable is a folder that runs where it is unzipped: no installer, no
# registry keys, no AppData writes, no system-wide runtimes. Everything the
# program needs travels with it.
#
# Usage:  .\package.ps1 [-Version 0.3.1] [-SkipBuild] [-Flutter C:\path\to\flutter.bat]

param(
    [string]$Version = "",
    [switch]$SkipBuild,
    [string]$Flutter = ""
)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($Version)) {
    $pubspec = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'frontend\pubspec.yaml') -Raw
    if ($pubspec -notmatch '(?m)^version:\s*([^+\s]+)') { throw 'frontend/pubspec.yaml version missing' }
    $Version = $Matches[1]
}
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$') { throw 'invalid release version' }

$root = $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($Flutter)) {
    $cmd = Get-Command flutter.bat -ErrorAction SilentlyContinue
    if ($null -eq $cmd) {
        $cmd = Get-Command flutter -ErrorAction SilentlyContinue
    }
    if ($null -ne $cmd) {
        $Flutter = $cmd.Source
    } else {
        $Flutter = 'D:\DevTools\flutter\bin\flutter.bat'
    }
}
if (-not $SkipBuild -and -not (Test-Path $Flutter)) {
    throw "flutter executable not found; add flutter to PATH or pass -Flutter"
}
$flutter = $Flutter
$appName = '鏡屏'
$dist = Join-Path $root "dist\$appName-$Version"
$zip = Join-Path $root "dist\MirrPane-$Version-win64.zip"

Write-Host "packaging $appName $Version"

# --- build -------------------------------------------------------------------

if (-not $SkipBuild) {
    Push-Location (Join-Path $root 'frontend')
    try {
        # One value drives the UI, Windows version resources, and zip name.
        & $flutter build windows --release "--build-name=$Version" "--dart-define=MIRRPANE_VERSION=$Version"
        if ($LASTEXITCODE -ne 0) { throw "flutter build failed" }
    }
    finally { Pop-Location }
}

$release = Join-Path $root 'frontend\build\windows\x64\runner\Release'
if (-not (Test-Path (Join-Path $release 'MirrPane.exe'))) {
    throw "release build missing at $release"
}

# --- stage -------------------------------------------------------------------

$dist = [IO.Path]::GetFullPath($dist)
$expectedDist = [IO.Path]::GetFullPath((Join-Path $root "dist\$appName-$Version"))
if ($dist -ne $expectedDist) { throw 'unexpected release staging directory' }
if (Test-Path -LiteralPath $dist) { Remove-Item -LiteralPath $dist -Recurse -Force }
New-Item -ItemType Directory -Path $dist -Force | Out-Null

# The Flutter output, minus anything this machine happens to have written next to
# it: a shipped copy must start with no pairing identity of its own.
Get-ChildItem $release -File | Copy-Item -Destination $dist -Force
Copy-Item (Join-Path $release 'data') -Destination $dist -Recurse -Force

# The sidecar and the FFmpeg libraries it loads from its own folder.
& (Join-Path $root 'backend\build.ps1') -Output (Join-Path $dist 'backend\airplayd.exe') | Out-Null
if ($LASTEXITCODE -ne 0) { throw "sidecar build failed" }

# The Visual C++ runtime Flutter's runner links against. Windows does not always
# have it, and asking the user to install it would break "unzip and run".
$crt = 'msvcp140.dll', 'vcruntime140.dll', 'vcruntime140_1.dll'
# Exact redistributable toolset; never choose a runner's newest installed CRT.
$crtVersion = '14.44.35112'
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
$vsRoot = $null
if (Test-Path -LiteralPath $vswhere) { $vsRoot = & $vswhere -latest -products '*' -property installationPath }
$crtRoot = if ($vsRoot) { Join-Path $vsRoot "VC\Redist\MSVC\$crtVersion\x64\Microsoft.VC143.CRT" } else { '' }
$hasPinnedCRT = $crtRoot -and (@($crt | Where-Object { -not (Test-Path -LiteralPath (Join-Path $crtRoot $_)) }).Count -eq 0)
if (-not $hasPinnedCRT) {
    Write-Warning "Pinned Visual Studio CRT $crtVersion is absent; falling back to System32 (package CRT is machine-dependent)."
    $crtRoot = Join-Path $env:SystemRoot 'System32'
}
foreach ($dll in $crt) {
    $src = Join-Path $crtRoot $dll
    if (Test-Path $src) {
        Copy-Item $src -Destination $dist -Force
    } else {
        Write-Warning "$dll not found; the app may need the VC++ redistributable"
    }
}

New-Item -ItemType Directory -Path (Join-Path $dist 'logs') -Force | Out-Null

# Ship both GPL texts and exact upstream/source/build pointers with the DLLs.
Copy-Item -LiteralPath (Join-Path $root 'LICENSE') -Destination (Join-Path $dist 'LICENSE') -Force
Copy-Item -LiteralPath (Join-Path $dist 'backend\COPYING') -Destination (Join-Path $dist 'COPYING') -Force
$runtimeManager = Get-Content -LiteralPath (Join-Path $root 'backend\internal\ffmpeg\runtime.go') -Raw
if ($runtimeManager -notmatch 'releaseTag\s*=\s*"([^"]+)"') { throw 'FFmpeg release pin missing' }
$ffmpegTag = $Matches[1]
if ($runtimeManager -notmatch 'assetSHA256\s*=\s*"([0-9a-f]{64})"') { throw 'FFmpeg SHA256 pin missing' }
$ffmpegDigest = $Matches[1]
$sourceNotice = @"
MirrPane ${Version}: GNU GPL version 3; see LICENSE.
Application source: https://github.com/Miku0139oao/MirrPane
FFmpeg shared runtime: N-129166-gae9f4540cb-20261121; GNU GPL version 3; see COPYING.
Pinned binary/build release: https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/tag/$ffmpegTag
Pinned binary SHA256: $ffmpegDigest
FFmpeg corresponding source: https://github.com/FFmpeg/FFmpeg/tree/ae9f4540cb6a549118aacef8361dc2f8da9d5f4a
Build scripts, dependency versions, patches and configuration:
https://github.com/FFmpeg-Builds/FFmpeg-Builds/tree/7f2a6b1050d302144de8366afa37a75e46f326aa
Obtain all enabled dependencies using the matching build scripts, not just FFmpeg's source tree.
Release distributors must provide the complete corresponding source for their conveyed build.
"@
Set-Content -LiteralPath (Join-Path $dist 'SOURCE.txt') -Value $sourceNotice -Encoding utf8

# --- readme ------------------------------------------------------------------

$readme = @'
鏡屏 (MirrPane) — AirPlay 螢幕接收器

用法
  1. 解壓到任何資料夾（不要放在需要系統管理員權限的位置）。
  2. 執行 MirrPane.exe。
  3. 在 iPhone 或 iPad 開啟「控制中心」→「螢幕鏡像輸出」，選擇「鏡屏」。

第一次啟動時 Windows 會詢問防火牆權限，請選擇「允許存取」。
沒有允許的話，裝置看得到這台電腦，卻無法連上。

這個程式不會寫入登錄表或使用者資料夾，所有檔案都在這個資料夾內：
  config\   裝置設定與配對金鑰（刪除後會重新產生一組新身分）
  logs\     執行記錄，發生問題時請提供 airplayd.log

授權
  GPLv3。本程式包含 FFmpeg（GPL）與 Flutter（BSD）。
'@
Set-Content -Path (Join-Path $dist 'README.txt') -Value $readme -Encoding utf8

# --- zip ---------------------------------------------------------------------

$size = (Get-ChildItem $dist -Recurse -File | Measure-Object -Property Length -Sum).Sum / 1MB
Write-Host ("staged {0:N0} MB in {1}" -f $size, $dist)

if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path (Join-Path $dist '*') -DestinationPath $zip -CompressionLevel Optimal
$zipSize = (Get-Item $zip).Length / 1MB
Write-Host ("wrote {0} ({1:N0} MB)" -f $zip, $zipSize)

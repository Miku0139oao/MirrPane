# Builds the portable release folder and its zip.
#
# The deliverable is a folder that runs where it is unzipped: no installer, no
# registry keys, no AppData writes, no system-wide runtimes. Everything the
# program needs travels with it.
#
# Usage:  .\package.ps1 [-Version 0.2.0] [-SkipBuild] [-Flutter C:\path\to\flutter.bat]

param(
    [string]$Version = "0.2.0",
    [switch]$SkipBuild,
    [string]$Flutter = ""
)

$ErrorActionPreference = 'Stop'

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
$dist = Join-Path $root "dist\$appName"
$zip = Join-Path $root "dist\MirrPane-$Version-win64.zip"

Write-Host "packaging $appName $Version"

# --- build -------------------------------------------------------------------

if (-not $SkipBuild) {
    Push-Location (Join-Path $root 'frontend')
    try {
        & $flutter build windows --release --no-tree-shake-icons
        if ($LASTEXITCODE -ne 0) { throw "flutter build failed" }
    }
    finally { Pop-Location }
}

$release = Join-Path $root 'frontend\build\windows\x64\runner\Release'
if (-not (Test-Path (Join-Path $release 'MirrPane.exe'))) {
    throw "release build missing at $release"
}

# --- stage -------------------------------------------------------------------

if (Test-Path $dist) { Remove-Item $dist -Recurse -Force }
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
foreach ($dll in $crt) {
    $src = Join-Path $env:SystemRoot "System32\$dll"
    if (Test-Path $src) {
        Copy-Item $src -Destination $dist -Force
    } else {
        Write-Warning "$dll not found; the app may need the VC++ redistributable"
    }
}

New-Item -ItemType Directory -Path (Join-Path $dist 'logs') -Force | Out-Null

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

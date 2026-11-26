# Builds the portable release folder, its zip, and its SHA256 checksum.
#
# The deliverable is a folder that runs where it is unzipped: no installer, no
# registry keys, no system-wide runtimes. Everything the program needs travels
# with it. The UI is the native CGO-free MyGo application; only the sidecar
# ships FFmpeg's shared libraries.
#
# The default version comes from the repository's VERSION file. An explicit
# -Version is allowed for CI and for uniquely named test builds.
#
# Usage:  .\package.ps1 [-Version 0.4.0] [-SkipBuild]
#
# Every build runs inside a unique dist\.staging folder, so no shared
# backend\*.exe or backend\*.dll is overwritten. Go's content-addressed build
# cache already reuses unchanged work; -SkipBuild is accepted for compatibility
# with older command lines but no longer skips the (cached) builds.
#
# The zip is written from the clean staging tree. An existing unpacked install
# is moved aside (never deleted when it holds user data), the clean stage is
# renamed into place, and its config, logs, screenshots, and unknown files
# (including additions inside backend\) are carried over.

param(
    [string]$Version = "",
    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$versionFile = Join-Path $root 'VERSION'
if ([string]::IsNullOrWhiteSpace($Version)) {
    if (-not (Test-Path -LiteralPath $versionFile -PathType Leaf)) { throw 'VERSION file missing' }
    $Version = (Get-Content -LiteralPath $versionFile -Raw).Trim()
}
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$') { throw "invalid release version: $Version" }

$appName = '鏡屏'
$distRoot = Join-Path $root 'dist'
$finalDir = Join-Path $distRoot "$appName-$Version"
$zip = Join-Path $distRoot "MirrPane-$Version-win64.zip"
$shaFile = "$zip.sha256"
$stagingParent = Join-Path $distRoot '.staging'
$stage = Join-Path $stagingParent ("{0}-{1}-{2}" -f $appName, $Version, [guid]::NewGuid().ToString('N'))

$uiExe = Join-Path $stage 'MirrPane.exe'
$stageBackend = Join-Path $stage 'backend'
$sidecarExe = Join-Path $stageBackend 'airplayd.exe'
$stageCopying = Join-Path $stageBackend 'COPYING'
$ffmpegDlls = 'avutil-61.dll', 'swresample-7.dll', 'swscale-10.dll', 'avcodec-63.dll'

Write-Host "packaging $appName $Version"
if ($SkipBuild) {
    Write-Warning '-SkipBuild is accepted for compatibility; builds still run in the clean staging folder through Go''s content-addressed cache.'
}

function Get-RunningUnder([string]$Directory) {
    # Matches processes whose executable lives anywhere below $Directory, so a
    # running UI or its airplayd child both protect the folder they use.
    $prefix = [IO.Path]::GetFullPath($Directory).TrimEnd('\', '/') + '\'
    $cimPaths = @{}
    try {
        foreach ($entry in @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue)) {
            if ($entry.ExecutablePath) { $cimPaths[[int]$entry.ProcessId] = $entry.ExecutablePath }
        }
    } catch { }
    $hits = @()
    foreach ($process in @(Get-Process -ErrorAction SilentlyContinue)) {
        $path = $null
        try { $path = $process.Path } catch { }
        if (-not $path) { $path = $cimPaths[[int]$process.Id] }
        if (-not $path) { continue }
        try { $resolved = [IO.Path]::GetFullPath($path) } catch { continue }
        if ($resolved.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { $hits += $process }
    }
    return $hits
}

function Assert-NoRunningInstall([string]$Directory) {
    if (-not (Test-Path -LiteralPath $Directory)) { return }
    $running = @(Get-RunningUnder $Directory)
    if ($running.Count -gt 0) {
        $names = ($running | ForEach-Object { "$($_.ProcessName) ($($_.Id))" }) -join ', '
        throw "refusing to replace $Directory while it is in use by $names; stop the app or package a different -Version"
    }
}

function Copy-UserContent([string]$Source, [string]$Destination) {
    # Carries every user-owned item from an existing install into the freshly
    # staged tree. Release files are replaced; everything else is preserved.
    $preserved = $false
    $knownTop = @('MirrPane.exe', 'backend', 'config', 'logs', 'screenshots', 'LICENSE', 'COPYING', 'README.txt', 'SOURCE.txt', 'THIRD_PARTY_LICENSES.txt')
    $knownBackend = @('airplayd.exe', 'COPYING') + $ffmpegDlls

    foreach ($name in @('config', 'logs', 'screenshots')) {
        $sourceItem = Join-Path $Source $name
        if (-not (Test-Path -LiteralPath $sourceItem)) { continue }
        if (@(Get-ChildItem -LiteralPath $sourceItem -Force).Count -eq 0) { continue }
        $destinationItem = Join-Path $Destination $name
        New-Item -ItemType Directory -Path $destinationItem -Force | Out-Null
        if ($name -eq 'config') {
            # Pairing files must inherit the original private directory ACL as
            # soon as they exist, including before the next receiver launch.
            Set-Acl -LiteralPath $destinationItem -AclObject (Get-Acl -LiteralPath $sourceItem)
        }
        foreach ($child in @(Get-ChildItem -LiteralPath $sourceItem -Force)) {
            Copy-Item -LiteralPath $child.FullName -Destination $destinationItem -Recurse -Force
        }
        $preserved = $true
    }

    foreach ($item in @(Get-ChildItem -LiteralPath $Source -Force)) {
        if ($knownTop -contains $item.Name) { continue }
        Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $Destination $item.Name) -Recurse -Force
        $preserved = $true
    }

    $sourceBackend = Join-Path $Source 'backend'
    if (Test-Path -LiteralPath $sourceBackend) {
        foreach ($item in @(Get-ChildItem -LiteralPath $sourceBackend -Force)) {
            if ($knownBackend -contains $item.Name) { continue }
            Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $Destination 'backend' $item.Name) -Recurse -Force
            $preserved = $true
        }
    }
    return $preserved
}

Assert-NoRunningInstall $finalDir

$published = $false
$tempZip = $null
try {
    # --- build into the clean stage ------------------------------------------

    if (-not (Test-Path -LiteralPath $stagingParent)) { New-Item -ItemType Directory -Path $stagingParent -Force | Out-Null }
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    New-Item -ItemType Directory -Path $stageBackend -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $stage 'logs') -Force | Out-Null

    Push-Location (Join-Path $root 'backend')
    try {
        $previousCGO = $env:CGO_ENABLED
        $previousGOFLAGS = $env:GOFLAGS
        $env:CGO_ENABLED = '0'
        # Keep the shared checkout's go.mod/go.sum untouched while packaging.
        $env:GOFLAGS = '-mod=readonly'
        try {
            # One value drives the UI's version footer, the exe stamp, and the
            # zip name.
            go build -trimpath -ldflags "-s -w -H windowsgui -X main.appVersion=$Version" -o $uiExe ./cmd/mirrpane
            if ($LASTEXITCODE -ne 0) { throw 'MirrPane UI build failed' }

            # The sidecar and the FFmpeg libraries it loads from its own folder.
            & (Join-Path $root 'backend\build.ps1') -Output $sidecarExe
            if ($LASTEXITCODE -ne 0) { throw 'sidecar build failed' }
        }
        finally {
            $env:CGO_ENABLED = $previousCGO
            $env:GOFLAGS = $previousGOFLAGS
        }
    }
    finally { Pop-Location }

    $required = @($uiExe, $sidecarExe, $stageCopying)
    foreach ($dll in $ffmpegDlls) { $required += Join-Path $stageBackend $dll }
    foreach ($file in $required) {
        if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "missing build output $file" }
    }

    # Ship both GPL texts and exact upstream/source/build pointers with the DLLs.
    Copy-Item -LiteralPath $stageCopying -Destination (Join-Path $stage 'COPYING') -Force
    Copy-Item -LiteralPath (Join-Path $root 'LICENSE') -Destination (Join-Path $stage 'LICENSE') -Force

    # Every Go module linked into the two executables, including transitive
    # dependencies and MyGo's bundled shader licence.
    & (Join-Path $root 'scripts\third-party-notices.ps1') -Output (Join-Path $stage 'THIRD_PARTY_LICENSES.txt') -Version $Version

    $runtimeManager = Get-Content -LiteralPath (Join-Path $root 'backend\internal\ffmpeg\runtime.go') -Raw
    if ($runtimeManager -notmatch '(?m)^\s*releaseTag\s*=\s*"([^"]+)"') { throw 'FFmpeg release pin missing' }
    $ffmpegTag = $Matches[1]
    if ($runtimeManager -notmatch '(?m)^\s*assetSHA256\s*=\s*"([0-9a-f]{64})"') { throw 'FFmpeg SHA256 pin missing' }
    $ffmpegDigest = $Matches[1]
    if ($runtimeManager -notmatch '(?m)^\s*assetName\s*=\s*"([^"]+)"') { throw 'FFmpeg assetName pin missing in runtime.go' }
    $ffmpegAsset = $Matches[1]
    if ($ffmpegAsset -cnotmatch '^ffmpeg-(N-\d+-g([0-9a-f]{7,40}))(?:-(\d{8}))?-win64-gpl-shared\.zip$') {
        throw "FFmpeg assetName has an unsupported format: $ffmpegAsset (expected ffmpeg-N-<build>-g<commit>[-YYYYMMDD]-win64-gpl-shared.zip)"
    }
    $ffmpegBuild = $Matches[1]
    $ffmpegCommit = $Matches[2]
    $ffmpegAssetDate = $Matches[3]
    # Current upstream filenames omit the date; runtime.go's dated ABI build ID
    # supplies it. Also validate a filename date if upstream ever includes one.
    if ($runtimeManager -notmatch '(?m)^\s*buildID\s*=\s*"([^"]+)"') { throw 'FFmpeg dated buildID missing in runtime.go' }
    $ffmpegBuildID = $Matches[1]
    if ($ffmpegBuildID -cnotmatch '^(.+)-(\d{8})$' -or $Matches[1] -cne $ffmpegBuild) {
        throw "FFmpeg buildID does not match assetName: $ffmpegBuildID versus $ffmpegAsset"
    }
    $ffmpegBuildDate = $Matches[2]
    if ($ffmpegAssetDate -and $ffmpegAssetDate -cne $ffmpegBuildDate) {
        throw "FFmpeg assetName date $ffmpegAssetDate does not match buildID date $ffmpegBuildDate"
    }
    if ($ffmpegTag -cnotmatch '^autobuild-(\d{4}-\d{2}-\d{2})-\d{2}-\d{2}$') { throw "FFmpeg releaseTag has an unsupported format: $ffmpegTag" }
    $ffmpegReleaseDate = $Matches[1]
    try {
        $ffmpegReleaseDate = [datetime]::ParseExact($ffmpegReleaseDate, 'yyyy-MM-dd', [Globalization.CultureInfo]::InvariantCulture).ToString('yyyyMMdd', [Globalization.CultureInfo]::InvariantCulture)
    } catch {
        throw "FFmpeg releaseTag contains an invalid date: $ffmpegTag"
    }
    if ($ffmpegReleaseDate -cne $ffmpegBuildDate) {
        throw "FFmpeg releaseTag date $ffmpegReleaseDate does not match buildID date $ffmpegBuildDate"
    }
    $sourceNotice = @"
MirrPane ${Version}: GNU GPL version 3; see LICENSE.
Application source: https://github.com/Miku0139oao/MirrPane
Go modules compiled into MirrPane.exe and airplayd.exe: see THIRD_PARTY_LICENSES.txt.
FFmpeg shared runtime: $ffmpegBuildID; GNU GPL version 3; see COPYING.
Pinned binary asset: $ffmpegAsset
Pinned binary/build release: https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/tag/$ffmpegTag
Pinned binary SHA256: $ffmpegDigest
FFmpeg corresponding source: https://github.com/FFmpeg/FFmpeg/tree/$ffmpegCommit
Build scripts, dependency versions, patches and configuration:
https://github.com/FFmpeg-Builds/FFmpeg-Builds/tree/$ffmpegTag
Obtain all enabled dependencies using the matching build scripts, not just FFmpeg's source tree.
Release distributors must provide the complete corresponding source for their conveyed build.
"@
    Set-Content -LiteralPath (Join-Path $stage 'SOURCE.txt') -Value $sourceNotice -Encoding utf8

    # --- readme --------------------------------------------------------------

    $readme = @'
鏡屏 (MirrPane) — AirPlay 螢幕接收器

用法
  1. 解壓到任何資料夾（不要放在需要系統管理員權限的位置）。
  2. 執行 MirrPane.exe。
  3. 在 iPhone 或 iPad 開啟「控制中心」→「螢幕鏡像輸出」，選擇「鏡屏」。

第一次啟動時 Windows 會詢問防火牆權限，請選擇「允許存取」。
沒有允許的話，裝置看得到這台電腦，卻無法連上。

程式不會寫入登錄檔或安裝系統元件，設定、配對金鑰與日誌都在這個資料夾內：
  config\   裝置設定與配對金鑰（刪除後會重新產生一組新身分）
  logs\     執行記錄，發生問題時請提供 airplayd.log
介面框架 MyGo 會在第一次啟動時於 %LOCALAPPDATA%\MyGo\ShaderCache
建立著色器快取；其餘使用者資料仍只寫入上述資料夾。

授權
  GPLv3。本程式包含 FFmpeg（GPL，見 COPYING）與 MyGo（MIT）。
  第三方元件授權見 THIRD_PARTY_LICENSES.txt，來源與建置資訊見 SOURCE.txt。
'@
    Set-Content -Path (Join-Path $stage 'README.txt') -Value $readme -Encoding utf8

    # --- zip -----------------------------------------------------------------

    $size = (Get-ChildItem $stage -Recurse -File | Measure-Object -Property Length -Sum).Sum / 1MB
    Write-Host ("staged {0:N0} MB in {1}" -f $size, $stage)

    # Write the archive from the clean staging tree, before any existing
    # install's user data is carried over into the unpacked folder.
    $tempZip = Join-Path $stagingParent ("MirrPane-$Version-win64-{0}.zip" -f [guid]::NewGuid().ToString('N'))
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $tempZip -CompressionLevel Optimal
    $hash = (Get-FileHash -LiteralPath $tempZip -Algorithm SHA256).Hash.ToLowerInvariant()
    $zipSize = (Get-Item -LiteralPath $tempZip).Length / 1MB
    if (-not (Test-Path -LiteralPath $distRoot)) { New-Item -ItemType Directory -Path $distRoot -Force | Out-Null }
    Move-Item -LiteralPath $tempZip -Destination $zip -Force
    "$hash  $(Split-Path -Leaf $zip)" | Set-Content -LiteralPath $shaFile -Encoding ascii

    # --- publish the unpacked, runnable folder -------------------------------

    # Re-check immediately before touching the folder; a launched copy must
    # never be replaced under the running process.
    Assert-NoRunningInstall $finalDir

    if (Test-Path -LiteralPath $finalDir) {
        $hasUserData = Copy-UserContent $finalDir $stage
        $backup = Join-Path $stagingParent ("previous-{0}-{1}-{2}" -f $Version, (Get-Date -Format 'yyyyMMddHHmmss'), [guid]::NewGuid().ToString('N'))
        Move-Item -LiteralPath $finalDir -Destination $backup
        try {
            Move-Item -LiteralPath $stage -Destination $finalDir
        }
        catch {
            # Never leave the install path absent because the swap failed.
            try {
                Move-Item -LiteralPath $backup -Destination $finalDir
            }
            catch {
                throw "could not move the staged build into $finalDir, and restoring the previous folder also failed. Previous install: $backup. $($_.Exception.Message)"
            }
            throw
        }
        if ($hasUserData) {
            Write-Warning "kept the previous installation, including its user files, at $backup"
        }
        else {
            Remove-Item -LiteralPath $backup -Recurse -Force
        }
    }
    else {
        Move-Item -LiteralPath $stage -Destination $finalDir
    }

    $published = $true
}
finally {
    # Dispose only this run's staging directory. A kept previous install lives
    # under the same parent and is intentionally left alone.
    if ($tempZip -and (Test-Path -LiteralPath $tempZip)) {
        Remove-Item -LiteralPath $tempZip -Force -ErrorAction SilentlyContinue
    }
    if (-not $published -and (Test-Path -LiteralPath $stage)) {
        Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
    }
    if ((Test-Path -LiteralPath $stagingParent) -and -not (Get-ChildItem -LiteralPath $stagingParent -Force)) {
        Remove-Item -LiteralPath $stagingParent -Force
    }
}

Write-Host ("wrote {0} ({1:N0} MB)" -f $zip, $zipSize)
Write-Host ("SHA256 {0}  {1}" -f $hash, (Split-Path -Leaf $zip))
Write-Host ("unpacked build ready at {0}" -f $finalDir)

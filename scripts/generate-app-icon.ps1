# Regenerate the two Windows assets from the README artwork after a design change.
# Only this authoring step needs the FFmpeg CLI. Normal Go builds link the
# checked-in .syso, including when cross-compiling the Windows application.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$source = Join-Path $root 'assets/logo.png'
$icon = Join-Path $root 'assets/mirrpane.ico'
$object = Join-Path $root 'backend/cmd/mirrpane/icon_windows_amd64.syso'
$run = [guid]::NewGuid().ToString('N')
$newIcon = "$icon.$run.tmp"
$newObject = "$object.$run.tmp"
$previousIcon = "$icon.$run.bak"

try {
    $sizes = @(16, 20, 24, 32, 40, 48, 64, 96, 128, 256)
    $options = @('-nostdin', '-hide_banner', '-loglevel', 'error', '-filter_threads', '1', '-i', $source)
    for ($i = 0; $i -lt $sizes.Count; $i++) {
        $filter = 'scale={0}:{0}:flags=lanczos,format=rgba' -f $sizes[$i]
        $options += @('-map', '0:v:0', "-filter:v:$i", $filter)
    }
    & ffmpeg @options -map_metadata -1 -c:v png -threads:v 1 -f ico $newIcon
    if ($LASTEXITCODE -ne 0) { throw 'Could not export the Windows icon sizes' }

    # With the ICO as the sole resource, rsrc gives its group ID 1. MyGo
    # loads that ID for the native window class and its taskbar icon.
    & go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -ico $newIcon -o $newObject
    if ($LASTEXITCODE -ne 0) { throw 'Could not build the Windows icon resource' }

    $hadIcon = [IO.File]::Exists($icon)
    if ($hadIcon) {
        [IO.File]::Replace($newIcon, $icon, $previousIcon)
    } else {
        [IO.File]::Move($newIcon, $icon)
    }
    try {
        [IO.File]::Move($newObject, $object, $true)
    } catch {
        if ($hadIcon) { [IO.File]::Move($previousIcon, $icon, $true) }
        else { [IO.File]::Delete($icon) }
        throw
    }
    [IO.File]::Delete($previousIcon)
} finally {
    [IO.File]::Delete($newIcon)
    [IO.File]::Delete($newObject)
}

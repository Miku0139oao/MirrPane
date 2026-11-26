# Builds the per-user Windows installer (MirrPane-<version>-setup.exe).
#
# The payload is the packaged portable folder produced by package.ps1, so run
# that first. Inno Setup 6's command-line compiler (ISCC.exe) is required; the
# release workflow installs it with Chocolatey, and a local machine can get it
# with:  winget install --id JRSoftware.InnoSetup -e
#
# Usage:  .\installer\build-installer.ps1 -Version <version> [-SourceDir <dir>] [-Iscc <path>]

param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$SourceDir = "",
    [string]$Iscc = ""
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$') { throw "invalid release version: $Version" }
if ([string]::IsNullOrWhiteSpace($SourceDir)) { $SourceDir = Join-Path $root "dist\鏡屏-$Version" }
$SourceDir = [IO.Path]::GetFullPath($SourceDir)
foreach ($required in 'MirrPane.exe', 'backend\airplayd.exe') {
    if (-not (Test-Path -LiteralPath (Join-Path $SourceDir $required) -PathType Leaf)) {
        throw "packaged build missing at ${SourceDir}: run package.ps1 -Version $Version first"
    }
}

if ([string]::IsNullOrWhiteSpace($Iscc)) {
    $candidates = @(
        (Join-Path $env:LOCALAPPDATA 'Programs\Inno Setup 6\ISCC.exe'),
        (Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6\ISCC.exe'),
        (Join-Path $env:ProgramFiles 'Inno Setup 6\ISCC.exe')
    )
    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate -PathType Leaf)) { $Iscc = $candidate; break }
    }
}
if ([string]::IsNullOrWhiteSpace($Iscc) -or -not (Test-Path -LiteralPath $Iscc -PathType Leaf)) {
    throw 'ISCC.exe (Inno Setup 6) not found; install it (winget install --id JRSoftware.InnoSetup -e) or pass -Iscc'
}

$outDir = Join-Path $root 'dist'
& $Iscc "/DAppVersion=$Version" "/DSourceDir=$SourceDir" "/O$outDir" (Join-Path $PSScriptRoot 'MirrPane.iss')
if ($LASTEXITCODE -ne 0) { throw "Inno Setup compiler failed with exit code $LASTEXITCODE" }

$setup = Join-Path $outDir "MirrPane-$Version-setup.exe"
if (-not (Test-Path -LiteralPath $setup -PathType Leaf)) { throw "installer missing at $setup" }
$hash = (Get-FileHash -LiteralPath $setup -Algorithm SHA256).Hash.ToLowerInvariant()
"$hash  MirrPane-$Version-setup.exe" | Set-Content -LiteralPath "$setup.sha256" -Encoding ascii
$size = (Get-Item -LiteralPath $setup).Length / 1MB
Write-Host ("wrote {0} ({1:N0} MB)" -f $setup, $size)
Write-Host ("SHA256 {0}  MirrPane-{1}-setup.exe" -f $hash, $Version)

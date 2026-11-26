# Collects the licences of every Go module linked into the shipped binary.
#
# package.ps1 calls this after building MirrPane.exe. The notice lists the
# exact module versions reported by `go list -deps` plus a licence file from
# the module root or from each compiled package directory, so a module that
# bundles separate terms (for example MyGo's shader code) is covered as well.
#
# Usage:  .\scripts\third-party-notices.ps1 -Output <path> [-Version <app version>]
param(
    [Parameter(Mandatory = $true)][string]$Output,
    [string]$Version = ""
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$backend = Join-Path $root 'backend'
$licensePattern = '^(?i)(licen[cs]e|copying|notice|patents)(\..*)?$'

function Get-LicenseFiles([string]$Directory) {
    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) { return @() }
    return @(Get-ChildItem -LiteralPath $Directory -File -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match $licensePattern } |
        Sort-Object Name |
        ForEach-Object { $_.FullName })
}

Push-Location $backend
try {
    $previousCGO = $env:CGO_ENABLED
    $previousGOFLAGS = $env:GOFLAGS
    $env:CGO_ENABLED = '0'
    $env:GOFLAGS = '-mod=readonly'
    try {
        $rows = @(& go list -deps -f '{{.ImportPath}}|{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}|{{.Main}}{{end}}|{{.Dir}}' ./cmd/mirrpane)
        if ($LASTEXITCODE -ne 0) { throw 'go list failed while collecting dependency licences' }
        # Resolve the toolchain from the module, not from whatever `go` the
        # caller's working directory happens to select.
        $goRoot = (& go env GOROOT).Trim()
        $goVersion = (& go version).Trim()
    }
    finally {
        $env:CGO_ENABLED = $previousCGO
        $env:GOFLAGS = $previousGOFLAGS
    }
}
finally {
    Pop-Location
}

# importPath | modulePath | moduleVersion | moduleDir | main | packageDir
$modules = @{}
foreach ($row in $rows) {
    $parts = $row -split '\|'
    if ($parts.Count -lt 6) { continue }
    $modulePath = $parts[1]
    $moduleVersion = $parts[2]
    $moduleDir = $parts[3]
    $isMain = $parts[4]
    $packageDir = $parts[5]
    if ([string]::IsNullOrWhiteSpace($modulePath) -or $isMain -eq 'true') { continue }

    $key = if ($moduleVersion) { "$modulePath $moduleVersion" } else { $modulePath }
    if (-not $modules.ContainsKey($key)) {
        $modules[$key] = [pscustomobject]@{
            Path  = $modulePath
            Version = $moduleVersion
            Dir   = $moduleDir
            Dirs  = New-Object System.Collections.Generic.HashSet[string]
        }
    }
    if ($packageDir) { [void]$modules[$key].Dirs.Add($packageDir) }
}

$lines = New-Object System.Collections.Generic.List[string]
$lines.Add('Third-party licence notices for MirrPane')
if ($Version) { $lines.Add("MirrPane version: $Version") }
$lines.Add('')
$lines.Add('MirrPane itself is licensed under GPL-3.0 (see LICENSE). The portable')
$lines.Add('archive also contains FFmpeg shared libraries under GPL-3.0 (see COPYING)')
$lines.Add('and the source/build pointers in SOURCE.txt.')
$lines.Add('')
$lines.Add('The Go modules below are linked into MirrPane.exe.')
$lines.Add('Each entry names the module version and reproduces the licence files')
$lines.Add('that ship with the module, including notices inside compiled packages.')
$lines.Add('')

foreach ($key in ($modules.Keys | Sort-Object { $modules[$_].Path })) {
    $module = $modules[$key]
    $dirs = New-Object System.Collections.Generic.List[string]
    if ($module.Dir) { $dirs.Add($module.Dir) }
    foreach ($dir in ($module.Dirs | Sort-Object)) { $dirs.Add($dir) }

    $licenseFiles = New-Object System.Collections.Generic.List[string]
    $seen = New-Object System.Collections.Generic.HashSet[string]
    foreach ($dir in $dirs) {
        foreach ($file in (Get-LicenseFiles $dir)) {
            if ($seen.Add($file)) { $licenseFiles.Add($file) }
        }
    }
    if ($licenseFiles.Count -eq 0) {
        Write-Warning "no licence file found for $key"
        continue
    }

    $title = if ($module.Version) { "$($module.Path) $($module.Version)" } else { $module.Path }
    $lines.Add(('=' * 78))
    $lines.Add($title)
    $lines.Add(('=' * 78))
    $lines.Add('')
    foreach ($file in $licenseFiles) {
        $display = $file
        if ($module.Dir -and $file.StartsWith($module.Dir, [StringComparison]::OrdinalIgnoreCase)) {
            $display = $file.Substring($module.Dir.Length).TrimStart('\', '/')
        }
        $lines.Add("--- $display ---")
        $lines.Add('')
        $lines.Add((Get-Content -LiteralPath $file -Raw).TrimEnd())
        $lines.Add('')
    }
}

# The Go toolchain statically links its standard library, whose licence is not
# part of any module directory.
if ($goRoot) {
    $goFiles = @(Get-LicenseFiles $goRoot)
    if ($goFiles.Count -gt 0) {
        $lines.Add(('=' * 78))
        $lines.Add("Go standard library ($goVersion)")
        $lines.Add(('=' * 78))
        $lines.Add('')
        foreach ($file in $goFiles) {
            $lines.Add("--- $(Split-Path -Leaf $file) ---")
            $lines.Add('')
            $lines.Add((Get-Content -LiteralPath $file -Raw).TrimEnd())
            $lines.Add('')
        }
    }
}

$outputDir = Split-Path -Parent ([IO.Path]::GetFullPath($Output))
if ($outputDir -and -not (Test-Path -LiteralPath $outputDir)) {
    New-Item -ItemType Directory -Path $outputDir -Force | Out-Null
}
Set-Content -LiteralPath $Output -Value ($lines -join "`r`n") -Encoding utf8
Write-Host "wrote third-party notices to $Output"

[CmdletBinding()]
param(
    [ValidatePattern('^\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$')]
    [string]$Version = '4.0.1',
    [string]$GoExe = 'go'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    throw 'Run this release build on Windows: the installer tests execute a Windows program.'
}

$repoRoot = [IO.Path]::GetFullPath((Split-Path -Parent $PSScriptRoot))
$goCommand = Get-Command -Name $GoExe -ErrorAction Stop
$GoExe = $goCommand.Source
$goVersion = & $GoExe version
if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch '\bgo(\d+\.\d+(?:\.\d+)?)\b') {
    throw 'Could not determine the installed Go version.'
}
if ([version]$Matches[1] -lt [version]'1.27.1') {
    throw 'Go 1.27.1 or newer is required.'
}

function Invoke-Go {
    param([Parameter(Mandatory = $true)][string[]]$GoArguments)
    & $GoExe @GoArguments
    if ($LASTEXITCODE -ne 0) {
        throw ('Go command failed: go ' + ($GoArguments -join ' '))
    }
}

$releaseName = 'mihomo-mobile-panel-' + $Version
$distRoot = Join-Path $repoRoot 'dist'
$releaseRoot = Join-Path $distRoot $releaseName
$payloadRoot = Join-Path $repoRoot 'installer/payload'
$payload = Join-Path $payloadRoot 'mihomo-mobile'
$installer = Join-Path $releaseRoot 'mihomo-mobile-panel-installer.exe'
$requiredDocuments = @('README.md', 'LICENSE', 'SECURITY.md', 'THIRD-PARTY-NOTICES.txt')
$optionalDocuments = @('PRIVACY.md', 'CHANGELOG.md')
$documents = @($requiredDocuments)
foreach ($name in $requiredDocuments) {
    if (-not (Test-Path -LiteralPath (Join-Path $repoRoot $name) -PathType Leaf)) {
        throw ('Required release document is missing: ' + $name)
    }
}
foreach ($name in $optionalDocuments) {
    if (Test-Path -LiteralPath (Join-Path $repoRoot $name) -PathType Leaf) {
        $documents += $name
    }
}

$launchers = @(Get-ChildItem -LiteralPath $PSScriptRoot -File | Where-Object { $_.Extension -in @('.bat', '.url') })
$licenseRoot = Join-Path $repoRoot 'third_party/licenses'
if (-not (Test-Path -LiteralPath $licenseRoot -PathType Container)) {
    throw 'Full third-party licenses are required for distribution.'
}
$licenseNames = @(Get-ChildItem -LiteralPath $licenseRoot -File | Where-Object { $_.Extension -eq '.txt' } | ForEach-Object { 'third_party/licenses/' + $_.Name })
$allowedNames = @($documents) + @($launchers.Name) + @(
    'mihomo-mobile', 'mihomo-mobile-panel-installer.exe',
    'common-rules.yaml', 'liquid-glass-license.txt', 'SHA256SUMS.txt'
) + $licenseNames

# Dist is generated output. Refuse to mix unrelated files into an existing
# release directory, and archive only the explicit package list below.
if (Test-Path -LiteralPath $releaseRoot) {
    foreach ($file in Get-ChildItem -LiteralPath $releaseRoot -Recurse -File) {
        $relative = $file.FullName.Substring($releaseRoot.Length + 1).Replace('\', '/')
        if ($relative -notin $allowedNames) {
            throw ('Release directory contains an unrecognized file; move it out before building: ' + $relative)
        }
    }
}
New-Item -ItemType Directory -Force -Path $releaseRoot, $payloadRoot | Out-Null

$oldEnvironment = @{}
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
    $oldEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    Push-Location (Join-Path $repoRoot 'mobile')
    try {
        Write-Host 'Testing and checking the panel on Windows...'
        Invoke-Go -GoArguments @('test', '-trimpath', '-buildvcs=false', './...')
        Invoke-Go -GoArguments @('vet', './...')
        $env:GOOS = 'linux'
        $env:GOARCH = 'arm64'
        Write-Host 'Building the Linux ARM64 router service...'
        Invoke-Go -GoArguments @('build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w', '-o', $payload, '.')
    } finally {
        Pop-Location
    }

    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    Push-Location (Join-Path $repoRoot 'installer')
    try {
        Write-Host 'Embedding the source manifest (asInvoker, no elevation)...'
        Invoke-Go -GoArguments @('run', 'github.com/akavel/rsrc@v0.10.2', '-manifest', 'app.manifest', '-o', 'manifest_windows_amd64.syso', '-arch', 'amd64')
        Write-Host 'Testing and checking the Windows installer...'
        Invoke-Go -GoArguments @('test', '-trimpath', '-buildvcs=false', './...')
        Invoke-Go -GoArguments @('vet', './...')
        Invoke-Go -GoArguments @('build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w', '-o', $installer, '.')
    } finally {
        Pop-Location
    }

    $payloadHash = (Get-FileHash -LiteralPath $payload -Algorithm SHA256).Hash.ToLowerInvariant()
    $checkStart = New-Object Diagnostics.ProcessStartInfo
    $checkStart.FileName = $installer
    $checkStart.Arguments = '--check-only'
    $checkStart.UseShellExecute = $false
    $checkStart.CreateNoWindow = $true
    $checkStart.RedirectStandardOutput = $true
    $checkStart.RedirectStandardError = $true
    $checkStart.StandardOutputEncoding = [Text.Encoding]::UTF8
    $checkProcess = New-Object Diagnostics.Process
    $checkProcess.StartInfo = $checkStart
    try {
        if (-not $checkProcess.Start()) { throw 'Could not start the installer payload check.' }
        $checkOutput = $checkProcess.StandardOutput.ReadToEnd()
        $checkError = $checkProcess.StandardError.ReadToEnd()
        $checkProcess.WaitForExit()
        if ($checkProcess.ExitCode -ne 0 -or $checkOutput -notmatch [regex]::Escape($payloadHash)) {
            throw ('The installer does not embed this router service. ' + $checkError)
        }
    } finally {
        $checkProcess.Dispose()
    }
    Write-Host $checkOutput

    Copy-Item -LiteralPath $payload -Destination (Join-Path $releaseRoot 'mihomo-mobile') -Force
    foreach ($name in $documents) {
        Copy-Item -LiteralPath (Join-Path $repoRoot $name) -Destination (Join-Path $releaseRoot $name) -Force
    }
    foreach ($launcher in $launchers) {
        Copy-Item -LiteralPath $launcher.FullName -Destination (Join-Path $releaseRoot $launcher.Name) -Force
    }
    Copy-Item -LiteralPath (Join-Path $repoRoot 'mobile/common-rules.yaml') -Destination (Join-Path $releaseRoot 'common-rules.yaml') -Force
    Copy-Item -LiteralPath (Join-Path $repoRoot 'mobile/web/liquid-glass-license.txt') -Destination (Join-Path $releaseRoot 'liquid-glass-license.txt') -Force
    New-Item -ItemType Directory -Force -Path (Join-Path $releaseRoot 'third_party/licenses') | Out-Null
    foreach ($name in $licenseNames) {
        Copy-Item -LiteralPath (Join-Path $repoRoot $name) -Destination (Join-Path $releaseRoot $name) -Force
    }

    $packageNames = @($allowedNames | Where-Object { $_ -ne 'SHA256SUMS.txt' } | Sort-Object -Unique)
    $hashLines = foreach ($name in $packageNames) {
        $fileHash = (Get-FileHash -LiteralPath (Join-Path $releaseRoot $name) -Algorithm SHA256).Hash.ToLowerInvariant()
        $fileHash + '  ' + $name
    }
    $utf8 = New-Object Text.UTF8Encoding($false)
    [IO.File]::WriteAllText((Join-Path $releaseRoot 'SHA256SUMS.txt'), (($hashLines -join "`n") + "`n"), $utf8)
    $packageNames += 'SHA256SUMS.txt'

    Add-Type -AssemblyName System.IO.Compression
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zipPath = Join-Path $distRoot ($releaseName + '.zip')
    $stream = [IO.File]::Open($zipPath, [IO.FileMode]::Create, [IO.FileAccess]::ReadWrite)
    $archive = New-Object IO.Compression.ZipArchive($stream, [IO.Compression.ZipArchiveMode]::Create, $false)
    try {
        foreach ($name in $packageNames) {
            $entry = $releaseName + '/' + $name
            [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, (Join-Path $releaseRoot $name), $entry, [IO.Compression.CompressionLevel]::Optimal) | Out-Null
        }
    } finally {
        $archive.Dispose()
        $stream.Dispose()
    }
    $zipHash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText(($zipPath + '.sha256'), ($zipHash + '  ' + [IO.Path]::GetFileName($zipPath) + "`n"), $utf8)
    Write-Host ('Built: ' + $zipPath)
    Write-Host ('SHA256: ' + $zipHash)
} finally {
    foreach ($name in $oldEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $oldEnvironment[$name], 'Process')
    }
}

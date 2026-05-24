# Lin Simulator release build script.
# Run from anywhere; outputs release binaries, simulator.db, and protocol.zip into ./dist by default.

[CmdletBinding()]
param(
    [string]$DistDir = "dist",
    [switch]$Clean
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$BinaryName = "lin_simulator"
$MainPackage = "."
$DatabaseFile = "simulator.db"
$ProtocolDir = "protocol"
$ProtocolArchive = "protocol.zip"
$ProjectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path

if ([System.IO.Path]::IsPathRooted($DistDir)) {
    $TargetDir = [System.IO.Path]::GetFullPath($DistDir)
} else {
    $TargetDir = [System.IO.Path]::GetFullPath((Join-Path $ProjectRoot $DistDir))
}

$Platforms = @(
    [pscustomobject]@{ Name = "windows-amd64"; GOOS = "windows"; GOARCH = "amd64"; GOARM = ""; Output = "$($BinaryName)_windows_amd64.exe" },
    [pscustomobject]@{ Name = "windows-arm64"; GOOS = "windows"; GOARCH = "arm64"; GOARM = ""; Output = "$($BinaryName)_windows_arm64.exe" },
    [pscustomobject]@{ Name = "linux-amd64"; GOOS = "linux"; GOARCH = "amd64"; GOARM = ""; Output = "$($BinaryName)_linux_amd64" },
    [pscustomobject]@{ Name = "linux-arm64"; GOOS = "linux"; GOARCH = "arm64"; GOARM = ""; Output = "$($BinaryName)_linux_arm64" },
    [pscustomobject]@{ Name = "linux-armv6"; GOOS = "linux"; GOARCH = "arm"; GOARM = "6"; Output = "$($BinaryName)_linux_armv6" },
    [pscustomobject]@{ Name = "linux-armv7l"; GOOS = "linux"; GOARCH = "arm"; GOARM = "7"; Output = "$($BinaryName)_linux_armv7l" },
    [pscustomobject]@{ Name = "darwin-amd64"; GOOS = "darwin"; GOARCH = "amd64"; GOARM = ""; Output = "$($BinaryName)_darwin_amd64" },
    [pscustomobject]@{ Name = "darwin-arm64"; GOOS = "darwin"; GOARCH = "arm64"; GOARM = ""; Output = "$($BinaryName)_darwin_arm64" }
)

function Set-GoEnv {
    param(
        [string]$GOOS,
        [string]$GOARCH,
        [string]$GOARM
    )

    $env:CGO_ENABLED = "0"
    $env:GOOS = $GOOS
    $env:GOARCH = $GOARCH

    if ([string]::IsNullOrWhiteSpace($GOARM)) {
        [Environment]::SetEnvironmentVariable("GOARM", $null, "Process")
    } else {
        $env:GOARM = $GOARM
    }
}

function Restore-GoEnv {
    param([hashtable]$OriginalEnv)

    foreach ($name in $OriginalEnv.Keys) {
        [Environment]::SetEnvironmentVariable($name, $OriginalEnv[$name], "Process")
    }
}

function New-Directory {
    param([string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -ItemType Directory -Path $Path -Force | Out-Null
    }
}

function Clear-ReleaseOutput {
    foreach ($platform in $Platforms) {
        $output = Join-Path $TargetDir $platform.Output
        Remove-Item -LiteralPath $output -Force -ErrorAction SilentlyContinue
    }

    Remove-Item -LiteralPath (Join-Path $TargetDir $DatabaseFile) -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath (Join-Path $TargetDir $ProtocolArchive) -Force -ErrorAction SilentlyContinue
}

function Invoke-ReleaseBuild {
    param([pscustomobject]$Platform)

    Set-GoEnv -GOOS $Platform.GOOS -GOARCH $Platform.GOARCH -GOARM $Platform.GOARM

    $output = Join-Path $TargetDir $Platform.Output
    $goarmText = if ([string]::IsNullOrWhiteSpace($Platform.GOARM)) { "" } else { " GOARM=$($Platform.GOARM)" }

    Write-Host "[$($Platform.Name)] CGO_ENABLED=0 GOOS=$($Platform.GOOS) GOARCH=$($Platform.GOARCH)$goarmText" -ForegroundColor Cyan
    & go build -o $output $MainPackage

    if ($LASTEXITCODE -ne 0) {
        throw "Build failed: $($Platform.Name)"
    }

    Write-Host "  Output: $output" -ForegroundColor Green
}

function Copy-ReleaseDatabase {
    $source = Join-Path $ProjectRoot $DatabaseFile
    if (-not (Test-Path -LiteralPath $source)) {
        throw "Missing database file: $source"
    }

    $destination = Join-Path $TargetDir $DatabaseFile
    Copy-Item -LiteralPath $source -Destination $destination -Force
    Write-Host "  Output: $destination" -ForegroundColor Green
}

function Compress-ReleaseProtocol {
    $sourceDir = Join-Path $ProjectRoot $ProtocolDir
    if (-not (Test-Path -LiteralPath $sourceDir -PathType Container)) {
        throw "Missing protocol directory: $sourceDir"
    }

    $items = @(Get-ChildItem -LiteralPath $sourceDir -Force)
    if ($items.Count -eq 0) {
        throw "Protocol directory is empty: $sourceDir"
    }

    $destination = Join-Path $TargetDir $ProtocolArchive
    Remove-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue

    Compress-Archive -LiteralPath $items.FullName -DestinationPath $destination -Force
    Write-Host "  Output: $destination" -ForegroundColor Green
}

function Show-ReleaseOutputs {
    Write-Host ""
    Write-Host "Release output complete:" -ForegroundColor Green

    $outputs = foreach ($platform in $Platforms) {
        $path = Join-Path $TargetDir $platform.Output
        if (Test-Path -LiteralPath $path) {
            $item = Get-Item -LiteralPath $path
            [pscustomobject]@{
                Platform = $platform.Name
                Name = $item.Name
                SizeMB = [math]::Round($item.Length / 1MB, 2)
            }
        }
    }

    $databasePath = Join-Path $TargetDir $DatabaseFile
    if (Test-Path -LiteralPath $databasePath) {
        $item = Get-Item -LiteralPath $databasePath
        $outputs += [pscustomobject]@{
            Platform = "data"
            Name = $item.Name
            SizeMB = [math]::Round($item.Length / 1MB, 2)
        }
    }

    $protocolPath = Join-Path $TargetDir $ProtocolArchive
    if (Test-Path -LiteralPath $protocolPath) {
        $item = Get-Item -LiteralPath $protocolPath
        $outputs += [pscustomobject]@{
            Platform = "protocol"
            Name = $item.Name
            SizeMB = [math]::Round($item.Length / 1MB, 2)
        }
    }

    $outputs | Format-Table Platform, Name, SizeMB -AutoSize
    Write-Host "Directory: $TargetDir" -ForegroundColor Green
}

$OriginalEnv = @{
    CGO_ENABLED = [Environment]::GetEnvironmentVariable("CGO_ENABLED", "Process")
    GOOS = [Environment]::GetEnvironmentVariable("GOOS", "Process")
    GOARCH = [Environment]::GetEnvironmentVariable("GOARCH", "Process")
    GOARM = [Environment]::GetEnvironmentVariable("GOARM", "Process")
}

try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "go command not found"
    }

    Push-Location $ProjectRoot
    try {
        if (-not (Test-Path -LiteralPath "go.mod")) {
            throw "Current directory is not a Go module root"
        }

        New-Directory $TargetDir

        if ($Clean) {
            Write-Host "Cleaning release outputs..." -ForegroundColor Yellow
            Clear-ReleaseOutput
        }

        Write-Host "Building release binaries..." -ForegroundColor Cyan
        Write-Host "Output directory: $TargetDir" -ForegroundColor Cyan

        foreach ($platform in $Platforms) {
            Invoke-ReleaseBuild -Platform $platform
        }

        Copy-ReleaseDatabase
        Compress-ReleaseProtocol
        Show-ReleaseOutputs
    } finally {
        Pop-Location
    }
} catch {
    Write-Host $_.Exception.Message -ForegroundColor Red
    exit 1
} finally {
    Restore-GoEnv -OriginalEnv $OriginalEnv
}

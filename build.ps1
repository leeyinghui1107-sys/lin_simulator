# Lin Simulator release build script for PowerShell.
# Cross-compiles the simulator binary and copies simulator.db to the publish dist directory.

param(
    [Parameter(Position = 0)]
    [ValidateSet("all", "windows", "linux", "darwin", "clean-dist", "help")]
    [string]$Action = "all",

    [string]$DistDir = "C:\source\01_project\linlin_pub\dist"
)

$ErrorActionPreference = "Stop"

$BinaryName = "lin_simulator"
$MainPackage = "."
$RepoRoot = $PSScriptRoot
$DatabasePath = Join-Path $RepoRoot "simulator.db"

$WindowsPlatforms = @(
    @{ Os = "windows"; Arch = "amd64"; OutputName = "${BinaryName}_windows_amd64.exe" },
    @{ Os = "windows"; Arch = "arm64"; OutputName = "${BinaryName}_windows_arm64.exe" }
)

$LinuxPlatforms = @(
    @{ Os = "linux"; Arch = "amd64"; OutputName = "${BinaryName}_linux_amd64" },
    @{ Os = "linux"; Arch = "arm64"; OutputName = "${BinaryName}_linux_arm64" },
    @{ Os = "linux"; Arch = "arm"; Arm = "7"; OutputName = "${BinaryName}_linux_armv7l" },
    @{ Os = "linux"; Arch = "arm"; Arm = "6"; OutputName = "${BinaryName}_linux_armv6" }
)

$DarwinPlatforms = @(
    @{ Os = "darwin"; Arch = "amd64"; OutputName = "${BinaryName}_darwin_amd64" },
    @{ Os = "darwin"; Arch = "arm64"; OutputName = "${BinaryName}_darwin_arm64" }
)

function New-Directory {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -ItemType Directory -Path $Path -Force | Out-Null
    }
}

function Invoke-GoBuild {
    param([hashtable]$Platform)

    $target = "$($Platform.Os)/$($Platform.Arch)"
    if ($Platform.ContainsKey("Arm")) {
        $target = "$target GOARM=$($Platform.Arm)"
    }

    $output = Join-Path $DistDir $Platform.OutputName
    Write-Host "Building $target -> $output" -ForegroundColor Yellow

    $env:CGO_ENABLED = "0"
    $env:GOOS = $Platform.Os
    $env:GOARCH = $Platform.Arch
    if ($Platform.ContainsKey("Arm")) {
        $env:GOARM = $Platform.Arm
    } else {
        Remove-Item Env:GOARM -ErrorAction SilentlyContinue
    }

    go build -trimpath -o $output $MainPackage
    if ($LASTEXITCODE -ne 0) {
        throw "Build failed for $target"
    }
}

function Copy-Database {
    if (-not (Test-Path -LiteralPath $DatabasePath)) {
        throw "simulator.db not found: $DatabasePath"
    }

    $target = Join-Path $DistDir "simulator.db"
    Copy-Item -LiteralPath $DatabasePath -Destination $target -Force
    Write-Host "Copied simulator.db -> $target" -ForegroundColor Green
}

function Clear-OwnOutputs {
    param([array]$Platforms)

    $names = @($Platforms | ForEach-Object { $_.OutputName })
    $names += @("${BinaryName}.exe", $BinaryName)
    foreach ($name in ($names | Select-Object -Unique)) {
        $path = Join-Path $DistDir $name
        if (Test-Path -LiteralPath $path) {
            Remove-Item -LiteralPath $path -Force
        }
    }
}

function Build-Platforms {
    param([array]$Platforms)

    New-Directory $DistDir
    Clear-OwnOutputs -Platforms $Platforms

    $oldCGO = $env:CGO_ENABLED
    $oldGOOS = $env:GOOS
    $oldGOARCH = $env:GOARCH
    $oldGOARM = $env:GOARM

    Push-Location $RepoRoot
    try {
        foreach ($platform in $Platforms) {
            Invoke-GoBuild -Platform $platform
        }
        Copy-Database
    }
    finally {
        Pop-Location

        if ($null -eq $oldCGO) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $oldCGO }
        if ($null -eq $oldGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $oldGOOS }
        if ($null -eq $oldGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $oldGOARCH }
        if ($null -eq $oldGOARM) { Remove-Item Env:GOARM -ErrorAction SilentlyContinue } else { $env:GOARM = $oldGOARM }
    }

    Write-Host ""
    Write-Host "Release files:" -ForegroundColor Green
    Get-ChildItem -LiteralPath $DistDir | Sort-Object Name | Format-Table Name, Length -AutoSize
}

function Build-All {
    Write-Host "Cross-compiling $BinaryName for all platforms..." -ForegroundColor Cyan
    Build-Platforms -Platforms ($WindowsPlatforms + $LinuxPlatforms + $DarwinPlatforms)
}

function Build-Windows {
    Write-Host "Building Windows platforms..." -ForegroundColor Cyan
    Build-Platforms -Platforms $WindowsPlatforms
}

function Build-Linux {
    Write-Host "Building Linux platforms..." -ForegroundColor Cyan
    Build-Platforms -Platforms $LinuxPlatforms
}

function Build-Darwin {
    Write-Host "Building macOS platforms..." -ForegroundColor Cyan
    Build-Platforms -Platforms $DarwinPlatforms
}

function Clear-Dist {
    if (Test-Path -LiteralPath $DistDir) {
        Remove-Item -LiteralPath $DistDir -Recurse -Force
        Write-Host "Clean complete: $DistDir" -ForegroundColor Green
    } else {
        Write-Host "Nothing to clean: $DistDir" -ForegroundColor Yellow
    }
}

function Show-Help {
    Write-Host "Lin Simulator Build Script"
    Write-Host "=========================="
    Write-Host ""
    Write-Host "Usage: .\build.ps1 [action] [-DistDir path]"
    Write-Host ""
    Write-Host "Actions:"
    Write-Host "  all        - Build all 8 platforms and copy simulator.db (default)"
    Write-Host "  windows    - Build windows/amd64 and windows/arm64"
    Write-Host "  linux      - Build linux/amd64, linux/arm64, linux/armv7l, linux/armv6"
    Write-Host "  darwin     - Build darwin/amd64 and darwin/arm64"
    Write-Host "  clean-dist - Remove the dist directory"
    Write-Host "  help       - Show this help"
    Write-Host ""
    Write-Host "Default dist: $DistDir"
}

switch ($Action) {
    "all"        { Build-All }
    "windows"    { Build-Windows }
    "linux"      { Build-Linux }
    "darwin"     { Build-Darwin }
    "clean-dist" { Clear-Dist }
    "help"       { Show-Help }
}

param(
    [switch]$SkipVulnCheck
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$buildDir = Join-Path ([System.IO.Path]::GetTempPath()) "lin-simulator-check"
New-Item -ItemType Directory -Force -Path $buildDir | Out-Null

$modules = @(
    @{ Name = "lin_simulator"; Path = $repoRoot; BuildOut = Join-Path $buildDir "lin_simulator.exe" }
)

foreach ($module in $modules) {
    Write-Host "== $($module.Name): test =="
    Push-Location $module.Path
    try {
        go test ./...

        Write-Host "== $($module.Name): vet =="
        go vet ./...

        Write-Host "== $($module.Name): build =="
        go build -o $module.BuildOut .

        if (-not $SkipVulnCheck) {
            Write-Host "== $($module.Name): govulncheck =="
            go run golang.org/x/vuln/cmd/govulncheck@latest ./...
        }
    }
    finally {
        Pop-Location
    }
}

Remove-Item -LiteralPath $buildDir -Recurse -Force -ErrorAction SilentlyContinue

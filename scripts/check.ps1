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

function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Label,
        [Parameter(Mandatory = $true)]
        [scriptblock]$Command
    )

    & $Command
    if ($LASTEXITCODE -ne 0) {
        throw "$Label failed with exit code $LASTEXITCODE"
    }
}

try {
    foreach ($module in $modules) {
        Write-Host "== $($module.Name): test =="
        Push-Location $module.Path
        try {
            Invoke-Native "go test" { go test ./... }

            Write-Host "== $($module.Name): vet =="
            Invoke-Native "go vet" { go vet ./... }

            Write-Host "== $($module.Name): build =="
            Invoke-Native "go build" { go build -o $module.BuildOut . }

            if (-not $SkipVulnCheck) {
                Write-Host "== $($module.Name): govulncheck =="
                Invoke-Native "govulncheck" { go run golang.org/x/vuln/cmd/govulncheck@latest ./... }
            }
        }
        finally {
            Pop-Location
        }
    }
}
finally {
    Remove-Item -LiteralPath $buildDir -Recurse -Force -ErrorAction SilentlyContinue
}

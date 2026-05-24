# Lin Simulator release publish script.
# Builds dist/ and publishes its files to GitHub and Gitee releases.

[CmdletBinding()]
param(
    [ValidateSet("github", "gitee", "both")]
    [string]$Target = "both",

    [string]$Tag = "v0.1.0",
    [string]$ReleaseName = "",
    [string]$BodyFile = "docs/releases/v0.1.0.md",
    [string]$DistDir = "dist",

    [string]$GitHubRemote = "origin",
    [string]$GiteeRemote = "gitee",
    [string]$GitHubOwner = "",
    [string]$GitHubRepo = "",
    [string]$GiteeOwner = "",
    [string]$GiteeRepo = "",

    [string]$GitHubToken = $(if ($env:GITHUB_TOKEN) { $env:GITHUB_TOKEN } else { $env:GH_TOKEN }),
    [string]$GiteeToken = $env:GITEE_TOKEN,

    [string]$GitHubApiBase = "https://api.github.com",
    [string]$GitHubUploadBase = "https://uploads.github.com",
    [string]$GitHubApiVersion = "2022-11-28",
    [string]$GiteeApiBase = "https://gitee.com/api/v5",

    [switch]$NoBuild,
    [switch]$NoClean,
    [switch]$NoTagPush,
    [switch]$ForceTag,
    [switch]$AllowDirty,
    [switch]$Prerelease,
    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$ProjectRoot = (Resolve-Path $PSScriptRoot).Path
$ProjectRootFull = [System.IO.Path]::GetFullPath($ProjectRoot).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)

if ([System.IO.Path]::IsPathRooted($DistDir)) {
    $DistFull = [System.IO.Path]::GetFullPath($DistDir)
} else {
    $DistFull = [System.IO.Path]::GetFullPath((Join-Path $ProjectRootFull $DistDir))
}

$DistFull = $DistFull.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
$ProjectRootPrefix = $ProjectRootFull + [System.IO.Path]::DirectorySeparatorChar

if ($DistFull -eq $ProjectRootFull -or -not $DistFull.StartsWith($ProjectRootPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "DistDir must be inside project root and must not be project root: $DistFull"
}

if ([string]::IsNullOrWhiteSpace($ReleaseName)) {
    $ReleaseName = "lin_simulator $Tag"
}

function Test-ReleaseTarget {
    param([string]$Name)

    return $Target -eq "both" -or $Target -eq $Name
}

function ConvertTo-UrlEncoded {
    param([string]$Value)

    return [System.Uri]::EscapeDataString($Value)
}

function Add-QueryString {
    param(
        [string]$Uri,
        [hashtable]$Parameters
    )

    $pairs = foreach ($key in $Parameters.Keys) {
        "{0}={1}" -f (ConvertTo-UrlEncoded $key), (ConvertTo-UrlEncoded ([string]$Parameters[$key]))
    }

    if ($pairs.Count -eq 0) {
        return $Uri
    }

    $separator = if ($Uri.Contains("?")) { "&" } else { "?" }
    return "$Uri$separator$($pairs -join "&")"
}

function Invoke-Native {
    param(
        [string]$FilePath,
        [string[]]$Arguments
    )

    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Command failed: $FilePath $($Arguments -join ' ')"
    }
}

function Get-HttpStatusCode {
    param([System.Management.Automation.ErrorRecord]$ErrorRecord)

    $response = $ErrorRecord.Exception.Response
    if ($null -eq $response) {
        return $null
    }

    try {
        return [int]$response.StatusCode
    } catch {
        try {
            return [int]$response.StatusCode.value__
        } catch {
            return $null
        }
    }
}

function Invoke-RestAllowNotFound {
    param(
        [string]$Method,
        [string]$Uri,
        [hashtable]$Headers = @{},
        $Body = $null,
        [string]$ContentType = ""
    )

    try {
        if ($null -eq $Body) {
            return Invoke-RestMethod -Method $Method -Uri $Uri -Headers $Headers
        }

        if ([string]::IsNullOrWhiteSpace($ContentType)) {
            return Invoke-RestMethod -Method $Method -Uri $Uri -Headers $Headers -Body $Body
        }

        return Invoke-RestMethod -Method $Method -Uri $Uri -Headers $Headers -ContentType $ContentType -Body $Body
    } catch {
        if ((Get-HttpStatusCode $_) -eq 404) {
            return $null
        }

        throw
    }
}

function Get-GitRemoteUrl {
    param([string]$Remote)

    $url = (& git remote get-url $Remote 2>$null)
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($url)) {
        throw "Git remote not found: $Remote"
    }

    return $url.Trim()
}

function Get-RepoFromRemote {
    param(
        [string]$Remote,
        [string]$ExpectedHost
    )

    $url = Get-GitRemoteUrl -Remote $Remote

    if ($url -match '^git@(?<host>[^:]+):(?<owner>[^/]+)/(?<repo>[^/]+?)(?:\.git)?$') {
        $remoteHost = $Matches.host
        $owner = $Matches.owner
        $repo = $Matches.repo
    } elseif ($url -match '^(?:https?://|ssh://git@)(?<host>[^/]+)/(?<owner>[^/]+)/(?<repo>[^/]+?)(?:\.git)?/?$') {
        $remoteHost = $Matches.host
        $owner = $Matches.owner
        $repo = $Matches.repo
    } else {
        throw "Unsupported remote URL format for ${Remote}: $url"
    }

    if ($remoteHost -ne $ExpectedHost) {
        throw "Remote $Remote points to $remoteHost, expected $ExpectedHost"
    }

    return [pscustomobject]@{
        Remote = $Remote
        Host = $remoteHost
        Owner = $owner
        Repo = $repo
        Url = $url
    }
}

function Resolve-RepoInfo {
    param(
        [string]$Owner,
        [string]$Repo,
        [string]$Remote,
        [string]$HostName
    )

    if (-not [string]::IsNullOrWhiteSpace($Owner) -and -not [string]::IsNullOrWhiteSpace($Repo)) {
        return [pscustomobject]@{
            Remote = $Remote
            Host = $HostName
            Owner = $Owner
            Repo = $Repo
            Url = ""
        }
    }

    return Get-RepoFromRemote -Remote $Remote -ExpectedHost $HostName
}

function Get-ReleaseBody {
    if ([string]::IsNullOrWhiteSpace($BodyFile)) {
        return "Release $Tag"
    }

    if ([System.IO.Path]::IsPathRooted($BodyFile)) {
        $bodyPath = [System.IO.Path]::GetFullPath($BodyFile)
    } else {
        $bodyPath = [System.IO.Path]::GetFullPath((Join-Path $ProjectRootFull $BodyFile))
    }

    if (-not (Test-Path -LiteralPath $bodyPath)) {
        throw "Release body file not found: $bodyPath"
    }

    return Get-Content -LiteralPath $bodyPath -Raw
}

function Assert-CleanGitTree {
    if ($AllowDirty) {
        return
    }

    $status = (& git status --porcelain)
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to inspect git status"
    }

    if ($status) {
        throw "Working tree is not clean. Commit or stash changes, or rerun with -AllowDirty."
    }
}

function Ensure-ReleaseTag {
    if ($DryRun) {
        $forceText = if ($ForceTag) { " with force update" } else { "" }
        Write-Host "[dry-run] Ensure git tag $Tag points to HEAD$forceText" -ForegroundColor Yellow
        return
    }

    $head = (& git rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($head)) {
        throw "Failed to resolve HEAD"
    }

    $tagRef = "refs/tags/$Tag"
    $tagExists = $false
    $tagSha = (& git rev-parse -q --verify $tagRef 2>$null)
    if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($tagSha)) {
        $tagExists = $true
    }

    if ($tagExists) {
        $tagCommit = (& git rev-list -n 1 $tagRef).Trim()
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to resolve tag commit: $Tag"
        }

        if ($tagCommit -ne $head) {
            if (-not $ForceTag) {
                throw "Tag $Tag exists but does not point to HEAD. Rerun with -ForceTag to move it to HEAD."
            }

            Invoke-Native -FilePath "git" -Arguments @("tag", "-d", $Tag)
            Invoke-Native -FilePath "git" -Arguments @("tag", "-a", $Tag, "-m", "Release $Tag")
            Write-Host "Moved git tag to HEAD: $Tag" -ForegroundColor Yellow
            return
        }

        Write-Host "Git tag exists: $Tag" -ForegroundColor Green
        return
    }

    Invoke-Native -FilePath "git" -Arguments @("tag", "-a", $Tag, "-m", "Release $Tag")
    Write-Host "Created git tag: $Tag" -ForegroundColor Green
}

function Push-ReleaseTag {
    param([string]$Remote)

    if ($NoTagPush) {
        Write-Host "Skip tag push for $Remote (-NoTagPush)" -ForegroundColor Yellow
        return
    }

    if ($DryRun) {
        $forceText = if ($ForceTag) { " --force" } else { "" }
        Write-Host "[dry-run] git push$forceText $Remote refs/tags/$Tag" -ForegroundColor Yellow
        return
    }

    $arguments = @("push")
    if ($ForceTag) {
        $arguments += "--force"
    }
    $arguments += @($Remote, "refs/tags/$Tag")

    Invoke-Native -FilePath "git" -Arguments $arguments
}

function Invoke-ReleaseBuild {
    if ($NoBuild) {
        Write-Host "Skip build (-NoBuild)" -ForegroundColor Yellow
        return
    }

    $buildScript = Join-Path $ProjectRootFull "build.ps1"
    if (-not (Test-Path -LiteralPath $buildScript)) {
        throw "Build script not found: $buildScript"
    }

    $buildParams = @{
        DistDir = $DistFull
    }

    if (-not $NoClean) {
        $buildParams.Clean = $true
    }

    if ($DryRun) {
        $cleanText = if ($NoClean) { "" } else { " -Clean" }
        Write-Host "[dry-run] $buildScript -DistDir $DistFull$cleanText" -ForegroundColor Yellow
        return
    }

    & $buildScript @buildParams
    if ($LASTEXITCODE -ne 0) {
        throw "Build failed: $buildScript"
    }
}

function Get-DistAssets {
    if (-not (Test-Path -LiteralPath $DistFull)) {
        throw "Dist directory does not exist: $DistFull"
    }

    $nestedDirs = @(Get-ChildItem -LiteralPath $DistFull -Directory)
    if ($nestedDirs.Count -gt 0) {
        throw "Release assets must be files directly under dist. Nested directory found: $($nestedDirs[0].FullName)"
    }

    $assets = @(Get-ChildItem -LiteralPath $DistFull -File | Sort-Object Name)
    if ($assets.Count -eq 0) {
        throw "Dist directory has no files: $DistFull"
    }

    return $assets
}

function Get-GitHubHeaders {
    return @{
        "Authorization" = "Bearer $GitHubToken"
        "Accept" = "application/vnd.github+json"
        "X-GitHub-Api-Version" = $GitHubApiVersion
        "User-Agent" = "lin-simulator-release-script"
    }
}

function Get-ObjectPropertyValue {
    param(
        $Object,
        [string[]]$Names
    )

    if ($null -eq $Object) {
        return $null
    }

    foreach ($name in $Names) {
        $property = $Object.PSObject.Properties[$name]
        if ($null -ne $property -and -not [string]::IsNullOrWhiteSpace([string]$property.Value)) {
            return $property.Value
        }
    }

    return $null
}

function Get-ReleaseAssetName {
    param($Asset)

    $name = Get-ObjectPropertyValue -Object $Asset -Names @("name", "filename", "file_name", "path")
    if ([string]::IsNullOrWhiteSpace([string]$name)) {
        return ""
    }

    return [System.IO.Path]::GetFileName([string]$name)
}

function Get-ReleaseAssetID {
    param($Asset)

    return Get-ObjectPropertyValue -Object $Asset -Names @("id", "uuid")
}

function Get-GitHubRelease {
    param([pscustomobject]$RepoInfo)

    $headers = Get-GitHubHeaders
    $uri = "$GitHubApiBase/repos/$(ConvertTo-UrlEncoded $RepoInfo.Owner)/$(ConvertTo-UrlEncoded $RepoInfo.Repo)/releases/tags/$(ConvertTo-UrlEncoded $Tag)"
    return Invoke-RestAllowNotFound -Method "Get" -Uri $uri -Headers $headers
}

function Save-GitHubRelease {
    param(
        [pscustomobject]$RepoInfo,
        [string]$Body
    )

    $headers = Get-GitHubHeaders
    $release = Get-GitHubRelease -RepoInfo $RepoInfo
    $payload = @{
        tag_name = $Tag
        name = $ReleaseName
        body = $Body
        draft = $false
        prerelease = [bool]$Prerelease
    }

    if ($null -eq $release) {
        $payload.target_commitish = (& git branch --show-current).Trim()
        if ([string]::IsNullOrWhiteSpace($payload.target_commitish)) {
            $payload.target_commitish = "HEAD"
        }

        $uri = "$GitHubApiBase/repos/$(ConvertTo-UrlEncoded $RepoInfo.Owner)/$(ConvertTo-UrlEncoded $RepoInfo.Repo)/releases"
        $json = $payload | ConvertTo-Json -Depth 8
        return Invoke-RestMethod -Method Post -Uri $uri -Headers $headers -ContentType "application/json" -Body $json
    }

    $uri = "$GitHubApiBase/repos/$(ConvertTo-UrlEncoded $RepoInfo.Owner)/$(ConvertTo-UrlEncoded $RepoInfo.Repo)/releases/$($release.id)"
    $json = $payload | ConvertTo-Json -Depth 8
    return Invoke-RestMethod -Method Patch -Uri $uri -Headers $headers -ContentType "application/json" -Body $json
}

function Publish-GitHubAssets {
    param(
        [pscustomobject]$RepoInfo,
        $Release,
        [System.IO.FileInfo[]]$Assets
    )

    $headers = Get-GitHubHeaders
    $owner = ConvertTo-UrlEncoded $RepoInfo.Owner
    $repo = ConvertTo-UrlEncoded $RepoInfo.Repo
    $assetListUri = "$GitHubApiBase/repos/$owner/$repo/releases/$($Release.id)/assets?per_page=100"
    $existingAssets = @(Invoke-RestMethod -Method Get -Uri $assetListUri -Headers $headers)

    foreach ($asset in $Assets) {
        $assetName = $asset.Name
        foreach ($existing in @($existingAssets | Where-Object { (Get-ReleaseAssetName $_) -eq $assetName })) {
            $existingID = Get-ReleaseAssetID $existing
            if ([string]::IsNullOrWhiteSpace([string]$existingID)) {
                Write-Host "GitHub skipped existing asset without id: $assetName" -ForegroundColor Yellow
                continue
            }

            $deleteUri = "$GitHubApiBase/repos/$owner/$repo/releases/assets/$existingID"
            Invoke-RestMethod -Method Delete -Uri $deleteUri -Headers $headers | Out-Null
            Write-Host "GitHub deleted existing asset: $assetName" -ForegroundColor Yellow
        }

        $uploadUri = "$GitHubUploadBase/repos/$owner/$repo/releases/$($Release.id)/assets?name=$(ConvertTo-UrlEncoded $assetName)"
        Invoke-RestMethod -Method Post -Uri $uploadUri -Headers $headers -ContentType "application/octet-stream" -InFile $asset.FullName | Out-Null
        Write-Host "GitHub uploaded: $assetName" -ForegroundColor Green
    }
}

function Get-GiteeRelease {
    param([pscustomobject]$RepoInfo)

    $uri = "$GiteeApiBase/repos/$(ConvertTo-UrlEncoded $RepoInfo.Owner)/$(ConvertTo-UrlEncoded $RepoInfo.Repo)/releases/tags/$(ConvertTo-UrlEncoded $Tag)"
    $uri = Add-QueryString -Uri $uri -Parameters @{ access_token = $GiteeToken }
    return Invoke-RestAllowNotFound -Method "Get" -Uri $uri
}

function Save-GiteeRelease {
    param(
        [pscustomobject]$RepoInfo,
        [string]$Body
    )

    $release = Get-GiteeRelease -RepoInfo $RepoInfo
    if ($null -ne $release) {
        return $release
    }

    $uri = "$GiteeApiBase/repos/$(ConvertTo-UrlEncoded $RepoInfo.Owner)/$(ConvertTo-UrlEncoded $RepoInfo.Repo)/releases"
    $payload = @{
        access_token = $GiteeToken
        tag_name = $Tag
        name = $ReleaseName
        body = $Body
        prerelease = ([bool]$Prerelease).ToString().ToLowerInvariant()
    }

    return Invoke-RestMethod -Method Post -Uri $uri -Body $payload
}

function Invoke-GiteeFileUpload {
    param(
        [string]$Uri,
        [System.IO.FileInfo]$Asset
    )

    Add-Type -AssemblyName System.Net.Http

    $client = [System.Net.Http.HttpClient]::new()
    $content = [System.Net.Http.MultipartFormDataContent]::new()
    $stream = [System.IO.File]::OpenRead($Asset.FullName)

    try {
        $fileContent = [System.Net.Http.StreamContent]::new($stream)
        $fileContent.Headers.ContentType = [System.Net.Http.Headers.MediaTypeHeaderValue]::Parse("application/octet-stream")
        $content.Add($fileContent, "file", $Asset.Name)

        $response = $client.PostAsync($Uri, $content).GetAwaiter().GetResult()
        $responseBody = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        if (-not $response.IsSuccessStatusCode) {
            throw "Gitee upload failed for $($Asset.Name): HTTP $([int]$response.StatusCode) $responseBody"
        }

        if ([string]::IsNullOrWhiteSpace($responseBody)) {
            return $null
        }

        return $responseBody | ConvertFrom-Json
    } finally {
        $stream.Dispose()
        $content.Dispose()
        $client.Dispose()
    }
}

function Publish-GiteeAssets {
    param(
        [pscustomobject]$RepoInfo,
        $Release,
        [System.IO.FileInfo[]]$Assets
    )

    $owner = ConvertTo-UrlEncoded $RepoInfo.Owner
    $repo = ConvertTo-UrlEncoded $RepoInfo.Repo
    $releaseId = ConvertTo-UrlEncoded ([string]$Release.id)
    $assetListUri = "$GiteeApiBase/repos/$owner/$repo/releases/$releaseId/attach_files"
    $assetListUri = Add-QueryString -Uri $assetListUri -Parameters @{ access_token = $GiteeToken }
    $existingAssets = @(Invoke-RestMethod -Method Get -Uri $assetListUri)

    foreach ($asset in $Assets) {
        $assetName = $asset.Name
        foreach ($existing in @($existingAssets | Where-Object { (Get-ReleaseAssetName $_) -eq $assetName })) {
            $existingID = Get-ReleaseAssetID $existing
            if ([string]::IsNullOrWhiteSpace([string]$existingID)) {
                Write-Host "Gitee skipped existing asset without id: $assetName" -ForegroundColor Yellow
                continue
            }

            $deleteUri = "$GiteeApiBase/repos/$owner/$repo/releases/$releaseId/attach_files/$existingID"
            $deleteUri = Add-QueryString -Uri $deleteUri -Parameters @{ access_token = $GiteeToken }
            Invoke-RestMethod -Method Delete -Uri $deleteUri | Out-Null
            Write-Host "Gitee deleted existing asset: $assetName" -ForegroundColor Yellow
        }

        $uploadUri = "$GiteeApiBase/repos/$owner/$repo/releases/$releaseId/attach_files"
        $uploadUri = Add-QueryString -Uri $uploadUri -Parameters @{ access_token = $GiteeToken }
        Invoke-GiteeFileUpload -Uri $uploadUri -Asset $asset | Out-Null
        Write-Host "Gitee uploaded: $assetName" -ForegroundColor Green
    }
}

function Publish-GitHubRelease {
    param(
        [pscustomobject]$RepoInfo,
        [string]$Body,
        [System.IO.FileInfo[]]$Assets
    )

    if ($DryRun) {
        Write-Host "[dry-run] Publish GitHub release $Tag to $($RepoInfo.Owner)/$($RepoInfo.Repo)" -ForegroundColor Yellow
        foreach ($asset in $Assets) {
            Write-Host "[dry-run] GitHub upload: $($asset.Name)" -ForegroundColor Yellow
        }
        return
    }

    $release = Save-GitHubRelease -RepoInfo $RepoInfo -Body $Body
    Publish-GitHubAssets -RepoInfo $RepoInfo -Release $release -Assets $Assets
}

function Publish-GiteeRelease {
    param(
        [pscustomobject]$RepoInfo,
        [string]$Body,
        [System.IO.FileInfo[]]$Assets
    )

    if ($DryRun) {
        Write-Host "[dry-run] Publish Gitee release $Tag to $($RepoInfo.Owner)/$($RepoInfo.Repo)" -ForegroundColor Yellow
        foreach ($asset in $Assets) {
            Write-Host "[dry-run] Gitee upload: $($asset.Name)" -ForegroundColor Yellow
        }
        return
    }

    $release = Save-GiteeRelease -RepoInfo $RepoInfo -Body $Body
    Publish-GiteeAssets -RepoInfo $RepoInfo -Release $release -Assets $Assets
}

try {
    Push-Location $ProjectRootFull
    try {
        if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
            throw "git command not found"
        }

        if (-not (Test-Path -LiteralPath "go.mod")) {
            throw "Current directory is not a Go module root"
        }

        $publishGitHub = Test-ReleaseTarget -Name "github"
        $publishGitee = Test-ReleaseTarget -Name "gitee"
        $githubInfo = $null
        $giteeInfo = $null

        if ($publishGitHub) {
            $githubInfo = Resolve-RepoInfo -Owner $GitHubOwner -Repo $GitHubRepo -Remote $GitHubRemote -HostName "github.com"
            if (-not $DryRun -and [string]::IsNullOrWhiteSpace($GitHubToken)) {
                throw "Missing GitHub token. Set GITHUB_TOKEN or GH_TOKEN, or pass -GitHubToken."
            }
        }

        if ($publishGitee) {
            $giteeInfo = Resolve-RepoInfo -Owner $GiteeOwner -Repo $GiteeRepo -Remote $GiteeRemote -HostName "gitee.com"
            if (-not $DryRun -and [string]::IsNullOrWhiteSpace($GiteeToken)) {
                throw "Missing Gitee token. Set GITEE_TOKEN, or pass -GiteeToken."
            }
        }

        Assert-CleanGitTree
        Invoke-ReleaseBuild
        $assets = Get-DistAssets
        $releaseBody = Get-ReleaseBody
        Ensure-ReleaseTag

        if ($publishGitHub) {
            Push-ReleaseTag -Remote $GitHubRemote
        }

        if ($publishGitee) {
            Push-ReleaseTag -Remote $GiteeRemote
        }

        Write-Host ""
        Write-Host "Publishing release assets:" -ForegroundColor Cyan
        $assets | Select-Object Name, Length | Format-Table -AutoSize

        if ($publishGitHub) {
            Publish-GitHubRelease -RepoInfo $githubInfo -Body $releaseBody -Assets $assets
        }

        if ($publishGitee) {
            Publish-GiteeRelease -RepoInfo $giteeInfo -Body $releaseBody -Assets $assets
        }

        Write-Host ""
        Write-Host "Release publish complete: $Tag" -ForegroundColor Green
    } finally {
        Pop-Location
    }
} catch {
    Write-Host $_.Exception.Message -ForegroundColor Red
    exit 1
}

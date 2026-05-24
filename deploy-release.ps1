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

    $pairs = @(foreach ($key in $Parameters.Keys) {
        "{0}={1}" -f (ConvertTo-UrlEncoded $key), (ConvertTo-UrlEncoded ([string]$Parameters[$key]))
    })

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

function Invoke-NativeCapture {
    param(
        [string]$FilePath,
        [string[]]$Arguments,
        [switch]$AllowFailure
    )

    $output = @(& $FilePath @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    $textOutput = @($output | ForEach-Object { [string]$_ })

    if (-not $AllowFailure -and $exitCode -ne 0) {
        $message = "Command failed ($exitCode): $FilePath $($Arguments -join ' ')"
        if ($textOutput.Count -gt 0) {
            $message += "`n$($textOutput -join "`n")"
        }

        throw $message
    }

    return [pscustomobject]@{
        ExitCode = $exitCode
        Output = $textOutput
    }
}

function Require-Command {
    param(
        [string]$Name,
        [string]$InstallHint
    )

    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Command '$Name' was not found. $InstallHint"
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

function Redact-SecretText {
    param([string]$Text)

    if ([string]::IsNullOrEmpty($Text)) {
        return $Text
    }

    $redacted = $Text
    foreach ($secret in @($GitHubToken, $GiteeToken)) {
        if (-not [string]::IsNullOrWhiteSpace($secret)) {
            $redacted = $redacted.Replace($secret, "***")
        }
    }

    return $redacted
}

function Get-RestErrorBody {
    param([System.Management.Automation.ErrorRecord]$ErrorRecord)

    if ($null -ne $ErrorRecord.ErrorDetails -and -not [string]::IsNullOrWhiteSpace($ErrorRecord.ErrorDetails.Message)) {
        return $ErrorRecord.ErrorDetails.Message
    }

    $response = $ErrorRecord.Exception.Response
    if ($null -eq $response) {
        return ""
    }

    try {
        $stream = $response.GetResponseStream()
        if ($null -eq $stream) {
            return ""
        }

        $reader = [System.IO.StreamReader]::new($stream)
        try {
            return $reader.ReadToEnd()
        } finally {
            $reader.Dispose()
        }
    } catch {
        return ""
    }
}

function New-RestErrorMessage {
    param(
        [string]$Method,
        [string]$Uri,
        [System.Management.Automation.ErrorRecord]$ErrorRecord
    )

    $status = Get-HttpStatusCode $ErrorRecord
    $safeUri = Redact-SecretText $Uri
    $body = Redact-SecretText (Get-RestErrorBody $ErrorRecord)
    $message = "HTTP request failed: $Method $safeUri"

    if ($null -ne $status) {
        $message += "`nStatus: $status"
    }

    if (-not [string]::IsNullOrWhiteSpace($body)) {
        $message += "`nResponse: $body"
    } else {
        $message += "`nError: $($ErrorRecord.Exception.Message)"
    }

    return $message
}

function Invoke-ReleaseRestMethod {
    param(
        [string]$Method,
        [string]$Uri,
        [hashtable]$Headers = @{},
        $Body = $null,
        [string]$ContentType = "",
        [string]$InFile = "",
        [switch]$AllowNotFound
    )

    try {
        $parameters = @{
            Method = $Method
            Uri = $Uri
            ErrorAction = "Stop"
        }

        if ($Headers.Count -gt 0) {
            $parameters.Headers = $Headers
        }

        if (-not [string]::IsNullOrWhiteSpace($ContentType)) {
            $parameters.ContentType = $ContentType
        }

        if ($null -ne $Body) {
            $parameters.Body = $Body
        }

        if (-not [string]::IsNullOrWhiteSpace($InFile)) {
            $parameters.InFile = $InFile
        }

        return Invoke-RestMethod @parameters
    } catch {
        if ($AllowNotFound -and (Get-HttpStatusCode $_) -eq 404) {
            return $null
        }

        throw (New-RestErrorMessage -Method $Method -Uri $Uri -ErrorRecord $_)
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

    return Invoke-ReleaseRestMethod -Method $Method -Uri $Uri -Headers $Headers -Body $Body -ContentType $ContentType -AllowNotFound
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

function Get-RepoSlug {
    param([pscustomobject]$RepoInfo)

    return "$($RepoInfo.Owner)/$($RepoInfo.Repo)"
}

function New-NameSet {
    param([object[]]$Names)

    $set = New-Object "System.Collections.Generic.HashSet[string]" -ArgumentList ([StringComparer]::OrdinalIgnoreCase)
    foreach ($name in @($Names)) {
        if (-not [string]::IsNullOrWhiteSpace([string]$name)) {
            [void]$set.Add([string]$name)
        }
    }

    return ,$set
}

function Get-ReleaseBodyPath {
    if ([string]::IsNullOrWhiteSpace($BodyFile)) {
        return $null
    }

    if ([System.IO.Path]::IsPathRooted($BodyFile)) {
        $bodyPath = [System.IO.Path]::GetFullPath($BodyFile)
    } else {
        $bodyPath = [System.IO.Path]::GetFullPath((Join-Path $ProjectRootFull $BodyFile))
    }

    if (-not (Test-Path -LiteralPath $bodyPath)) {
        throw "Release body file not found: $bodyPath"
    }

    return $bodyPath
}

function Get-ReleaseBody {
    $bodyPath = Get-ReleaseBodyPath
    if ($null -eq $bodyPath) {
        return "Release $Tag"
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

function Get-TargetCommitish {
    $branch = (& git branch --show-current).Trim()
    if ([string]::IsNullOrWhiteSpace($branch)) {
        return "HEAD"
    }

    return $branch
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
            $safeUri = Redact-SecretText $Uri
            $safeBody = Redact-SecretText $responseBody
            throw "Gitee upload failed for $($Asset.Name): HTTP $([int]$response.StatusCode) $safeUri`nResponse: $safeBody"
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

function Set-GitHubCliToken {
    if ([string]::IsNullOrWhiteSpace($GitHubToken)) {
        return
    }

    $env:GH_TOKEN = $GitHubToken
    $env:GITHUB_TOKEN = $GitHubToken
}

function Get-GitHubNotesArguments {
    param([string]$Body)

    $bodyPath = Get-ReleaseBodyPath
    if ($null -ne $bodyPath) {
        return @("--notes-file", $bodyPath)
    }

    return @("--notes", $Body)
}

function Save-GitHubRelease {
    param(
        [pscustomobject]$RepoInfo,
        [string]$Body
    )

    $repoSlug = Get-RepoSlug -RepoInfo $RepoInfo
    $targetCommitish = Get-TargetCommitish
    $notesArguments = @(Get-GitHubNotesArguments -Body $Body)
    $viewResult = Invoke-NativeCapture -FilePath "gh" -Arguments @("release", "view", $Tag, "--repo", $repoSlug) -AllowFailure

    if ($viewResult.ExitCode -ne 0) {
        $createArguments = @("release", "create", $Tag, "--repo", $repoSlug, "--title", $ReleaseName, "--target", $targetCommitish, "--verify-tag")
        $createArguments += $notesArguments
        if ($Prerelease) {
            $createArguments += "--prerelease"
        }

        Write-Host "GitHub creating release..." -ForegroundColor Yellow
        Invoke-NativeCapture -FilePath "gh" -Arguments $createArguments | Out-Null
        return
    }

    $editArguments = @("release", "edit", $Tag, "--repo", $repoSlug, "--title", $ReleaseName, "--target", $targetCommitish)
    $editArguments += $notesArguments
    if ($Prerelease) {
        $editArguments += "--prerelease"
    }

    Write-Host "GitHub release exists. Updating metadata..." -ForegroundColor Yellow
    Invoke-NativeCapture -FilePath "gh" -Arguments $editArguments | Out-Null
}

function Get-GitHubAssetNames {
    param([pscustomobject]$RepoInfo)

    $repoSlug = Get-RepoSlug -RepoInfo $RepoInfo
    $result = Invoke-NativeCapture -FilePath "gh" -Arguments @("release", "view", $Tag, "--repo", $repoSlug, "--json", "assets", "--jq", ".assets[].name")
    return @($result.Output | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) })
}

function Publish-GitHubAssets {
    param(
        [pscustomobject]$RepoInfo,
        [System.IO.FileInfo[]]$Assets
    )

    $repoSlug = Get-RepoSlug -RepoInfo $RepoInfo
    $assetsToUpload = @()

    if ($ForceTag) {
        $assetsToUpload = @($Assets)
    } else {
        $existingNames = New-NameSet -Names (Get-GitHubAssetNames -RepoInfo $RepoInfo)
        foreach ($asset in $Assets) {
            if ($existingNames.Contains($asset.Name)) {
                Write-Host "Skip existing GitHub asset: $($asset.Name)" -ForegroundColor Yellow
            } else {
                $assetsToUpload += $asset
            }
        }
    }

    if ($assetsToUpload.Count -eq 0) {
        Write-Host "No GitHub assets to upload." -ForegroundColor Green
        return
    }

    $uploadArguments = @("release", "upload", $Tag, "--repo", $repoSlug)
    if ($ForceTag) {
        $uploadArguments += "--clobber"
    }

    foreach ($asset in $assetsToUpload) {
        $uploadArguments += $asset.FullName
    }

    Write-Host "Uploading GitHub assets:" -ForegroundColor Cyan
    foreach ($asset in $assetsToUpload) {
        Write-Host " - $($asset.Name)"
    }

    Invoke-NativeCapture -FilePath "gh" -Arguments $uploadArguments | Out-Null
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
        target_commitish = Get-TargetCommitish
    }

    if ($Prerelease) {
        $payload.prerelease = "true"
    }

    Write-Host "Gitee creating release..." -ForegroundColor Yellow
    return Invoke-ReleaseRestMethod -Method "Post" -Uri $uri -Body $payload
}

function Get-GiteeReleaseAssets {
    param(
        [pscustomobject]$RepoInfo,
        $Release
    )

    $owner = ConvertTo-UrlEncoded $RepoInfo.Owner
    $repo = ConvertTo-UrlEncoded $RepoInfo.Repo
    $releaseId = ConvertTo-UrlEncoded ([string]$Release.id)
    $assetListUri = "$GiteeApiBase/repos/$owner/$repo/releases/$releaseId/attach_files"
    $assetListUri = Add-QueryString -Uri $assetListUri -Parameters @{
        access_token = $GiteeToken
        per_page = 100
    }

    return @(Invoke-ReleaseRestMethod -Method "Get" -Uri $assetListUri)
}

function Remove-GiteeReleaseAsset {
    param(
        [pscustomobject]$RepoInfo,
        $Release,
        [string]$AssetID
    )

    $owner = ConvertTo-UrlEncoded $RepoInfo.Owner
    $repo = ConvertTo-UrlEncoded $RepoInfo.Repo
    $releaseId = ConvertTo-UrlEncoded ([string]$Release.id)
    $deleteUri = "$GiteeApiBase/repos/$owner/$repo/releases/$releaseId/attach_files/$AssetID"
    $deleteUri = Add-QueryString -Uri $deleteUri -Parameters @{ access_token = $GiteeToken }
    Invoke-ReleaseRestMethod -Method "Delete" -Uri $deleteUri | Out-Null
}

function Publish-GiteeAssets {
    param(
        [pscustomobject]$RepoInfo,
        $Release,
        [System.IO.FileInfo[]]$Assets
    )

    $existingAssets = @(Get-GiteeReleaseAssets -RepoInfo $RepoInfo -Release $Release)
    $assetsToUpload = @()

    if ($ForceTag) {
        foreach ($asset in $Assets) {
            $assetName = $asset.Name
            foreach ($existing in @($existingAssets | Where-Object { (Get-ReleaseAssetName $_) -eq $assetName })) {
                $existingID = [string](Get-ReleaseAssetID $existing)
                if ([string]::IsNullOrWhiteSpace($existingID)) {
                    Write-Host "Gitee skipped existing asset without id: $assetName" -ForegroundColor Yellow
                    continue
                }

                Remove-GiteeReleaseAsset -RepoInfo $RepoInfo -Release $Release -AssetID $existingID
                Write-Host "Gitee deleted existing asset: $assetName" -ForegroundColor Yellow
            }

            $assetsToUpload += $asset
        }
    } else {
        $existingNames = New-NameSet -Names (@($existingAssets | ForEach-Object { Get-ReleaseAssetName $_ }))
        foreach ($asset in $Assets) {
            if ($existingNames.Contains($asset.Name)) {
                Write-Host "Skip existing Gitee asset: $($asset.Name)" -ForegroundColor Yellow
            } else {
                $assetsToUpload += $asset
            }
        }
    }

    if ($assetsToUpload.Count -eq 0) {
        Write-Host "No Gitee assets to upload." -ForegroundColor Green
        return
    }

    $owner = ConvertTo-UrlEncoded $RepoInfo.Owner
    $repo = ConvertTo-UrlEncoded $RepoInfo.Repo
    $releaseId = ConvertTo-UrlEncoded ([string]$Release.id)
    $uploadUri = "$GiteeApiBase/repos/$owner/$repo/releases/$releaseId/attach_files"
    $uploadUri = Add-QueryString -Uri $uploadUri -Parameters @{ access_token = $GiteeToken }

    Write-Host "Uploading Gitee assets:" -ForegroundColor Cyan
    foreach ($asset in $assetsToUpload) {
        Write-Host " - $($asset.Name)"
        Invoke-GiteeFileUpload -Uri $uploadUri -Asset $asset | Out-Null
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

    Set-GitHubCliToken
    if ([string]::IsNullOrWhiteSpace($GitHubToken)) {
        $authStatus = Invoke-NativeCapture -FilePath "gh" -Arguments @("auth", "status", "--hostname", "github.com") -AllowFailure
        if ($authStatus.ExitCode -ne 0) {
            throw "GitHub CLI is not authenticated. Run gh auth login, or set GITHUB_TOKEN/GH_TOKEN."
        }
    }

    Save-GitHubRelease -RepoInfo $RepoInfo -Body $Body
    Publish-GitHubAssets -RepoInfo $RepoInfo -Assets $Assets
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
            if (-not $DryRun) {
                Require-Command -Name "gh" -InstallHint "Install GitHub CLI and ensure 'gh' is available in PATH."
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

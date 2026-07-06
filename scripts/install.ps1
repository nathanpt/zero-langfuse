[CmdletBinding()]
param(
  [string]$Version = $env:ZLF_VERSION,
  [string]$Repository = $(if ($env:ZLF_REPO) { $env:ZLF_REPO } else { "nathanpt/zero-langfuse" }),
  [string]$InstallDir = $env:ZLF_INSTALL_DIR,
  [string]$GitHubApi = $(if ($env:ZLF_GITHUB_API) { $env:ZLF_GITHUB_API } else { "https://api.github.com" }),
  [string]$GitHubBaseUrl = $(if ($env:ZLF_GITHUB_BASE_URL) { $env:ZLF_GITHUB_BASE_URL } else { "https://github.com" })
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($Version)) {
  $Version = "latest"
}

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
  $InstallDir = Join-Path $env:LOCALAPPDATA "zero-langfuse\bin"
}

function Get-ZlfLatestTag {
  param([string]$Repository, [string]$GitHubApi)

  $apiBase = $GitHubApi.TrimEnd([char[]]"/")
  $release = Invoke-RestMethod `
    -Uri "$apiBase/repos/$Repository/releases/latest" `
    -Headers @{ Accept = "application/vnd.github+json" } `
    -TimeoutSec 15

  if ([string]::IsNullOrWhiteSpace($release.tag_name)) {
    throw "GitHub release response did not include tag_name"
  }

  return [string]$release.tag_name
}

function Get-ZlfArch {
  $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()

  switch ($arch) {
    "X64" { return "amd64" }
    "Arm64" { return "arm64" }
    default { throw "Unsupported architecture: $arch" }
  }
}

function Find-ZlfExtractedFile {
  param(
    [string]$Root,
    [string]$FileName
  )

  $candidate = Join-Path $Root $FileName
  if (Test-Path $candidate -PathType Leaf) {
    return $candidate
  }

  $matches = @(Get-ChildItem -Path $Root -Filter $FileName -File -Recurse)
  if ($matches.Count -eq 1) {
    return $matches[0].FullName
  }

  throw "Release archive did not contain exactly one $FileName"
}

function Test-ZlfPathContainsDir {
  param(
    [string]$PathValue,
    [string]$Dir
  )

  if ([string]::IsNullOrEmpty($PathValue)) {
    return $false
  }

  return @($PathValue -split [System.IO.Path]::PathSeparator) -contains $Dir
}

if ($Version -eq "latest") {
  $tag = Get-ZlfLatestTag -Repository $Repository -GitHubApi $GitHubApi
} elseif ($Version.StartsWith("v")) {
  $tag = $Version
} else {
  $tag = "v$Version"
}

$releaseVersion = $tag -replace "^v", ""
$arch = Get-ZlfArch
$archiveName = "zero-langfuse_${releaseVersion}_windows_${arch}.zip"
$checksumName = "zero-langfuse_${releaseVersion}_checksums.txt"
$releaseBase = $GitHubBaseUrl.TrimEnd([char[]]"/")
$releaseUrl = "$releaseBase/$Repository/releases/download/$tag"
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("zero-langfuse-install-" + [System.Guid]::NewGuid().ToString("N"))
$extractDir = Join-Path $tempDir "extract"
$archivePath = Join-Path $tempDir $archiveName
$checksumPath = Join-Path $tempDir $checksumName

try {
  New-Item -ItemType Directory -Path $tempDir, $extractDir -Force | Out-Null

  Write-Host "Installing zero-langfuse $tag for windows-$arch"
  Invoke-WebRequest -Uri "$releaseUrl/$archiveName" -OutFile $archivePath -UseBasicParsing -TimeoutSec 300
  Invoke-WebRequest -Uri "$releaseUrl/$checksumName" -OutFile $checksumPath -UseBasicParsing -TimeoutSec 300

  # The checksums file is one combined sha256sum listing: <hash>  <filename>.
  # Find the line for our archive and take its hash (first whitespace-delimited field).
  $expectedChecksum = $null
  foreach ($rawLine in Get-Content -Path $checksumPath) {
    $line = $rawLine.Trim()
    if ($line -eq "") { continue }
    if ($line -match "\s${archiveName}$") {
      $expectedChecksum = ($line -split "\s+")[0].ToLowerInvariant()
      break
    }
  }
  if (-not $expectedChecksum) {
    throw "No checksum entry for $archiveName in $checksumName"
  }
  $actualChecksum = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($expectedChecksum -ne $actualChecksum) {
    throw "Checksum mismatch for $archiveName"
  }

  Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force

  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  $requiredFiles = @("zero-langfuse.exe")
  foreach ($fileName in $requiredFiles) {
    $sourcePath = Find-ZlfExtractedFile -Root $extractDir -FileName $fileName
    Copy-Item -Path $sourcePath -Destination (Join-Path $InstallDir $fileName) -Force
  }

  $targetPath = Join-Path $InstallDir "zero-langfuse.exe"
  Write-Host "Installed $targetPath"

  $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
  if (-not (Test-ZlfPathContainsDir -PathValue $userPath -Dir $InstallDir)) {
    try {
      $newUserPath = if ([string]::IsNullOrEmpty($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
      [Environment]::SetEnvironmentVariable("PATH", $newUserPath, "User")
      Write-Host "Added $InstallDir to your user PATH. Restart your terminal to use 'zero-langfuse'."
    } catch {
      Write-Warning "Could not update your user PATH automatically: $_"
      Write-Warning "Add $InstallDir to PATH manually to run zero-langfuse from any directory."
    }
  }

  if (-not (Test-ZlfPathContainsDir -PathValue $env:PATH -Dir $InstallDir)) {
    $env:PATH = "$env:PATH;$InstallDir"
  }
} finally {
  if (Test-Path $tempDir) {
    Remove-Item -Path $tempDir -Recurse -Force
  }
}

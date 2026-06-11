# Tulki/jprq Windows installer.
# Usage (PowerShell, normal user — placed in %LOCALAPPDATA%\jprq, added to PATH):
#   iwr -useb https://tulki.uz/install.ps1 | iex

$ErrorActionPreference = "Stop"

$Repo       = "musanna-soft/jprq"
$ReleaseTag = "v1.2"
$InstallDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "jprq" }

$Arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "386" }
$AssetName = "jprq-windows-$Arch.exe"
$Url       = "https://github.com/$Repo/releases/download/$ReleaseTag/$AssetName"

Write-Host "Downloading $AssetName from $Url"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$Target = Join-Path $InstallDir "jprq.exe"

try {
    Invoke-WebRequest -Uri $Url -OutFile $Target -UseBasicParsing
} catch {
    Write-Error "Download failed: $($_.Exception.Message)"
    exit 1
}

# Add InstallDir to the user's PATH if it's not already there.
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not ($UserPath -split ";" | Where-Object { $_ -ieq $InstallDir })) {
    $NewPath = if ([string]::IsNullOrEmpty($UserPath)) { $InstallDir } else { "$UserPath;$InstallDir" }
    [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
    Write-Host "Added $InstallDir to user PATH. Open a new terminal for it to take effect."
}

Write-Host ""
Write-Host "jprq is successfully installed at $Target"
Write-Host "Get your auth token at https://me.musanna.uz/api-keys, then run: jprq auth <token>"

# Registers the restart-message scheduled task (run at boot as SYSTEM).
# Must be run from an elevated (Administrator) PowerShell.
# Usage: pwsh -File scripts/install-task.ps1 [-ExePath C:\path\restart-message.exe] [-ConfigPath C:\ProgramData\restart-message\config.json]
param(
    [string]$ExePath,
    [string]$ConfigPath
)

$ErrorActionPreference = "Stop"

# Require elevation.
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    throw "管理者権限の PowerShell で実行してください。"
}

if (-not $ExePath) {
    $root = Split-Path -Parent $PSScriptRoot
    $ExePath = Join-Path $root "bin\restart-message.exe"
}
if (-not (Test-Path $ExePath)) {
    throw "実行ファイルが見つかりません: $ExePath （先に scripts/build.ps1 を実行してください）"
}
$ExePath = (Resolve-Path $ExePath).Path

# Delegate to the binary's own installer for a single source of truth.
$installArgs = @("install")
if ($ConfigPath) { $installArgs += @("-config", $ConfigPath) }
& $ExePath @installArgs

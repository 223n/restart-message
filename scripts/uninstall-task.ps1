# Removes the restart-message scheduled task.
# Must be run from an elevated (Administrator) PowerShell.
$ErrorActionPreference = "Stop"

$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    throw "管理者権限の PowerShell で実行してください。"
}

schtasks /Delete /TN "restart-message" /F

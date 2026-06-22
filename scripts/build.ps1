# Builds restart-message.exe into ./bin
# Usage: pwsh -File scripts/build.ps1 [-Version 0.1.0]
param(
    [string]$Version = "0.2.0"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$env:GOOS = "windows"
$env:GOARCH = "amd64"

$out = Join-Path $root "bin\restart-message.exe"
New-Item -ItemType Directory -Force (Split-Path $out) | Out-Null

Write-Host "Building restart-message $Version -> $out"
go build -trimpath -ldflags "-s -w -X main.Version=$Version" -o $out .
Write-Host "Done: $out"

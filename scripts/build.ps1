# Builds restart-message.exe into ./bin
# Usage: pwsh -File scripts/build.ps1 [-Version 0.1.0] [-Arch arm64]
param(
    # Empty by default: with no -Version, the -X linker flag is omitted entirely and the
    # value of `var Version` in main.go is used as-is. Hard-coding the number here would
    # duplicate main.go and silently go stale at the next release.
    [string]$Version = "",
    [ValidateSet("amd64", "arm64")]
    [string]$Arch = "amd64"
)

$ErrorActionPreference = "Stop"

# ValidateSet is case-insensitive and hands the value through with the caller's casing,
# but GOARCH is not: `-Arch ARM64` would otherwise reach go as the unsupported pair
# windows/ARM64. `-Arch AMD64` is worse still, because -eq is case-insensitive too, so
# the script would announce the documented output path and only then fail inside go.
$Arch = $Arch.ToLowerInvariant()

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$env:GOOS = "windows"
$env:GOARCH = $Arch

# amd64 keeps the plain bin\restart-message.exe path documented in README.md; other
# architectures get a suffixed name so the two cannot be mistaken for each other.
if ($Arch -eq "amd64") {
    $out = Join-Path $root "bin\restart-message.exe"
} else {
    $out = Join-Path $root "bin\restart-message_$Arch.exe"
}
New-Item -ItemType Directory -Force (Split-Path $out) | Out-Null

$ldflags = "-s -w"
if ($Version) {
    $ldflags = "$ldflags -X main.Version=$Version"
    $label = "バージョン $Version"
} else {
    $label = "バージョンは main.go の既定値"
}

Write-Host "restart-message をビルドします: windows/$Arch, $label -> $out"
go build -trimpath -ldflags $ldflags -o $out .
# Windows PowerShell 5.1 does not apply $ErrorActionPreference to native commands, so a
# failing go build would otherwise fall through to the success message below.
if ($LASTEXITCODE -ne 0) { throw "go build に失敗しました (終了コード: $LASTEXITCODE)" }
Write-Host "完了: $out"

if ($Arch -ne "amd64") {
    # install-task.ps1 の既定は bin\restart-message.exe なので、明示しないと
    # 「実行ファイルが見つかりません」になるか、古い amd64 版が残っていれば
    # そちらが黙ってタスクに登録されてしまう。
    Write-Host "scripts/install-task.ps1 には -ExePath $out を渡してください。"
}

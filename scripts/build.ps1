# Build Windows tray and console binaries.
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root
$env:Path = "C:\Program Files\Go\bin;$env:USERPROFILE\go\bin;" + $env:Path

# Replace the current Windows binaries and remove obsolete local release
# variants. Other files in dist remain untouched.
New-Item -ItemType Directory -Force -Path dist | Out-Null
$artifacts = @(
    "dist/hellogrok.exe",
    "dist/hellogrok-cli.exe",
    "dist/hellogrok-windows-amd64.exe",
    "dist/hellogrok-windows-arm64.exe",
    "dist/hellogrok-cli-windows-amd64.exe",
    "dist/hellogrok-cli-windows-arm64.exe",
    "dist/hellogrok-linux-amd64",
    "dist/hellogrok-linux-arm64",
    "dist/hellogrok-cli-linux-amd64",
    "dist/hellogrok-cli-linux-arm64",
    "dist/hellogrok-darwin-amd64",
    "dist/hellogrok-darwin-arm64",
    "dist/hellogrok-cli-darwin-amd64",
    "dist/hellogrok-cli-darwin-arm64"
)
foreach ($artifact in $artifacts) {
    if (Test-Path -LiteralPath $artifact) {
        Remove-Item -LiteralPath $artifact -Force
    }
}

# The repository includes architecture-specific Windows resources generated from
# cmd/hellogrok/icon.ico, so normal builds need only Go.
go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/hellogrok.exe ./cmd/hellogrok
go build -trimpath -ldflags "-s -w" -o dist/hellogrok-cli.exe ./cmd/hellogrok
Write-Host "OK: $root\dist\hellogrok.exe"
Write-Host "OK: $root\dist\hellogrok-cli.exe"
Get-ChildItem dist | Sort-Object Name | Format-Table Name, Length

# Builds the xcod Linux binaries into dist/, with the installer next to them.
# Usage: .\build.ps1 [-Version v1.0.0]
# Then copy dist/ to the server (scp) and run "sudo bash install.sh".
param([string]$Version = (Get-Date -Format "yyyy.MM.dd-HHmm"))
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null
try {
  $env:GOOS = "linux"; $env:CGO_ENABLED = "0"
  foreach ($arch in "amd64", "arm64") {
    $env:GOARCH = $arch
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "dist/xcod-linux-$arch" ./cmd/xcod
    if ($LASTEXITCODE) { throw "build failed ($arch)" }
  }
} finally {
  Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}
Copy-Item deploy/install.sh dist/install.sh
Write-Host "done: dist/ (version $Version)"
Get-ChildItem dist | Format-Table Name, Length

$ErrorActionPreference = "Stop"

$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$OutDir = Join-Path $Root "dist\9router-gateway-linux-amd64"
$TarPath = Join-Path $Root "dist\9router-gateway-linux-amd64.tar.gz"

if (Test-Path $OutDir) { Remove-Item -Recurse -Force $OutDir }
New-Item -ItemType Directory -Force -Path (Join-Path $OutDir "bin") | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $OutDir "systemd") | Out-Null

$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
try {
    & go build -trimpath -ldflags="-s -w" -o (Join-Path $OutDir "bin\9router-go") ./cmd/9router-go/
    if ($LASTEXITCODE -ne 0) { throw "linux build failed" }
} finally {
    Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
}

# Normalize LF line endings for Linux scripts, systemd unit, docs, and env template
foreach ($Pair in @(
    @{ Src = "deploy\start.sh"; Dst = "bin\start.sh" },
    @{ Src = "deploy\install.sh"; Dst = "install.sh" },
    @{ Src = "deploy\9router-gateway.service"; Dst = "systemd\9router-gateway.service" },
    @{ Src = "deploy\README_DEPLOY.md"; Dst = "README_DEPLOY.md" },
    @{ Src = ".env.example"; Dst = ".env.example" }
)) {
    $Content = (Get-Content -Raw (Join-Path $Root $Pair.Src)) -replace "`r`n", "`n"
    [System.IO.File]::WriteAllText((Join-Path $OutDir $Pair.Dst), $Content, [System.Text.UTF8Encoding]::new($false))
}

if (Get-Command tar -ErrorAction SilentlyContinue) {
    if (Test-Path $TarPath) { Remove-Item -Force $TarPath }
    & tar -czf $TarPath -C $OutDir .
    if ($LASTEXITCODE -ne 0) { throw "tar archive creation failed" }
    $Hash = (Get-FileHash -Algorithm SHA256 $TarPath).Hash.ToLowerInvariant()
    [System.IO.File]::WriteAllText("$TarPath.sha256", "$Hash  9router-gateway-linux-amd64.tar.gz`n", [System.Text.UTF8Encoding]::new($false))
    Write-Host "PACKAGED_BUNDLE=$TarPath"
    Write-Host "PACKAGED_SHA256=$Hash"
} else {
    Write-Host "PACKAGED_DIR=$OutDir"
}

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

# Normalize LF line endings for Linux shell scripts
foreach ($Pair in @(
    @{ Src = "deploy\start-gateway.sh"; Dst = "bin\start-gateway.sh" },
    @{ Src = "deploy\entrypoint.sh";    Dst = "bin\entrypoint.sh" }
)) {
    $Content = (Get-Content -Raw (Join-Path $Root $Pair.Src)) -replace "`r`n", "`n"
    [System.IO.File]::WriteAllText((Join-Path $OutDir $Pair.Dst), $Content, [System.Text.UTF8Encoding]::new($false))
}

Copy-Item (Join-Path $Root "deploy\9router-gateway.service") (Join-Path $OutDir "systemd\9router-gateway.service")
Copy-Item (Join-Path $Root "deploy\README_DEPLOY.md") (Join-Path $OutDir "README_DEPLOY.md")
Copy-Item (Join-Path $Root ".env.example") (Join-Path $OutDir ".env.example")

if (Get-Command tar -ErrorAction SilentlyContinue) {
    if (Test-Path $TarPath) { Remove-Item -Force $TarPath }
    & tar -czf $TarPath -C $OutDir .
    if ($LASTEXITCODE -ne 0) { throw "tar archive creation failed" }
    Write-Host "PACKAGED_BUNDLE=$TarPath"
} else {
    Write-Host "PACKAGED_DIR=$OutDir"
}

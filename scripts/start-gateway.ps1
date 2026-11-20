$ErrorActionPreference = "Stop"

$env:HOST = if ($env:HOST) { $env:HOST } else { "127.0.0.1" }
$env:PORT = if ($env:PORT) { $env:PORT } else { "20128" }
$env:DATA_DIR = Join-Path $env:APPDATA "9router"
$env:DASHBOARD_PORT = if ($env:DASHBOARD_PORT) { $env:DASHBOARD_PORT } else { "3000" }
$env:DASHBOARD_URL = "http://127.0.0.1:$($env:DASHBOARD_PORT)"

$CliJs = Join-Path $env:APPDATA "npm\node_modules\9router\cli.js"
if (-not (Test-Path $CliJs)) {
    throw "Companion 9router dashboard not found at $CliJs (install via: npm install -g 9router@0.5.119)"
}

$DashProc = Start-Process -FilePath "node" -ArgumentList @($CliJs, "--port", $env:DASHBOARD_PORT, "--host", "127.0.0.1", "--no-browser", "--skip-update", "--log") -PassThru -NoNewWindow
try {
    for ($i = 0; $i -lt 40; $i++) {
        try {
            $c = [System.Net.Sockets.TcpClient]::new("127.0.0.1", [int]$env:DASHBOARD_PORT)
            $c.Close()
            break
        } catch {
            Start-Sleep -Milliseconds 250
        }
    }
    $Exe = Join-Path $env:USERPROFILE "go\bin\9router-go.exe"
    if (Test-Path $Exe) {
        & $Exe
    } else {
        & go run ./cmd/9router-go
    }
} finally {
    if ($DashProc -and -not $DashProc.HasExited) {
        Stop-Process -Id $DashProc.Id -Force -ErrorAction SilentlyContinue
    }
}

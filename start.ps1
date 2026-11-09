# Builds the image, starts the container, and checks the page from the host
# (outside the container). Run from this folder:  powershell -ExecutionPolicy Bypass -File .\start.ps1
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

docker compose up -d --build
if ($LASTEXITCODE -ne 0) { throw "docker compose failed" }

$url = "http://localhost:8080/"
for ($i = 0; $i -lt 30; $i++) {
    try {
        $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 3
        if ($r.StatusCode -eq 200 -and $r.Content -match "<title>Feature Table</title>") {
            Write-Host "OK: $url returned the web page (HTTP $($r.StatusCode))" -ForegroundColor Green
            exit 0
        }
    } catch { }
    Start-Sleep -Seconds 1
}
docker compose logs --tail 50
throw "Page did not come up at $url"

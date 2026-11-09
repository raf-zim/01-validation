# Stops the running container, rebuilds the image (go vet + go test run inside
# the build), starts the new container and runs verify.ps1 against it.
# Output is also written to deploy.log.
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\deploy.ps1
$ErrorActionPreference = "Continue"
Set-Location $PSScriptRoot
Start-Transcript -Path (Join-Path $PSScriptRoot "deploy.log") -Force | Out-Null

function Step($name, [scriptblock]$cmd) {
    Write-Host "`n=== $name ==="
    & $cmd
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAILED: $name (exit $LASTEXITCODE)"
        Stop-Transcript | Out-Null
        exit 1
    }
}

Step "Stop existing container (data volume is kept)" { docker compose down }
Step "Build image (runs go vet + go test)"            { docker compose build --progress plain }
Step "Start new container"                             { docker compose up -d }

Write-Host "`n=== Wait for container to report healthy ==="
$status = ""
for ($i = 0; $i -lt 30; $i++) {
    $status = docker inspect --format "{{.State.Health.Status}}" featuretable
    if ($status -eq "healthy") { break }
    Start-Sleep -Seconds 2
}
Write-Host "health: $status"
if ($status -ne "healthy") { docker compose logs --tail 50; Stop-Transcript | Out-Null; exit 1 }

Step "Verify from host" { powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot "verify.ps1") }

Write-Host "`nDEPLOY OK"
Stop-Transcript | Out-Null
exit 0

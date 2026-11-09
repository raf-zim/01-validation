# Checks the running container from the host (outside the container).
# Exits 0 if every check passes, 1 otherwise. Results also go to verify.log.
$ErrorActionPreference = "Stop"
$base = "http://localhost:8080"
$results = New-Object System.Collections.Generic.List[string]
$failed = 0
$logDir = Join-Path $PSScriptRoot "logs"
$today  = (Get-Date).ToString("yyyy-MM-dd")
$old5   = (Get-Date).AddDays(-5).ToString("yyyy-MM-dd")   # older than 4 days: must be purged
$old4   = (Get-Date).AddDays(-4).ToString("yyyy-MM-dd")   # exactly 4 days: must be kept

function Check($name, [scriptblock]$test) {
    try {
        $detail = & $test
        $script:results.Add("PASS  $name  $detail")
    } catch {
        $script:failed++
        $script:results.Add("FAIL  $name  $($_.Exception.Message)")
    }
}
function Assert($cond, $msg) { if (-not $cond) { throw $msg } }
function Req($method, $path, $body, $headers) {
    $p = @{ Uri = "$base$path"; Method = $method; UseBasicParsing = $true; Headers = $headers }
    if ($body) { $p.Body = $body; $p.ContentType = "application/json" }
    try { Invoke-WebRequest @p } catch {
        if ($_.Exception.Response) { return $_.Exception.Response } else { throw }
    }
}
function Status($r) { [int]$r.StatusCode }
function Hdr($r, $n) { if ($r.Headers -is [System.Net.WebHeaderCollection]) { $r.Headers[$n] } else { "$($r.Headers[$n])" } }

Check "web page served" {
    $r = Req GET "/"
    Assert ((Status $r) -eq 200 -and $r.Content -match "<title>Feature Table</title>") "status $(Status $r)"
    "HTTP 200"
}
Check "health check returns 204" {
    $r = Req GET "/healthz"
    Assert ((Status $r) -eq 204) "status $(Status $r)"
    "HTTP 204"
}
Check "tracing: incoming X-Request-Id echoed" {
    $r = Req GET "/api/rows" $null @{ "X-Request-Id" = "verify-trace-001" }
    Assert ((Hdr $r "X-Request-Id") -eq "verify-trace-001") "got '$(Hdr $r 'X-Request-Id')'"
    "verify-trace-001"
}
Check "tracing: X-Request-Id generated when absent" {
    $r = Req GET "/api/rows"
    $id = Hdr $r "X-Request-Id"
    Assert ($id -match "^[0-9a-f]{16}$") "got '$id'"
    $id
}
Check "tracing: unsafe X-Request-Id replaced" {
    $r = Req GET "/api/rows" $null @{ "X-Request-Id" = "bad id with spaces" }
    $id = Hdr $r "X-Request-Id"
    Assert ($id -match "^[0-9a-f]{16}$") "got '$id'"
    $id
}
Check "CRUD insert / update / delete" {
    $c = Req POST "/api/rows" '{"date":"2026-11-09","name":"verify","feature":"temp row"}'
    Assert ((Status $c) -eq 201) "insert status $(Status $c)"
    $id = ($c.Content | ConvertFrom-Json).id
    $u = Req PUT "/api/rows/$id" '{"date":"2026-11-10","name":"verify","feature":"edited"}'
    Assert ((Status $u) -eq 200 -and $u.Content -match "edited") "update status $(Status $u)"
    $d = Req DELETE "/api/rows/$id"
    Assert ((Status $d) -eq 204) "delete status $(Status $d)"
    "row $id created, edited, deleted"
}
Check "validation rejects bad date" {
    $r = Req POST "/api/rows" '{"date":"nope","name":"x","feature":"y"}'
    Assert ((Status $r) -eq 400) "status $(Status $r)"
    "HTTP 400"
}
Check "logging: access log has request id, status, duration" {
    $logs = docker logs featuretable | Out-String
    Assert ($logs -match 'http: .*id=verify-trace-001 GET /api/rows status=200 bytes=\d+ dur=') "no matching access-log line"
    "found"
}
Check "log file: today's dated file on host has the request" {
    $f = Join-Path $logDir "http-$today.log"
    Assert (Test-Path $f) "missing $f"
    Assert ((Get-Content $f -Raw) -match 'id=verify-trace-001 GET /api/rows status=200') "request line not in $f"
    "logs\http-$today.log"
}
Check "container health + restart policy" {
    $s = docker inspect --format "{{.State.Health.Status}} {{.HostConfig.RestartPolicy.Name}}" featuretable
    Assert ($s -eq "healthy unless-stopped") "got '$s'"
    $s
}
Check "graceful shutdown on docker stop" {
    # Plant old log files; the restart below must purge only the >4-day one.
    Set-Content (Join-Path $logDir "http-$old5.log") "planted by verify.ps1"
    Set-Content (Join-Path $logDir "http-$old4.log") "planted by verify.ps1"
    $t = Measure-Command { docker stop featuretable | Out-Null }
    $code = docker inspect --format "{{.State.ExitCode}}" featuretable
    $logs = docker logs --tail 5 featuretable | Out-String
    docker start featuretable | Out-Null
    Assert ($code -eq "0") "exit code $code (non-zero means killed or crashed)"
    Assert ($logs -match "received terminated, server is shutting down") "shutdown not logged"
    Assert ($logs -match "server stopped") "'server stopped' not logged"
    "exit 0 in $([math]::Round($t.TotalSeconds,1))s"
}
Check "container back up and healthy after restart" {
    $s = ""
    for ($i = 0; $i -lt 20; $i++) {
        $s = docker inspect --format "{{.State.Health.Status}}" featuretable
        if ($s -eq "healthy") { break }
        Start-Sleep -Seconds 2
    }
    Assert ($s -eq "healthy") "status '$s'"
    $r = Req GET "/"
    Assert ((Status $r) -eq 200) "page status $(Status $r)"
    "healthy, page HTTP 200"
}

Check "log purge on startup: >4 days removed, 4 days kept" {
    Assert (-not (Test-Path (Join-Path $logDir "http-$old5.log"))) "http-$old5.log was not purged"
    Assert (Test-Path (Join-Path $logDir "http-$old4.log")) "http-$old4.log was wrongly purged"
    $note = Get-Content (Join-Path $logDir "http-$today.log") -Raw
    Assert ($note -match "removed http-$old5\.log") "purge not recorded in today's log"
    Remove-Item (Join-Path $logDir "http-$old4.log")
    "removed http-$old5.log, kept http-$old4.log"
}

$results.Add("")
$results.Add($(if ($failed -eq 0) { "ALL CHECKS PASSED" } else { "$failed CHECK(S) FAILED" }))
$results | Set-Content (Join-Path $PSScriptRoot "verify.log")
$results | ForEach-Object { Write-Host $_ }
if ($failed -eq 0) { exit 0 } else { exit 1 }

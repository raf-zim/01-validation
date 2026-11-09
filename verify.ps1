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
# multipart upload with curl.exe (Windows PowerShell 5.1 has no -Form); returns "status body"
function Upload($id, $path, $name) {
    $out = curl.exe -s -w "`n%{http_code}" -X PUT -F "file=@$path;filename=$name" "$base/api/rows/$id/file"
    $lines = @($out)
    return @{ Status = [int]$lines[-1]; Body = ($lines[0..($lines.Count - 2)] -join "`n") }
}
$tmpDir = Join-Path $env:TEMP "featuretable-verify"
New-Item -ItemType Directory -Force $tmpDir | Out-Null
$pngPath = Join-Path $tmpDir "verify.png"
[IO.File]::WriteAllBytes($pngPath, [Convert]::FromBase64String("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="))
$txtPath = Join-Path $tmpDir "verify.txt"
[IO.File]::WriteAllText($txtPath, "hello from verify.ps1")
$htmlPath = Join-Path $tmpDir "verify.html"
[IO.File]::WriteAllText($htmlPath, "<html><script>alert(1)</script></html>")
$pdfPath = Join-Path $tmpDir "verify.pdf"
[IO.File]::WriteAllText($pdfPath, "%PDF-1.4`n1 0 obj << /Type /Catalog >> endobj`ntrailer << /Root 1 0 R >>`n%%EOF`n")
$mp4Path = Join-Path $tmpDir "verify.mp4"
# ftyp box (size 24, brand isom) + an empty free box: enough to be recognised as MP4
[IO.File]::WriteAllBytes($mp4Path, [byte[]]([byte[]](0,0,0,24) + [Text.Encoding]::ASCII.GetBytes("ftypisom") + [byte[]](0,0,2,0) + [Text.Encoding]::ASCII.GetBytes("isomiso2") + [byte[]](0,0,0,8) + [Text.Encoding]::ASCII.GetBytes("free")))
$script:fileRowId = $null

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
Check "database file on host is the live database" {
    $db = Join-Path $PSScriptRoot "db\featuretable.db"
    Assert (Test-Path $db) "missing $db"
    $t0 = (Get-Item $db).LastWriteTimeUtc
    Start-Sleep -Milliseconds 1100
    $c = Req POST "/api/rows" '{"date":"2026-11-09","name":"verify","feature":"db file check"}'
    $id = ($c.Content | ConvertFrom-Json).id
    $t1 = (Get-Item $db).LastWriteTimeUtc
    Req DELETE "/api/rows/$id" | Out-Null
    Assert ($t1 -gt $t0) "db\featuretable.db not modified by an insert"
    Assert (-not (Test-Path "$db-wal")) "unexpected -wal file (journal mode should be DELETE)"
    $logs = docker logs featuretable | Out-String
    Assert ($logs -match "sqlite database /db/featuretable.db \(journal mode DELETE\)") "journal mode DELETE not logged"
    "db\featuretable.db, journal mode DELETE"
}
Check "file: upload png, view inline, download attachment" {
    $c = Req POST "/api/rows" '{"date":"2026-11-09","name":"verify","feature":"file check"}'
    $id = ($c.Content | ConvertFrom-Json).id
    $script:fileRowId = $id
    $u = Upload $id $pngPath "verify.png"
    Assert ($u.Status -eq 200) "upload status $($u.Status) $($u.Body)"
    $f = ($u.Body | ConvertFrom-Json).file
    Assert ($f.type -eq "image/png" -and $f.viewable -eq $true -and $f.size -eq (Get-Item $pngPath).Length) "file info $($u.Body)"
    $v = Req GET "/api/rows/$id/file?view=1"
    Assert ((Status $v) -eq 200) "view status $(Status $v)"
    Assert ((Hdr $v "Content-Type") -eq "image/png") "view type $(Hdr $v 'Content-Type')"
    Assert ((Hdr $v "Content-Disposition") -like "inline*") "view disposition $(Hdr $v 'Content-Disposition')"
    Assert ((Hdr $v "Content-Security-Policy") -match "sandbox") "no sandbox CSP"
    Assert ((Hdr $v "X-Content-Type-Options") -eq "nosniff") "no nosniff"
    $dlPath = Join-Path $tmpDir "download.bin"
    $code = curl.exe -s -o $dlPath -w "%{http_code}" "$base/api/rows/$id/file"
    Assert ($code -eq "200") "download status $code"
    Assert ((Get-FileHash $dlPath).Hash -eq (Get-FileHash $pngPath).Hash) "downloaded bytes differ"
    $d = Req GET "/api/rows/$id/file"
    Assert ((Hdr $d "Content-Disposition") -like "attachment*verify.png*") "download disposition $(Hdr $d 'Content-Disposition')"
    "row ${id}: png viewable, download identical"
}
Check "file: txt viewable, html never served inline" {
    $id = $script:fileRowId
    $t = Upload $id $txtPath "verify.txt"
    Assert ($t.Status -eq 200) "txt upload $($t.Status)"
    $v = Req GET "/api/rows/$id/file?view=1"
    Assert ((Hdr $v "Content-Type") -eq "text/plain; charset=utf-8") "txt view type $(Hdr $v 'Content-Type')"
    $h = Upload $id $htmlPath "verify.html"
    Assert ($h.Status -eq 200 -and ($h.Body | ConvertFrom-Json).file.viewable -eq $false) "html upload $($h.Status) $($h.Body)"
    $v = Req GET "/api/rows/$id/file?view=1"
    Assert ((Status $v) -eq 415) "html view status $(Status $v), want 415"
    $d = Req GET "/api/rows/$id/file"
    Assert ((Hdr $d "Content-Type") -eq "application/octet-stream") "html download type $(Hdr $d 'Content-Type')"
    "txt text/plain inline; html 415 + octet-stream"
}
Check "file: mp4 and pdf viewable inline" {
    $id = $script:fileRowId
    $m = Upload $id $mp4Path "verify.mp4"
    $mf = ($m.Body | ConvertFrom-Json).file
    Assert ($m.Status -eq 200 -and $mf.type -eq "video/mp4" -and $mf.viewable -eq $true) "mp4 upload $($m.Status) $($m.Body)"
    $v = Req GET "/api/rows/$id/file?view=1"
    Assert ((Hdr $v "Content-Type") -eq "video/mp4" -and (Hdr $v "Content-Disposition") -like "inline*") "mp4 view $(Hdr $v 'Content-Type') $(Hdr $v 'Content-Disposition')"
    Assert ((Hdr $v "Content-Security-Policy") -match "media-src 'self'") "mp4 CSP $(Hdr $v 'Content-Security-Policy')"
    $range = curl.exe -s -o NUL -w "%{http_code}" -H "Range: bytes=0-3" "$base/api/rows/$id/file?view=1"
    Assert ($range -eq "206") "mp4 range request $range, want 206"
    $p = Upload $id $pdfPath "verify.pdf"
    $pf = ($p.Body | ConvertFrom-Json).file
    Assert ($p.Status -eq 200 -and $pf.type -eq "application/pdf" -and $pf.viewable -eq $true) "pdf upload $($p.Status) $($p.Body)"
    $v = Req GET "/api/rows/$id/file?view=1"
    Assert ((Hdr $v "Content-Type") -eq "application/pdf" -and (Hdr $v "Content-Disposition") -like "inline*") "pdf view $(Hdr $v 'Content-Type')"
    Assert (-not (Hdr $v "Content-Security-Policy")) "pdf should have no CSP, got $(Hdr $v 'Content-Security-Policy')"
    Assert ((Hdr $v "X-Content-Type-Options") -eq "nosniff") "pdf missing nosniff"
    $u = Upload $id $pngPath "verify.png"
    Assert ($u.Status -eq 200) "re-upload png $($u.Status)"
    "mp4 video/mp4 inline + range 206; pdf application/pdf inline"
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

Check "file survives container restart" {
    $id = $script:fileRowId
    Assert ($id) "no file row from earlier check"
    $dlPath = Join-Path $tmpDir "after-restart.bin"
    $code = curl.exe -s -o $dlPath -w "%{http_code}" "$base/api/rows/$id/file"
    Assert ($code -eq "200") "download after restart $code"
    Assert ((Get-FileHash $dlPath).Hash -eq (Get-FileHash $pngPath).Hash) "bytes differ after restart"
    $d = Req DELETE "/api/rows/$id"
    Assert ((Status $d) -eq 204) "cleanup delete $(Status $d)"
    "row $id file intact, row cleaned up"
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

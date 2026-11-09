# Exercises the API of the running container from the host and logs results.
$b = "http://localhost:8080"
$ErrorActionPreference = "Stop"
$o = @()
$page = Invoke-WebRequest "$b/" -UseBasicParsing
$o += "GET /            -> $($page.StatusCode), title ok: $($page.Content -match '<title>Feature Table</title>')"
$new = Invoke-RestMethod "$b/api/rows" -Method Post -ContentType "application/json" -Body '{"date":"2026-11-08","name":"Rafael","feature":"validation row"}'
$o += "POST /api/rows   -> id=$($new.id) name=$($new.name)"
$upd = Invoke-RestMethod "$b/api/rows/$($new.id)" -Method Put -ContentType "application/json" -Body '{"date":"2026-11-09","name":"Rafael","feature":"edited row"}'
$o += "PUT  /api/rows/$($new.id) -> date=$($upd.date) feature=$($upd.feature)"
$keep = Invoke-RestMethod "$b/api/rows" -Method Post -ContentType "application/json" -Body '{"date":"2026-11-08","name":"Sample","feature":"Inline editing"}'
Invoke-RestMethod "$b/api/rows/$($new.id)" -Method Delete | Out-Null
$o += "DELETE /api/rows/$($new.id) -> ok"
$all = Invoke-RestMethod "$b/api/rows"
$o += "GET /api/rows    -> " + ($all | ConvertTo-Json -Compress)
try { Invoke-RestMethod "$b/api/rows" -Method Post -ContentType "application/json" -Body '{"date":"bad","name":"x","feature":"y"}' | Out-Null; $o += "validation: NOT rejected" } catch { $o += "validation: bad date rejected (400)" }
$o += "health: " + (docker inspect --format "{{.State.Health.Status}} restart={{.HostConfig.RestartPolicy.Name}}" featuretable)
$o | Set-Content (Join-Path $PSScriptRoot "verify.log")

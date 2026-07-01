$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

Write-Host "`n=== New Conversation Detection Tool ===" -ForegroundColor Cyan
$Url = "http://127.0.0.1:8766"

try { $h = Invoke-RestMethod -Uri "$Url/healthz" -TimeoutSec 5; Write-Host "[OK] Service: $($h.status)" -ForegroundColor Green }
catch { Write-Host "[ERR] Service down" -ForegroundColor Red; exit 1 }

Write-Host "`n--- Test 1: Single message = NEW conversation ---" -ForegroundColor Yellow
$body1 = '{"model":"deepseek-chat","messages":[{"role":"user","content":"Mark: SINGLE_MSG_NEW"}]}'
try {
    $r1 = Invoke-RestMethod -Uri "$Url/v1/chat/completions" -Method Post `
        -Body ([System.Text.Encoding]::UTF8.GetBytes($body1)) `
        -ContentType "application/json; charset=utf-8" -TimeoutSec 60
    Write-Host "[OK] Reply: $($r1.choices[0].message.content.Substring(0, [Math]::Min(60, $r1.choices[0].message.content.Length)))..." -ForegroundColor Green
    Write-Host "[INFO] Single msg -> should be NEW window on browser" -ForegroundColor Gray
} catch { Write-Host "[ERR] $_" -ForegroundColor Red }

Start-Sleep 2

Write-Host "`n--- Test 2: Multi messages = CONTINUOUS chat ---" -ForegroundColor Yellow
$body2 = '{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"Mark: MULTI_MSG_SAME"}]}'
try {
    $r2 = Invoke-RestMethod -Uri "$Url/v1/chat/completions" -Method Post `
        -Body ([System.Text.Encoding]::UTF8.GetBytes($body2)) `
        -ContentType "application/json; charset=utf-8" -TimeoutSec 60
    Write-Host "[OK] Reply: $($r2.choices[0].message.content.Substring(0, [Math]::Min(60, $r2.choices[0].message.content.Length)))..." -ForegroundColor Green
    Write-Host "[INFO] Multi msgs -> should be SAME window on browser" -ForegroundColor Gray
} catch { Write-Host "[ERR] $_" -ForegroundColor Red }

Start-Sleep 2

Write-Host "`n--- Test 3: Single msg again = NEW conversation again ---" -ForegroundColor Yellow
$body3 = '{"model":"deepseek-chat","messages":[{"role":"user","content":"Mark: SINGLE_AGAIN_NEW"}]}'
try {
    $r3 = Invoke-RestMethod -Uri "$Url/v1/chat/completions" -Method Post `
        -Body ([System.Text.Encoding]::UTF8.GetBytes($body3)) `
        -ContentType "application/json; charset=utf-8" -TimeoutSec 60
    Write-Host "[OK] Reply: $($r3.choices[0].message.content.Substring(0, [Math]::Min(60, $r3.choices[0].message.content.Length)))..." -ForegroundColor Green
    Write-Host "[INFO] Single msg -> should be ANOTHER NEW window" -ForegroundColor Gray
} catch { Write-Host "[ERR] $_" -ForegroundColor Red }

Write-Host "`n=== Result ===" -ForegroundColor Cyan
Write-Host "Check browser:" -ForegroundColor White
Write-Host "  Test 1 -> NEW conversation window" -ForegroundColor Gray
Write-Host "  Test 2 -> SAME window as Test 1 (continuous)" -ForegroundColor Gray
Write-Host "  Test 3 -> NEW conversation window again" -ForegroundColor Gray
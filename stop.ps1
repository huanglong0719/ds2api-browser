Write-Host "Stopping ds2api-browser..." -ForegroundColor Yellow

# 停止编译后的 exe 或 go run 进程
$serverProc = Get-Process | Where-Object {
    ($_.CommandLine -match "ds2api-browser\.exe" -or $_.CommandLine -match "go.*run.*main\.go") -and $_.Id -ne $pid
} -ErrorAction SilentlyContinue

if ($serverProc) {
    Write-Host "[STOP] Closing API server (PID: $($serverProc.Id))" -ForegroundColor Yellow
    Stop-Process -Id $serverProc.Id -Force -ErrorAction SilentlyContinue
    Write-Host "[OK] Server stopped" -ForegroundColor Green
} else {
    Write-Host "[INFO] No running ds2api-browser process found" -ForegroundColor Gray
}

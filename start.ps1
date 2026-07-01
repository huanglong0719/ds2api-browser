$ErrorActionPreference = "Continue"
$ProjectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path

function Test-ServerRunning {
    try {
        $r = Invoke-WebRequest -Uri "http://127.0.0.1:8766/healthz" -TimeoutSec 2 -ErrorAction SilentlyContinue
        if ($r.StatusCode -eq 200) { return $true }
    } catch {}
    return $false
}

function Start-Server {
    Write-Host "[START] Launching ds2api-browser..." -ForegroundColor Yellow

    $exePath = Join-Path $ProjectRoot "ds2api-browser.exe"
    if (-not (Test-Path $exePath)) {
        Write-Host "[ERROR] ds2api-browser.exe not found, please run: go build -o ds2api-browser.exe ." -ForegroundColor Red
        return $false
    }

    # 启动服务，将 stdout 和 stderr 合并重定向到日志文件
    $logFile = Join-Path $ProjectRoot "ds2api-browser.log"
    # 通过 cmd /c 启动以合并 stdout 和 stderr 到一个文件
    Start-Process -FilePath "$env:windir\system32\cmd.exe" `
        -ArgumentList "/c", "`"$exePath`" > `"$logFile`" 2>&1" `
        -WindowStyle Hidden -PassThru

    Write-Host "[WAIT] Server starting..." -ForegroundColor Yellow
    Start-Sleep -Seconds 5

    for ($i = 0; $i -lt 20; $i++) {
        if (Test-ServerRunning) {
            Write-Host "[OK] API server ready: http://127.0.0.1:8766" -ForegroundColor Green
            return $true
        }
        Start-Sleep -Seconds 2
    }

    Write-Host "[ERROR] Server startup timeout. Check ds2api-browser.log for details." -ForegroundColor Red
    return $false
}

Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  ds2api-browser One-Click Start" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

# 关闭上次可能遗留的旧 Chrome 进程（新 exe 会自动处理，但确保环境干净）
$existing = Get-Process -Name "chrome" -ErrorAction SilentlyContinue
if ($existing) {
    Write-Host "[INFO] Closing existing Chrome instances..." -ForegroundColor Yellow
    Get-Process -Name "chrome" -ErrorAction SilentlyContinue | Where-Object {
        $_.CommandLine -match "remote-debugging-port=9222"
    } -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
}

if (Test-ServerRunning) {
    Write-Host "[OK] API server already running on port 8766" -ForegroundColor Green
} else {
    $started = Start-Server
    if (-not $started) {
        Write-Host "[FAIL] Startup failed" -ForegroundColor Red
        exit 1
    }
}

Write-Host ""
Write-Host "========================================" -ForegroundColor Green
Write-Host "  STARTUP COMPLETE!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host ""
Write-Host "  API:       http://127.0.0.1:8766/v1/chat/completions" -ForegroundColor White
Write-Host "  Health:    http://127.0.0.1:8766/healthz" -ForegroundColor White
Write-Host "  Log:       ds2api-browser.log" -ForegroundColor White
Write-Host ""
Write-Host "  Client config:" -ForegroundColor Yellow
Write-Host "    API Host:  http://127.0.0.1:8766" -ForegroundColor Gray
Write-Host "    API Key:  (set in browser_config.json)" -ForegroundColor Gray
Write-Host "    Model:    deepseek" -ForegroundColor Gray
Write-Host ""
Write-Host "To stop, run: .\stop.ps1" -ForegroundColor Cyan
Write-Host "To view log: Get-Content .\ds2api-browser.log -Tail 20" -ForegroundColor Cyan
Write-Host ""

while ($true) {
    Start-Sleep -Seconds 30
    if (-not (Test-ServerRunning)) {
        Write-Host "[WARN] Server stopped unexpectedly. Check ds2api-browser.log" -ForegroundColor Red
        break
    }
}

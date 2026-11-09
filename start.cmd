@echo off
cd /d "%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start.ps1" > "%~dp0start.log" 2>&1
echo exit code %ERRORLEVEL% >> "%~dp0start.log"
type "%~dp0start.log"
timeout /t 15

@echo off
cd /d "%~dp0"
docker build --target export --output type=local,dest=bin . > build-exe.log 2>&1
echo exit code %ERRORLEVEL% >> build-exe.log
type build-exe.log | findstr /C:"exit code" /C:"ERROR"

@echo off
rem hot-update demo setup: build v1.0.0 + v1.1.0, create agent_repo manifest, prepare agent_run dir
setlocal
set GOPATH=D:\codesoft\gopath
set GOMODCACHE=D:\codesoft\gomodcache
set GOCACHE=D:\codesoft\gocache
set PATH=D:\codesoft\go\bin;%PATH%

set AGENT_DIR=E:\Temp\printer-agent-go
set CLOUD_DIR=E:\Temp\cloudprinter-demo
set REPO_DIR=%CLOUD_DIR%\agent_repo

echo NOTE: paths inside this script point to the local demo layout, edit before reuse.

cd /d "%AGENT_DIR%"
if not exist "%REPO_DIR%\v1.0.0" mkdir "%REPO_DIR%\v1.0.0"
if not exist "%REPO_DIR%\v1.1.0" mkdir "%REPO_DIR%\v1.1.0"

echo [1/5] build v1.0.0 (old version, demo start point)...
powershell -NoProfile -Command "(Get-Content main.go -Raw -Encoding UTF8) -replace 'const AgentVersion = \"v[\d.]+\"','const AgentVersion = \"v1.0.0\"' | Set-Content main.go -NoNewline -Encoding UTF8"
if errorlevel 1 goto :fail
go build -trimpath -ldflags="-s -w" -o "%REPO_DIR%\v1.0.0\printer-agent.exe" .
if errorlevel 1 goto :fail

echo [2/5] restore v1.1.0 and build new version...
powershell -NoProfile -Command "(Get-Content main.go -Raw -Encoding UTF8) -replace 'const AgentVersion = \"v[\d.]+\"','const AgentVersion = \"v1.1.0\"' | Set-Content main.go -NoNewline -Encoding UTF8"
if errorlevel 1 goto :fail
go build -trimpath -ldflags="-s -w" -o printer-agent.exe .
if errorlevel 1 goto :fail
copy /Y printer-agent.exe "%REPO_DIR%\v1.1.0\printer-agent.exe" >nul
copy /Y app.html "%REPO_DIR%\v1.1.0\app.html" >nul

echo [3/5] generate manifest.json (sha256, UTF-8 without BOM)...
powershell -NoProfile -Command ^
 "$exe=(Get-FileHash '%REPO_DIR%\v1.1.0\printer-agent.exe' -Algorithm SHA256).Hash.ToLower();" ^
 "$page=(Get-FileHash '%REPO_DIR%\v1.1.0\app.html' -Algorithm SHA256).Hash.ToLower();" ^
 "$mf=[ordered]@{latest='v1.1.0';minRequired='v1.0.0';channel='stable';notes='demo release';files=[ordered]@{agent=[ordered]@{url='http://127.0.0.1:8000/agent_repo/v1.1.0/printer-agent.exe';sha256=$exe;size=(Get-Item '%REPO_DIR%\v1.1.0\printer-agent.exe').Length};page=[ordered]@{url='http://127.0.0.1:8000/agent_repo/v1.1.0/app.html';sha256=$page;size=(Get-Item '%REPO_DIR%\v1.1.0\app.html').Length}}};" ^
 "$utf8NoBom=New-Object System.Text.UTF8Encoding $false;" ^
 "[IO.File]::WriteAllText('%REPO_DIR%\manifest.json',(ConvertTo-Json -InputObject $mf -Depth 5),$utf8NoBom)"
if errorlevel 1 goto :fail

echo [4/5] prepare agent_run dir...
if exist "%AGENT_DIR%\agent_run" rmdir /s /q "%AGENT_DIR%\agent_run"
mkdir "%AGENT_DIR%\agent_run\files"
copy /Y "%REPO_DIR%\v1.0.0\printer-agent.exe" "%AGENT_DIR%\agent_run\printer-agent.exe" >nul
copy /Y "%AGENT_DIR%\files\sample-order.txt" "%AGENT_DIR%\agent_run\files\" >nul

echo [5/5] done. verify:
echo   1. serve %CLOUD_DIR% at http://127.0.0.1:8000
echo   2. run agent_run\printer-agent.exe with AGENT_UPDATE_INTERVAL_SEC=15
echo   3. watch log: download - verify - swap - restart; then GET /version
pause
exit /b 0

:fail
echo build failed
exit /b 1

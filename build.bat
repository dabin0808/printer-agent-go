@echo off
rem 构建 printer-agent.exe（绿色单文件，内嵌页面）
rem 用法: 双击 build.bat 或在项目目录执行
setlocal
set GOPATH=D:\codesoft\gopath
set GOMODCACHE=D:\codesoft\gomodcache
set GOCACHE=D:\codesoft\gocache
set PATH=D:\codesoft\go\bin;%PATH%

cd /d "%~dp0"
echo [1/2] 编译中...
go build -trimpath -ldflags="-s -w" -o printer-agent.exe .
if errorlevel 1 (
  echo 构建失败
  exit /b 1
)
echo [2/2] 完成: printer-agent.exe
dir printer-agent.exe | find "printer-agent"
endlocal

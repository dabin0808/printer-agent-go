@echo off
chcp 65001 >nul
title 云打印 PC 中转端 - 启动器
cd /d "%~dp0"

echo ============================================
echo   云打印 PC 中转端 启动器
echo ============================================
echo.

rem 检查 exe 是否存在
if not exist "printer-agent.exe" (
    echo [错误] 找不到 printer-agent.exe，请确认本脚本与 exe 在同一目录。
    echo.
    pause
    exit /b 1
)

rem 检查 18080 端口是否已被占用（服务是否已在运行）
netstat -ano | findstr ":18080" | findstr "LISTENING" >nul 2>&1
if %errorlevel% equ 0 (
    echo [提示] 打印服务已在运行，直接打开操作台...
    start "" "http://127.0.0.1:18080/"
    timeout /t 2 >nul
    exit /b 0
)

rem 启动服务（新控制台窗口，保持运行）
echo [1/2] 正在启动打印服务...
start "云打印中转端（请勿关闭此窗口）" /normal "printer-agent.exe"

rem 等待服务就绪（最多 10 秒）
echo [2/2] 等待服务就绪...
set /a n=0
:wait
set /a n+=1
timeout /t 1 >nul
curl -s -o nul -w "%%{http_code}" --max-time 2 "http://127.0.0.1:18080/version" 2>nul | findstr "200" >nul
if %errorlevel% equ 0 goto ready
if %n% LSS 10 goto wait

echo [警告] 服务启动超时，可能启动失败。请检查是否有杀毒软件拦截。
echo.
pause
exit /b 1

:ready
echo [成功] 打印服务已启动，正在打开操作台...
start "" "http://127.0.0.1:18080/"
echo.
echo 操作台地址: http://127.0.0.1:18080/
echo 关闭打印服务: 关闭标题为"云打印中转端"的控制台窗口即可
echo.
pause

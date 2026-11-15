# rkttl 构建脚本（在 Windows 上本地构建时使用）
# 需要 Go 1.22+：https://go.dev/dl/
@echo off
setlocal

where go >nul 2>nul
if errorlevel 1 (
    echo [错误] 未找到 go，请先安装 Go 1.22+
    exit /b 1
)

echo [1/3] 构建 rkttl.exe（命令行版，Linux/Windows 通用）...
go build -trimpath -ldflags "-s -w" -o rkttl.exe .\cmd\rkttl
if errorlevel 1 exit /b 1

echo [2/3] 生成 GUI 资源（清单 + 图标 -^> .syso）...
if not exist .\cmd\rkttl-gui\rsrc_windows_amd64.syso (
    if not exist rkttl.ico python tools\mkicon.py
    go run github.com/akavel/rsrc@v0.10.2 -manifest cmd\rkttl-gui\app.manifest -ico rkttl.ico -arch amd64 -o cmd\rkttl-gui\rsrc_windows_amd64.syso
)
if errorlevel 1 exit /b 1

echo [3/3] 构建 rkttl-gui.exe（图形界面版）...
go build -trimpath -ldflags "-s -w -H windowsgui" -o rkttl-gui.exe .\cmd\rkttl-gui
if errorlevel 1 exit /b 1

echo.
echo 构建完成:
echo   rkttl.exe      命令行版（serve / bridge / monitor / ports）
echo   rkttl-gui.exe  图形界面版
echo.
echo 快速上手:
echo   rkttl-gui.exe                        （推荐：图形界面，开箱即共享）
echo   rkttl.exe ports                      列出串口
echo   rkttl.exe serve -port COM3 -tcp 0.0.0.0:9999
endlocal

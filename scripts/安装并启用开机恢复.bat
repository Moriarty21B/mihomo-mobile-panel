@echo off
chcp 65001 >nul
cd /d "%~dp0"
if not exist "%~dp0mihomo-mobile-panel-installer.exe" (
  echo 请先下载并解压完整发行 ZIP，再运行这个文件。
  pause
  exit /b 1
)
echo 本次将为支持的固件启用面板和代理开机恢复。
"%~dp0mihomo-mobile-panel-installer.exe" --install-boot %*

@echo off
chcp 65001 >nul
cd /d "%~dp0"
if not exist "%~dp0mihomo-mobile-panel-installer.exe" (
  echo 请先下载并解压完整发行 ZIP，再运行这个文件。
  pause
  exit /b 1
)
"%~dp0mihomo-mobile-panel-installer.exe" --reset-password %*

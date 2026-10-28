@echo off
rem Builds dist\tidydesk.exe for Windows. Run "build.cmd all" to also build Linux and macOS binaries.
setlocal
cd /d "%~dp0"
set "GO=go"
where go >nul 2>nul || set "GO=C:\Program Files\Go\bin\go.exe"
if not exist dist mkdir dist
set CGO_ENABLED=0
set "VERSION=dev"
for /f %%v in ('git describe --tags --always 2^>nul') do set "VERSION=%%v"
set "FLAGS=-s -w -X main.version=%VERSION%"

set GOOS=windows
set GOARCH=amd64
"%GO%" build -trimpath -ldflags "%FLAGS%" -o dist\tidydesk.exe . || exit /b 1

if /i "%~1"=="all" (
  set GOOS=linux
  set GOARCH=amd64
  "%GO%" build -trimpath -ldflags "%FLAGS%" -o dist\tidydesk-linux-amd64 . || exit /b 1
  set GOOS=darwin
  set GOARCH=arm64
  "%GO%" build -trimpath -ldflags "%FLAGS%" -o dist\tidydesk-macos-arm64 . || exit /b 1
)
echo Built TidyDesk %VERSION%
dir /b dist

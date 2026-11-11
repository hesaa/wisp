@echo off
rem Builds the release wisp.exe at the repository root, from any working directory.
rem The built-in GIPHY key comes from WISP_GIPHY_KEY, else the first line of giphy.key at the
rem repository root; it is never printed. Exits with go build's exit code.
setlocal
pushd "%~dp0.." || exit /b 1
set "KEY=%WISP_GIPHY_KEY%"
if not defined KEY if exist giphy.key set /p KEY=<giphy.key
if not defined KEY echo build.bat: no GIPHY key found, building without one (a key can be set in Settings).
go build -trimpath -ldflags="-H windowsgui -s -w -X main.giphyKey=%KEY%" -o wisp.exe ./cmd/wisp
set ERR=%ERRORLEVEL%
popd
exit /b %ERR%

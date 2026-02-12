@echo off
setlocal

echo ========================================
echo QzoneWall-Go - Start Script (Windows)
echo ========================================

echo Go version:
go version
if errorlevel 1 goto :fail

echo Tidy modules...
go mod tidy
if errorlevel 1 goto :fail

echo Generate resources...
go generate ./...
if errorlevel 1 goto :fail

echo Build project...
go build -trimpath -ldflags="-s -w" -o wall.exe ./cmd/wall
if errorlevel 1 goto :fail

echo Run program...
wall.exe
goto :end

:fail
echo.
echo Build or run failed.

:end
pause
endlocal

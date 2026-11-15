@echo off
title QQMailDrive Compiler
echo ========================================================
echo   Building QQMailDrive (Pure Go Standard Library)
echo ========================================================
echo.

echo [1/3] Checking Go compiler environment...
go version >nul 2>&1
if %errorlevel% neq 0 (
    echo.
    echo [ERROR] Go compiler is NOT detected in your system!
    echo --------------------------------------------------------
    echo Please install Go from official website:
    echo https://golang.google.cn/dl/
    echo.
    echo After installing Go, please run build.bat again.
    echo --------------------------------------------------------
    echo.
    pause
    exit /b 1
)

echo [2/3] Checking Go module file...
if not exist go.mod (
    echo Initializing Go module...
    go mod init qqmaildrive >nul 2>&1
)

echo [3/3] Compiling QQMailDrive.exe ...
go build -ldflags="-s -w" -o QQMailDrive.exe .

if %errorlevel% neq 0 (
    echo.
    echo [ERROR] Build failed! Please check the error message above.
    pause
    exit /b 1
)

echo.
echo ========================================================
echo [SUCCESS] QQMailDrive.exe has been built successfully!
echo You can now double click QQMailDrive.exe to run.
echo ========================================================
echo.
pause

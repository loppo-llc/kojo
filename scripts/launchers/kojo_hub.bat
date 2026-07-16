@echo off
title kojo hub
cd /d "%~dp0"
echo Starting kojo (hub mode). Press Ctrl+C to stop.
"%~dp0kojo.exe" %*
pause

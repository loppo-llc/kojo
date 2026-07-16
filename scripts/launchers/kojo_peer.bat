@echo off
title kojo peer
cd /d "%~dp0"
echo Starting kojo (peer mode). Press Ctrl+C to stop.
"%~dp0kojo.exe" --peer %*
pause

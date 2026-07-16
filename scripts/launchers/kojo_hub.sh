#!/bin/sh
# Run kojo in hub mode from the directory this script lives in.
# Stays in the foreground until Ctrl+C.
dir="$(cd "$(dirname "$0")" && pwd)"
echo "Starting kojo (hub mode). Press Ctrl+C to stop."
exec "$dir/kojo" "$@"

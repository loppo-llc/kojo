#!/bin/sh
# Run kojo in peer mode from the directory this script lives in.
# Stays in the foreground until Ctrl+C.
dir="$(cd "$(dirname "$0")" && pwd)"
echo "Starting kojo (peer mode). Press Ctrl+C to stop."
exec "$dir/kojo" --peer "$@"

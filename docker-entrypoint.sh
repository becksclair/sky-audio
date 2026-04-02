#!/bin/bash
set -e

echo "=== sky-audio Docker ==="
echo "TTS_ENGINE: ${TTS_ENGINE:-pocket}"
echo "PORT: ${PORT:-8585}"
echo "API_KEY: ${SKY_AUDIO_API_KEY:+(set)}"

# Clean up stale sockets
rm -f /tmp/sky-audio-*.sock

# Start the gateway (it spawns workers internally based on TTS_ENGINE)
exec ./sky-audio

#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")"

echo "[1/4] Syncing local Pocket cloned voices..."
python3 workers/python-pocket-tts/sync_custom_voices.py

echo "[2/4] Building dashboard..."
cd dashboard
bun install --frozen-lockfile
bun run build
cd ..

echo "[3/4] Copying dashboard assets into Go embed..."
rm -rf gateway/pkg/dashboard/dist
cp -r dashboard/dist gateway/pkg/dashboard/dist

echo "[4/4] Building Go gateway..."
cd gateway
../scripts/with-onnx-env.sh go build -o sky-audio ./cmd/sky-audio

echo "Done. Binary: gateway/sky-audio"

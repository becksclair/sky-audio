#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")"

echo "[1/3] Building dashboard..."
cd dashboard
bun install --frozen-lockfile
bun run build
cd ..

echo "[2/3] Copying dashboard assets into Go embed..."
rm -rf gateway/pkg/dashboard/dist
cp -r dashboard/dist gateway/pkg/dashboard/dist

echo "[3/3] Building Go gateway..."
cd gateway
export CGO_LDFLAGS='-L/usr/local/lib/onnxruntime -lonnxruntime -Wl,-rpath,/usr/local/lib/onnxruntime'
export CGO_CFLAGS='-I/usr/local/include/onnxruntime'
export LD_LIBRARY_PATH="/usr/local/lib/onnxruntime:${LD_LIBRARY_PATH:-}"
go build -o sky-audio ./cmd/sky-audio

echo "Done. Binary: gateway/sky-audio"

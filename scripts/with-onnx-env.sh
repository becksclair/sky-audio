#!/usr/bin/env bash

set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

if [ -d /usr/local/include/onnxruntime ]; then
  ONNX_INCLUDE_DIR=/usr/local/include/onnxruntime
elif [ -d /usr/include/onnxruntime ]; then
  ONNX_INCLUDE_DIR=/usr/include/onnxruntime
else
  echo "missing ONNX Runtime headers (expected /usr/local/include/onnxruntime or /usr/include/onnxruntime)" >&2
  exit 1
fi

if [ -d "${ONNX_INCLUDE_DIR}" ]; then
  for header in "${ONNX_INCLUDE_DIR}"/*.h; do
    [ -e "$header" ] || continue
    target="/usr/local/include/$(basename "$header")"
    if [ ! -e "$target" ]; then
      ln -s "$header" "$target" 2>/dev/null || true
    fi
  done
fi

if [ -d /usr/local/lib/onnxruntime ]; then
  ONNX_LIB_DIR=/usr/local/lib/onnxruntime
elif [ -e /usr/lib/libonnxruntime.so ]; then
  ONNX_LIB_DIR=/usr/lib
else
  echo "missing ONNX Runtime library (expected /usr/local/lib/onnxruntime or /usr/lib/libonnxruntime.so)" >&2
  exit 1
fi

export CGO_CFLAGS="-I${ONNX_INCLUDE_DIR}${CGO_CFLAGS:+ ${CGO_CFLAGS}}"
export CGO_LDFLAGS="-L${ONNX_LIB_DIR} -lonnxruntime -Wl,-rpath,${ONNX_LIB_DIR}${CGO_LDFLAGS:+ ${CGO_LDFLAGS}}"
export LD_LIBRARY_PATH="${ONNX_LIB_DIR}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"

exec "$@"

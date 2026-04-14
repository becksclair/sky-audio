# sky-audio Docker image
# Packages the Go gateway + Python TTS workers (Pocket & Kyutai) + STT worker
# with Pocket cloned voices and model assets baked in at build time.
#
# Usage:
#   docker build -t sky-audio .
#   docker run -p 8585:8585 -e TTS_ENGINE=pocket sky-audio
#   docker run -p 8585:8585 -e TTS_ENGINE=kyutai --gpus all sky-audio
#
# Environment variables:
#   TTS_ENGINE    - "pocket" (default, CPU) or "kyutai" (GPU-capable)
#   PORT          - API port (default: 8585)
#   SKY_AUDIO_API_KEY - Optional API key for authentication

# ============================================================
# Stage 1: Build the Go gateway binary
# ============================================================
FROM golang:1.26-bookworm AS gateway-builder

# Install ONNX Runtime (needed for Silero VAD in the gateway)
RUN apt-get update && apt-get install -y --no-install-recommends \
    wget ca-certificates pkg-config libopus-dev libopusfile-dev && \
    wget -qL https://github.com/microsoft/onnxruntime/releases/download/v1.24.4/onnxruntime-linux-x64-1.24.4.tgz && \
    tar xzf onnxruntime-linux-x64-1.24.4.tgz && \
    mkdir -p /usr/local/lib/onnxruntime /usr/local/include/onnxruntime && \
    cp -r onnxruntime-linux-x64-1.24.4/lib/* /usr/local/lib/onnxruntime/ && \
    cp -r onnxruntime-linux-x64-1.24.4/include/* /usr/local/include/onnxruntime/ && \
    ldconfig && \
    rm -rf onnxruntime-linux-x64-1.24.4*

WORKDIR /src

# Copy go workspace files
COPY go.work go.work.sum ./
COPY gateway/go.mod gateway/go.sum ./gateway/

# Download Go dependencies
RUN cd gateway && go mod download

# Copy pre-built dashboard dist into gateway embed location
COPY dashboard/dist/ ./gateway/pkg/dashboard/dist/

# Copy gateway source
COPY gateway/ ./gateway/

# Build gateway binary
ENV CGO_LDFLAGS='-L/usr/local/lib/onnxruntime -lonnxruntime -Wl,-rpath,/usr/local/lib/onnxruntime'
ENV CGO_CFLAGS='-I/usr/local/include/onnxruntime'
RUN cd gateway && go build -o /sky-audio ./cmd/sky-audio

# ============================================================
# Stage 2: Build Python worker environments and bake model caches
# ============================================================
FROM debian:bookworm-slim AS worker-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    python3 \
    python3-dev \
    build-essential \
    ffmpeg \
    libopus0 \
    libopusfile0 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

ENV UV_LINK_MODE=copy
ENV UV_PYTHON_INSTALL_DIR=/opt/sky-audio-python
ENV HF_HOME=/opt/sky-audio-model-cache/huggingface
ENV HF_HUB_CACHE=/opt/sky-audio-model-cache/huggingface/hub
ENV HF_HUB_DISABLE_TELEMETRY=1
ENV HF_HUB_DISABLE_PROGRESS_BARS=1

WORKDIR /app

COPY proto/ ./proto/
COPY custom-voices/ ./custom-voices/
COPY workers/python-pocket-tts/ ./workers/python-pocket-tts/
COPY workers/python-kyutai-tts/ ./workers/python-kyutai-tts/
COPY workers/python-nemo/ ./workers/python-nemo/

RUN mkdir -p \
    /opt/sky-audio-pocket-runtime \
    /opt/sky-audio-gpu-runtime \
    /opt/sky-audio-model-cache/huggingface \
    /opt/sky-audio-python

RUN printf '%s\n' \
    '[project]' \
    'name = "sky-audio-pocket-runtime"' \
    'version = "0.1.0"' \
    'requires-python = ">=3.13"' \
    'dependencies = [' \
    '  "grpcio>=1.78.0",' \
    '  "huggingface-hub>=0.36.0",' \
    '  "numpy>=2.4.3",' \
    '  "pocket-tts",' \
    '  "protobuf>=5.29.6,<6",' \
    '  "torch>=2.5.0",' \
    ']' \
    '' \
    '[[tool.uv.index]]' \
    'name = "pytorch-cpu"' \
    'url = "https://download.pytorch.org/whl/cpu"' \
    'explicit = true' \
    '' \
    '[tool.uv.sources]' \
    'torch = { index = "pytorch-cpu" }' \
    > /opt/sky-audio-pocket-runtime/pyproject.toml

RUN printf '%s\n' \
    '[project]' \
    'name = "sky-audio-gpu-runtime"' \
    'version = "0.1.0"' \
    'requires-python = ">=3.13,<3.14"' \
    'dependencies = [' \
    '  "cuda-python>=12.3",' \
    '  "grpcio>=1.78.0",' \
    '  "huggingface-hub>=0.36.0",' \
    '  "moshi==0.2.13",' \
    '  "nemo-toolkit[asr]==2.7.2",' \
    '  "protobuf>=5.29.6,<6",' \
    ']' \
    > /opt/sky-audio-gpu-runtime/pyproject.toml

RUN set -eux; \
    cd /opt/sky-audio-pocket-runtime; \
    uv sync --python 3.13

RUN set -eux; \
    cd /opt/sky-audio-gpu-runtime; \
    uv sync --python 3.13

RUN rm -rf /app/workers/python-pocket-tts/.venv /app/workers/python-kyutai-tts/.venv /app/workers/python-nemo/.venv && \
    ln -s /opt/sky-audio-pocket-runtime/.venv /app/workers/python-pocket-tts/.venv && \
    ln -s /opt/sky-audio-gpu-runtime/.venv /app/workers/python-kyutai-tts/.venv && \
    ln -s /opt/sky-audio-gpu-runtime/.venv /app/workers/python-nemo/.venv

RUN set -eux; \
    cd /app/workers/python-pocket-tts; \
    python3 sync_custom_voices.py; \
    .venv/bin/python prefetch_model.py

RUN set -eux; \
    cd /app/workers/python-kyutai-tts; \
    .venv/bin/python prefetch_model.py

RUN set -eux; \
    cd /app/workers/python-nemo; \
    .venv/bin/python prefetch_model.py

RUN find /app/workers -type d \( -name '__pycache__' -o -name '.pytest_cache' \) -prune -exec rm -rf {} + && \
    find /opt/sky-audio-pocket-runtime /opt/sky-audio-gpu-runtime -type d -name '__pycache__' -prune -exec rm -rf {} + && \
    find /opt/sky-audio-pocket-runtime /opt/sky-audio-gpu-runtime -type f \( -name '*.pyc' -o -name '*.pyo' \) -delete && \
    find /opt/sky-audio-model-cache -type d \( -name 'logs' -o -name '.locks' \) -prune -exec rm -rf {} + && \
    rm -rf /opt/sky-audio-model-cache/huggingface/xet

# ============================================================
# Stage 3: Runtime image
# ============================================================
FROM debian:bookworm-slim AS runtime

# Install runtime dependencies only.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    gcc \
    libc6-dev \
    python3 \
    ffmpeg \
    libopus0 \
    libopusfile0 \
    && rm -rf /var/lib/apt/lists/*

# Install ONNX Runtime (runtime libs only)
COPY --from=gateway-builder /usr/local/lib/onnxruntime/ /usr/local/lib/onnxruntime/
RUN echo "/usr/local/lib/onnxruntime" > /etc/ld.so.conf.d/onnxruntime.conf && ldconfig

WORKDIR /app

# Copy the gateway binary and required runtime assets.
COPY --from=gateway-builder /sky-audio ./sky-audio
COPY gateway/models/ ./gateway/models/
COPY proto/ ./proto/
COPY --from=worker-builder /opt/sky-audio-pocket-runtime /opt/sky-audio-pocket-runtime
COPY --from=worker-builder /opt/sky-audio-gpu-runtime /opt/sky-audio-gpu-runtime
COPY --from=worker-builder /opt/sky-audio-model-cache /opt/sky-audio-model-cache
COPY --from=worker-builder /opt/sky-audio-python /opt/sky-audio-python
COPY --from=worker-builder /app/workers/ ./workers/

# Copy entrypoint
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

# Environment defaults
ENV TTS_ENGINE=pocket
ENV PORT=8585
ENV LD_LIBRARY_PATH=/usr/local/lib/onnxruntime
ENV UV_PYTHON_INSTALL_DIR=/opt/sky-audio-python
ENV HF_HOME=/opt/sky-audio-model-cache/huggingface
ENV HF_HUB_CACHE=/opt/sky-audio-model-cache/huggingface/hub
ENV HF_HUB_OFFLINE=1
ENV TRANSFORMERS_OFFLINE=1
ENV HF_HUB_DISABLE_TELEMETRY=1

EXPOSE 8585

ENTRYPOINT ["/docker-entrypoint.sh"]

# sky-audio Docker image
# Packages the Go gateway + Python TTS workers (Pocket & Kyutai) + voice assets
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
FROM golang:1.26-bookworm AS builder

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
# Stage 2: Runtime image
# ============================================================
FROM debian:bookworm-slim AS runtime

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    python3 \
    python3-pip \
    python3-dev \
    build-essential \
    curl \
    libopus0 \
    libopusfile0 \
    && rm -rf /var/lib/apt/lists/*

# Install ONNX Runtime (runtime libs only)
COPY --from=builder /usr/local/lib/onnxruntime/ /usr/local/lib/onnxruntime/
RUN echo "/usr/local/lib/onnxruntime" > /etc/ld.so.conf.d/onnxruntime.conf && ldconfig

# Install uv (Python package manager)
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
ENV PATH="/root/.local/bin:${PATH}"

# Set up working directory
WORKDIR /app

# Copy the gateway binary
COPY --from=builder /sky-audio ./sky-audio

# Copy Silero VAD model
COPY gateway/models/ ./gateway/models/

# Copy proto files (needed by workers)
COPY proto/ ./proto/

# ---- Pocket TTS worker ----
COPY workers/python-pocket-tts/ ./workers/python-pocket-tts/
RUN cd workers/python-pocket-tts && \
    uv sync --frozen 2>/dev/null || uv sync

# ---- Kyutai TTS worker ----
COPY workers/python-kyutai-tts/ ./workers/python-kyutai-tts/
RUN cd workers/python-kyutai-tts && \
    uv sync --frozen 2>/dev/null || uv sync

# ---- STT worker (Nemo) ----
COPY workers/python-nemo/ ./workers/python-nemo/
# Note: Nemo has heavy CUDA dependencies; skip sync if not needed
# Uncomment the next line to include STT support:
# RUN cd workers/python-nemo && uv sync --frozen 2>/dev/null || uv sync

# Copy entrypoint
COPY docker-entrypoint.sh /docker-entrypoint.sh
RUN chmod +x /docker-entrypoint.sh

# Environment defaults
ENV TTS_ENGINE=pocket
ENV PORT=8585
ENV LD_LIBRARY_PATH=/usr/local/lib/onnxruntime

EXPOSE 8585

ENTRYPOINT ["/docker-entrypoint.sh"]

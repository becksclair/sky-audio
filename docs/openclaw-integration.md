# OpenClaw Integration Guide

This guide explains how to use sky-audio as the TTS (Text-to-Speech) and STT (Speech-to-Text) engine for [OpenClaw](https://openclaw.ai), an open-source AI agent platform.

## Overview

sky-audio exposes OpenAI-compatible HTTP endpoints that OpenClaw can use for voice interactions:

- **TTS** - Convert OpenClaw's text replies to speech using local Kyutai or PocketTTS models
- **STT** - Transcribe voice messages sent to OpenClaw using local NeMo Parakeet

This gives OpenClaw fully local, self-hosted voice capabilities without requiring cloud API keys.

## Quick Setup

### 1. Start sky-audio

```bash
mise run dev
```

You can tune live STT end-of-speech behavior with `STT_END_OF_SPEECH_MS` before starting the gateway. Lower values make final transcripts return faster; higher values are more tolerant of pauses.

```bash
export STT_END_OF_SPEECH_MS=250
mise run dev
```

The gateway listens on `:8080` by default. Verify it's running:

```bash
curl http://localhost:8585/health
# Should return: ok
```

### 2. Configure OpenClaw

Edit your OpenClaw configuration file (usually `~/.openclaw/openclaw.json`):

```json
{
  "messages": {
    "tts": {
      "auto": "inbound",
      "provider": "openai",
      "openai": {
        "model": "sky-kyutai",
        "baseUrl": "http://localhost:8585/v1"
      },
      "maxTextLength": 4000,
      "timeoutMs": 60000
    }
  },
  "tools": {
    "media": {
      "audio": {
        "enabled": true,
        "maxBytes": 20971520,
        "models": [
          {
            "provider": "openai",
            "model": "whisper-1",
            "baseUrl": "http://localhost:8585/v1"
          }
        ]
      }
    }
  }
}
```

### 3. Restart OpenClaw

```bash
openclaw gateway restart
```

## Available Models

### TTS Models

| Model ID | Engine | Description | Best For |
|----------|--------|-------------|----------|
| `sky-kyutai` | Kyutai TTS 1.6B | High-quality neural TTS | Natural-sounding voice replies |
| `sky-pocket` | PocketTTS | Fast, lightweight TTS | Speed, lower resource usage |
| `tts-1` (alias) | PocketTTS | OpenAI-compatible alias | Compatibility with OpenAI examples |
| `tts-1-hd` (alias) | Kyutai | OpenAI-compatible alias | Higher quality OpenAI examples |
| `gpt-4o-mini-tts` (alias) | Kyutai | OpenAI-compatible alias | Latest OpenAI model name |

### STT Models

| Model ID | Engine | Description |
|----------|--------|-------------|
| `whisper-1` | NeMo Parakeet | Fast, accurate local transcription |
| `sky-nemo-parakeet` | NeMo Parakeet | Explicit sky-audio model name |

### Model Discovery

List all available models:

```bash
curl http://localhost:8585/v1/models
```

Response:
```json
{
  "object": "list",
  "data": [
    {"id": "sky-kyutai", "object": "model", "owned_by": "sky-audio"},
    {"id": "sky-pocket", "object": "model", "owned_by": "sky-audio"},
    {"id": "whisper-1", "object": "model", "owned_by": "sky-audio"}
  ]
}
```

## API Reference

### POST /v1/audio/speech

Synthesize text to speech.

**Request:**
```bash
curl -X POST http://localhost:8585/v1/audio/speech \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sky-kyutai",
    "input": "Hello, this is sky-audio speaking!",
    "voice": "default"
  }' \
  --output speech.wav
```

**Parameters:**
- `model` (required) - One of the TTS model IDs above
- `input` (required) - Text to synthesize (max 4096 characters)
- `voice` (optional) - Voice ID (specific to engine)
- `response_format` (optional) - Output format: `wav`, `pcm` (default: `wav`)
- `speed` (optional) - Ignored for compatibility

**Response:**
- `audio/wav` - WAV audio file (16-bit PCM, mono, 16-24kHz depending on engine)

**Response Formats:**
- `wav` - WAV container with PCM audio (default, always supported)
- `pcm` - Raw PCM16 data with `audio/pcm; rate=16000` content type
- `mp3`, `opus` - **Not supported** - Returns HTTP 501 with error message

### POST /v1/audio/transcriptions

Transcribe audio to text.

**Request:**
```bash
curl -X POST http://localhost:8585/v1/audio/transcriptions \
  -F "file=@recording.wav" \
  -F "model=whisper-1" \
  -F "language=en"
```

**Parameters:**
- `file` (required) - Audio file (multipart/form-data)
- `model` (required) - STT model ID
- `language` (optional) - Language hint (e.g., `en`, `fr`, `de`)
- `response_format` (optional) - `json`, `text`, `verbose_json`, `srt`, `vtt` (default: `json`)
- `stream` (optional) - `true` to receive partial/final transcript events over SSE
- `prompt` (optional) - Ignored for compatibility
- `temperature` (optional) - Ignored for compatibility

**Supported Audio Formats:**
- WAV (16-bit PCM, required)

Note: MP3 and OGG/Opus are not supported. Please convert to WAV before uploading.

**Response (json format):**
```json
{
  "text": "This is the transcribed text from the audio."
}
```

**Response (text format):**
```
This is the transcribed text from the audio.
```

**Streaming response (`stream=true`):**
```text
data: {"text":"hello","isFinal":false}

data: {"text":"hello world","isFinal":true}

data: [DONE]
```

### GET /v1/models

List available models in OpenAI-compatible format.

**Request:**
```bash
curl http://localhost:8585/v1/models
```

**Response:**
```json
{
  "object": "list",
  "data": [
    {
      "id": "sky-kyutai",
      "object": "model",
      "created": 1740787200,
      "owned_by": "sky-audio"
    }
  ]
}
```

## Authentication (Optional)

By default, the OpenAI-compatible endpoints don't require authentication. To enable API key protection:

```bash
export SKY_AUDIO_API_KEY="your-secret-key-here"
mise run dev
```

Then configure OpenClaw with the key:

```json
{
  "messages": {
    "tts": {
      "openai": {
        "baseUrl": "http://localhost:8585/v1",
        "apiKey": "your-secret-key-here"
      }
    }
  }
}
```

The gateway expects `Authorization: Bearer <token>` headers when `SKY_AUDIO_API_KEY` is set.

## Advanced Configuration

### Voice Selection

The `voice` parameter in TTS requests is passed through to the worker. If no voice is specified, the worker uses its default voice.

**Default Voices:**
- **PocketTTS:** `alba` (clear and natural)
- **Kyutai TTS:** `expresso/ex03-ex01_happy_001_channel1_334s.wav` (expressive, high quality)

**Request with specific voice:**
```bash
# PocketTTS - use the 'george' voice
curl -X POST http://localhost:8585/v1/audio/speech \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sky-pocket",
    "input": "Hello with a specific voice",
    "voice": "george"
  }' \
  --output speech.wav

# Kyutai TTS - use a specific voice file
curl -X POST http://localhost:8585/v1/audio/speech \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sky-kyutai",
    "input": "Hello with a different voice",
    "voice": "alba-mackenna/a-moment-by.wav"
  }' \
  --output speech.wav
```

**Available Voices:**

**PocketTTS:**
Uses voice assets from `workers/python-pocket-tts/voices/`. Check `workers/python-pocket-tts/voices.json` for the full catalog.

Popular voices:
- `alba` (default) - Clear, natural voice
- `anna` - Natural voice
- `george` - Professional voice
- `vera` - Warm voice
- `peter_yearsley` - British voice

**Kyutai TTS:**
Uses voice assets from `workers/python-kyutai-tts/voices/`. Check `workers/python-kyutai-tts/voices.json` for 900+ voices.

Voice categories include:
- `expresso/` - Expressive, emotional voices
- `alba-mackenna/` - Professional narrator voices
- `voice-donations/` - Community-contributed voices
- `cml-tts/fr/` - French language voices

**OpenClaw Configuration with Voice:**
```json
{
  "messages": {
    "tts": {
      "provider": "openai",
      "openai": {
        "model": "sky-pocket",
        "baseUrl": "http://localhost:8585/v1",
        "voice": "george"
      }
    }
  }
}
```

Note: OpenClaw may not expose voice selection in all interfaces. The default voices (alba for Pocket, expresso for Kyutai) are chosen for quality and naturalness.

### TTS Auto-Trigger Modes

OpenClaw supports several TTS trigger modes via `messages.tts.auto`:

| Mode | Behavior |
|------|----------|
| `"off"` | Never auto-trigger TTS |
| `"inbound"` | TTS only for replies to voice messages |
| `"all"` | TTS on all replies |

Example:
```json
{
  "messages": {
    "tts": {
      "auto": "inbound",
      "provider": "openai",
      "openai": {
        "model": "sky-kyutai",
        "baseUrl": "http://localhost:8585/v1"
      }
    }
  }
}
```

### STT Model Chain

OpenClaw supports fallback chains for STT. If the first model fails, it tries the next:

```json
{
  "tools": {
    "media": {
      "audio": {
        "enabled": true,
        "models": [
          {"provider": "openai", "model": "whisper-1", "baseUrl": "http://localhost:8585/v1"},
          {"type": "cli", "command": "whisper", "args": ["--model", "base", "{{MediaPath}}"]}
        ]
      }
    }
  }
}
```

## Troubleshooting

### TTS Returns Text Instead of Voice

Check OpenClaw's TTS configuration:
- Verify `messages.tts.provider` is set to `"openai"`
- Ensure `baseUrl` points to the correct sky-audio address
- Check that `auto` isn't set to `"off"`
- Verify text length doesn't exceed `maxTextLength`

### STT Not Transcribing Voice Messages

- Verify `tools.media.audio.enabled` is `true`
- Check that the model provider is set to `"openai"` with the correct `baseUrl`
- Ensure audio file size is under `maxBytes` (default 20MB)
- Check sky-audio logs for STT worker errors

### Connection Refused

- Verify sky-audio is running: `curl http://localhost:8585/health`
- Check that OpenClaw's `baseUrl` includes the correct port (default `8080`)
- If sky-audio is on a different host, use the appropriate IP/hostname

### Audio Quality Issues

**TTS sounds robotic:**
- Use `sky-kyutai` instead of `sky-pocket` for higher quality

**STT has poor accuracy:**
- Ensure audio is clear and at appropriate volume
- Check that input audio is 16kHz mono for best results
- The NeMo Parakeet model works best with English audio

## Architecture

The OpenAI-compatible layer is implemented in `gateway/pkg/openaihttp/`:

```
┌─────────────────────────────────────────────────────────────┐
│                        OpenClaw                              │
│  (Configured with baseUrl: http://sky-audio:8080/v1)        │
└──────────────────────┬──────────────────────────────────────┘
                       │ POST /v1/audio/speech
                       │ POST /v1/audio/transcriptions
                       │ GET /v1/models
                       ▼
┌─────────────────────────────────────────────────────────────┐
│              sky-audio Gateway (Go)                          │
│  ┌──────────────────────────────────────────────────────┐  │
│  │  openaihttp adapter (OpenAI-compatible HTTP)         │  │
│  │  - /v1/audio/speech      → TTSClient.Synthesize()    │  │
│  │  - /v1/audio/transcribe  → STTClient.Transcribe()    │  │
│  │  - /v1/models            → Static model catalog      │  │
│  └──────────────────────────────────────────────────────┘  │
└──────────────────────┬──────────────────────────────────────┘
                       │ gRPC over Unix sockets
                       ▼
┌─────────────────────────────────────────────────────────────┐
│                     Python Workers                           │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Kyutai TTS   │  │ PocketTTS    │  │ NeMo STT     │      │
│  │ (1.6B)       │  │ (Fast)       │  │ (Parakeet)   │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
└─────────────────────────────────────────────────────────────┘
```

## References

- [OpenClaw Voice Documentation](https://docs.openclaw.ai/features/voice)
- [OpenClaw SAG (Speech-Audio Gateway)](https://docs.openclaw.ai/features/sag)
- [OpenAI TTS API Reference](https://platform.openai.com/docs/guides/text-to-speech)
- [OpenAI STT API Reference](https://platform.openai.com/docs/guides/speech-to-text)
- [sky-audio source: gateway/pkg/openaihttp/](../gateway/pkg/openaihttp/)

## Testing

### Manual Testing

Test the endpoints with curl after starting the gateway:

```bash
# 1. Start the gateway
mise run dev

# 2. Test TTS in another terminal
curl -X POST http://localhost:8585/v1/audio/speech \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sky-kyutai",
    "input": "Hello from sky-audio!",
    "voice": "default"
  }' \
  --output /tmp/test-speech.wav

# Play the result (if you have a player installed)
ffplay /tmp/test-speech.wav

# 3. Test models endpoint
curl http://localhost:8585/v1/models | jq

# 4. Test STT (requires a WAV file)
curl -X POST http://localhost:8585/v1/audio/transcriptions \
  -F "file=@/tmp/test-speech.wav" \
  -F "model=whisper-1" \
  -F "response_format=json" | jq
```

### Automated E2E Tests

The `gateway/e2e/openai_test.go` file contains end-to-end tests that verify the endpoints with actual workers:

```bash
# Run all E2E tests (requires workers to be running)
mise run test:e2e

# Or run specific OpenAI endpoint tests
cd gateway
go test -v ./e2e/ -run TestOpenAI
```

E2E tests check:
- TTS synthesis with multiple engines (PocketTTS, Kyutai)
- STT transcription with WAV input
- Model listing and retrieval
- Authentication middleware
- Error handling (invalid requests, oversized input)

**Note:** E2E tests are skipped if workers aren't running. They auto-detect socket paths at `/tmp/sky-audio-*.sock`.

### Unit Tests

The `gateway/pkg/openaihttp/openaihttp_test.go` contains fast unit tests using mock interfaces:

```bash
cd gateway
go test -v ./pkg/openaihttp/
```

These tests verify request/response handling without requiring actual workers.

## Contributing

To extend the OpenAI-compatible layer:

1. Add new endpoints in `gateway/pkg/openaihttp/`
2. Follow OpenAI API request/response formats for compatibility
3. Map to existing internal gRPC services when possible
4. Add unit tests in `openaihttp_test.go`
5. Add E2E tests in `gateway/e2e/openai_test.go` for full-stack verification
6. Update this documentation

#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
GATEWAY_DIR="$ROOT/gateway"
FIXTURE_PATH="${1:-$ROOT/sky_voice_test.wav}"
PORT="${PORT:-8688}"
GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:${PORT}}"
API_KEY="${SKY_AUDIO_API_KEY:-smoke-key}"
STT_DEVICE="${STT_DEVICE:-auto}"
LOG_DIR=$(mktemp -d /tmp/sky-audio-realtime-ws-smoke.XXXXXX)
STT_SOCKET_PATH="${STT_SOCKET_PATH:-$LOG_DIR/sky-audio-stt.sock}"
FIXTURE_WAV="$LOG_DIR/fixture.wav"
WS_CLIENT="$LOG_DIR/realtime_ws_smoke_client.go"
GATEWAY_LOG="$LOG_DIR/gateway.log"
GATEWAY_BIN="$LOG_DIR/sky-audio"
STARTED_GATEWAY=0
GATEWAY_PID=""

cleanup() {
  set +e
  if [[ "$STARTED_GATEWAY" == "1" && -n "$GATEWAY_PID" ]]; then
    kill "$GATEWAY_PID" 2>/dev/null || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  rm -rf "$LOG_DIR"
}
trap cleanup EXIT

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

need_cmd curl
need_cmd ffmpeg
need_cmd go
need_cmd python3

if [[ ! -f "$FIXTURE_PATH" ]]; then
  echo "fixture not found: $FIXTURE_PATH" >&2
  exit 1
fi

ffmpeg -nostdin -v error -i "$FIXTURE_PATH" -ar 16000 -ac 1 -c:a pcm_s16le -f wav "$FIXTURE_WAV" -y >/dev/null

cat > "$WS_CLIENT" <<'GO'
package main

import (
  "context"
  "encoding/base64"
  "encoding/json"
  "fmt"
  "os"
  "strings"
  "time"

  "github.com/coder/websocket"
  "github.com/coder/websocket/wsjson"
  "github.com/heliasar/sky-audio/gateway/pkg/audio"
)

func main() {
  if len(os.Args) != 4 {
    panic("usage: realtime_ws_smoke_client <ws_url> <ephemeral_token> <fixture_wav>")
  }
  wsURL, token, wavPath := os.Args[1], os.Args[2], os.Args[3]
  wav, err := os.ReadFile(wavPath)
  if err != nil {
    panic(err)
  }
  pcm, sampleRate, err := audio.DecodeWAV(wav)
  if err != nil {
    panic(err)
  }
  if sampleRate != audio.TargetSampleRate {
    panic(fmt.Sprintf("fixture sample rate %d", sampleRate))
  }

  ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
  defer cancel()
  conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"realtime", "openai-insecure-api-key." + token, "openai-beta.realtime-v1"}})
  if err != nil {
    panic(err)
  }
  defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

  var event map[string]any
  if err := wsjson.Read(ctx, conn, &event); err != nil {
    panic(err)
  }
  if err := wsjson.Write(ctx, conn, map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription"}}); err != nil {
    panic(err)
  }
  if err := wsjson.Read(ctx, conn, &event); err != nil {
    panic(err)
  }

  const chunkSize = 3200
  for offset := 0; offset < len(pcm); offset += chunkSize {
    end := offset + chunkSize
    if end > len(pcm) {
      end = len(pcm)
    }
    if err := wsjson.Write(ctx, conn, map[string]any{
      "type":  "input_audio_buffer.append",
      "audio": base64.StdEncoding.EncodeToString(pcm[offset:end]),
    }); err != nil {
      panic(err)
    }
  }
  if err := wsjson.Write(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
    panic(err)
  }

  itemID := ""
  partials := 0
  for {
    if err := wsjson.Read(ctx, conn, &event); err != nil {
      panic(err)
    }
    switch event["type"] {
    case "input_audio_buffer.committed":
      itemID, _ = event["item_id"].(string)
    case "conversation.item.input_audio_transcription.delta":
      gotItemID, _ := event["item_id"].(string)
      if gotItemID == itemID && strings.TrimSpace(fmt.Sprint(event["delta"])) != "" {
        partials++
      }
    case "conversation.item.input_audio_transcription.completed":
      gotItemID, _ := event["item_id"].(string)
      transcript, _ := event["transcript"].(string)
      out, _ := json.Marshal(map[string]any{
        "transport": "websocket",
        "item_id":   gotItemID,
        "transcript": transcript,
        "partials":  partials,
      })
      fmt.Println(string(out))
      return
    case "error":
      body, _ := json.Marshal(event)
      panic(string(body))
    }
  }
}
GO

if curl -fsS "$GATEWAY_URL/health" >/dev/null 2>&1; then
  echo "reusing existing gateway at $GATEWAY_URL"
  if ! curl -fsS -H "Authorization: Bearer $API_KEY" "$GATEWAY_URL/v1/models" >/dev/null 2>&1; then
    echo "existing gateway is healthy but rejected SKY_AUDIO_API_KEY; export the correct key before reusing $GATEWAY_URL" >&2
    exit 1
  fi
else
  if [[ -S "$STT_SOCKET_PATH" ]]; then
    echo "refusing to start a new gateway while $STT_SOCKET_PATH already exists; stop the existing worker or point GATEWAY_URL at the running gateway" >&2
    exit 1
  fi
  echo "building gateway/sky-audio..."
  "$ROOT/scripts/with-onnx-env.sh" go build -o "$GATEWAY_BIN" ./gateway/cmd/sky-audio
  echo "starting gateway at $GATEWAY_URL with STT_DEVICE=$STT_DEVICE STT_SOCKET_PATH=$STT_SOCKET_PATH"
  (
    cd "$ROOT"
    SKY_AUDIO_API_KEY="$API_KEY" SKY_AUDIO_STT_SOCKET="$STT_SOCKET_PATH" STT_DEVICE="$STT_DEVICE" PORT="$PORT" "$GATEWAY_BIN" >"$GATEWAY_LOG" 2>&1
  ) &
  GATEWAY_PID=$!
  STARTED_GATEWAY=1
  for i in $(seq 1 180); do
    HEALTH=0
    SOCKET=0
    curl -fsS "$GATEWAY_URL/health" >/dev/null 2>&1 && HEALTH=1
    [[ -S "$STT_SOCKET_PATH" ]] && SOCKET=1
    if [[ "$HEALTH" == "1" && "$SOCKET" == "1" ]]; then
      break
    fi
    sleep 1
    if [[ "$i" == 180 ]]; then
      echo "gateway/socket readiness failed" >&2
      tail -n 220 "$GATEWAY_LOG" >&2 || true
      exit 1
    fi
  done
fi

WS_URL=$(python3 - <<'PY' "$GATEWAY_URL"
import sys
url = sys.argv[1]
if url.startswith('https://'):
    print('wss://' + url[len('https://'):])
elif url.startswith('http://'):
    print('ws://' + url[len('http://'):])
else:
    raise SystemExit('unsupported GATEWAY_URL: ' + url)
PY
)
WS_URL="$WS_URL/v1/realtime"
CLIENT_SECRET_JSON=$(curl -fsS -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' -d '{"session":{"type":"transcription"}}' "$GATEWAY_URL/v1/realtime/client_secrets")
EPHEMERAL_TOKEN=$(python3 - <<'PY' "$CLIENT_SECRET_JSON"
import json, sys
print(json.loads(sys.argv[1])["value"])
PY
)
BATCH_JSON=$(curl -fsS -H "Authorization: Bearer $API_KEY" -F "file=@$FIXTURE_WAV;type=audio/wav" -F 'model=whisper-1' "$GATEWAY_URL/v1/audio/transcriptions")
WS_JSON=$(cd "$GATEWAY_DIR" && go run "$WS_CLIENT" "$WS_URL" "$EPHEMERAL_TOKEN" "$FIXTURE_WAV")

python3 - <<'PY' "$BATCH_JSON" "$WS_JSON"
import json, re, sys
batch = json.loads(sys.argv[1])
ws = json.loads(sys.argv[2])
batch_text = (batch.get('text') or '').strip()
ws_text = (ws.get('transcript') or '').strip()

def tokens(text):
    return {token for token in re.findall(r"[a-z0-9']+", text.lower()) if len(token) >= 3}

shared = sorted(tokens(batch_text) & tokens(ws_text))
print(json.dumps({"batch": batch_text, "websocket": ws_text, "partials": ws.get("partials", 0), "shared_tokens": shared[:12], "shared_token_count": len(shared)}))
if not batch_text:
    raise SystemExit('batch transcript empty')
if not ws_text:
    raise SystemExit('websocket transcript empty')
if int(ws.get('partials', 0)) <= 0:
    raise SystemExit('websocket observed no partial transcripts')
if not shared:
    raise SystemExit('batch and websocket transcripts share no meaningful tokens')
PY

if command -v nvidia-smi >/dev/null 2>&1; then
  echo '--- vram ---'
  nvidia-smi --query-gpu=index,memory.used,memory.free --format=csv,noheader,nounits || true
fi

if [[ "$STARTED_GATEWAY" == "1" ]]; then
  echo '--- gateway log tail ---'
  tail -n 120 "$GATEWAY_LOG"
fi

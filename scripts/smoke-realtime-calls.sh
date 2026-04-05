#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
GATEWAY_DIR="$ROOT/gateway"
FIXTURE_PATH="${1:-$ROOT/sky_voice_test.wav}"
PORT="${PORT:-8688}"
GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:${PORT}}"
API_KEY="${SKY_AUDIO_API_KEY:-smoke-key}"
STT_DEVICE="${STT_DEVICE:-auto}"
LOG_DIR=$(mktemp -d /tmp/sky-audio-realtime-calls-smoke.XXXXXX)
FIXTURE_WAV="$LOG_DIR/fixture.wav"
CALLS_CLIENT="$LOG_DIR/realtime_calls_smoke_client.go"
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

cat > "$CALLS_CLIENT" <<'GO'
package main

import (
  "encoding/json"
  "fmt"
  "io"
  "net/http"
  "os"
  "strings"
  "time"

  "github.com/heliasar/sky-audio/gateway/pkg/audio"
  "github.com/pion/rtp"
  "github.com/pion/webrtc/v4"
)

func main() {
  if len(os.Args) != 4 {
    panic("usage: realtime_calls_smoke_client <server_url> <api_key> <fixture_wav>")
  }
  serverURL, apiKey, wavPath := os.Args[1], os.Args[2], os.Args[3]
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

  pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
  if err != nil {
    panic(err)
  }
  defer func() { _ = pc.Close() }()

  track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "sky-smoke")
  if err != nil {
    panic(err)
  }
  if _, err := pc.AddTrack(track); err != nil {
    panic(err)
  }
  if _, err := pc.CreateDataChannel("client-probe", nil); err != nil {
    panic(err)
  }

  connected := make(chan struct{})
  pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
    if state == webrtc.ICEConnectionStateConnected {
      select {
      case <-connected:
      default:
        close(connected)
      }
    }
  })

  events := make(chan map[string]any, 256)
  pc.OnDataChannel(func(dc *webrtc.DataChannel) {
    if dc.Label() != "oai-events" {
      return
    }
    dc.OnMessage(func(msg webrtc.DataChannelMessage) {
      var event map[string]any
      if err := json.Unmarshal(msg.Data, &event); err != nil {
        fmt.Fprintf(os.Stderr, "[smoke] unmarshal datachannel event: %v\n", err)
        return
      }
      events <- event
    })
  })

  offer, err := pc.CreateOffer(nil)
  if err != nil {
    panic(err)
  }
  if err := pc.SetLocalDescription(offer); err != nil {
    panic(err)
  }
  <-webrtc.GatheringCompletePromise(pc)
  if pc.LocalDescription() == nil || strings.TrimSpace(pc.LocalDescription().SDP) == "" {
    panic("empty local SDP offer")
  }

  req, err := http.NewRequest(http.MethodPost, serverURL, strings.NewReader(pc.LocalDescription().SDP))
  if err != nil {
    panic(err)
  }
  req.Header.Set("Content-Type", "application/sdp")
  req.Header.Set("Authorization", "Bearer "+apiKey)
  resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
  if err != nil {
    panic(err)
  }
  defer resp.Body.Close()
  answerBody, err := io.ReadAll(resp.Body)
  if err != nil {
    panic(err)
  }
  if resp.StatusCode != http.StatusCreated {
    panic(fmt.Sprintf("status=%d body=%s", resp.StatusCode, string(answerBody)))
  }
  if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answerBody)}); err != nil {
    panic(err)
  }

  select {
  case <-connected:
  case <-time.After(20 * time.Second):
    panic("timed out waiting for ICE connection")
  }

  enc, err := audio.NewOpusEncoder()
  if err != nil {
    panic(err)
  }
  frameSize := audio.EncodeSamples * 2
  sequenceNumber := uint16(0)
  timestamp := uint32(0)
  sendFrame := func(framePCM []byte) {
    opusFrame, err := enc.Encode(framePCM)
    if err != nil {
      panic(err)
    }
    if err := track.WriteRTP(&rtp.Packet{
      Header: rtp.Header{
        Version:        2,
        PayloadType:    111,
        SequenceNumber: sequenceNumber,
        Timestamp:      timestamp,
        SSRC:           12345,
      },
      Payload: opusFrame,
    }); err != nil {
      panic(err)
    }
    sequenceNumber++
    timestamp += 960
    time.Sleep(5 * time.Millisecond)
  }

  for offset := 0; offset < len(pcm); offset += frameSize {
    end := offset + frameSize
    frame := make([]byte, frameSize)
    if end > len(pcm) {
      copy(frame, pcm[offset:])
    } else {
      copy(frame, pcm[offset:end])
    }
    sendFrame(frame)
  }
  silence := make([]byte, frameSize)
  for i := 0; i < 20; i++ {
    sendFrame(silence)
  }

  deadline := time.After(90 * time.Second)
  itemID := ""
  partials := 0
  for {
    select {
    case event := <-events:
      switch event["type"] {
      case "session.created":
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
          "transport": "webrtc-calls",
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
    case <-deadline:
      panic("timed out waiting for call transcript")
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
  if [[ -S /tmp/sky-audio-stt.sock ]]; then
    echo "refusing to start a new gateway while /tmp/sky-audio-stt.sock already exists; stop the existing worker or point GATEWAY_URL at the running gateway" >&2
    exit 1
  fi
  echo "building gateway/sky-audio..."
  "$ROOT/scripts/with-onnx-env.sh" go build -o "$GATEWAY_BIN" ./gateway/cmd/sky-audio
  echo "starting gateway at $GATEWAY_URL with STT_DEVICE=$STT_DEVICE"
  (
    cd "$ROOT"
    SKY_AUDIO_API_KEY="$API_KEY" STT_DEVICE="$STT_DEVICE" PORT="$PORT" "$GATEWAY_BIN" >"$GATEWAY_LOG" 2>&1
  ) &
  GATEWAY_PID=$!
  STARTED_GATEWAY=1
  for i in $(seq 1 180); do
    HEALTH=0
    SOCKET=0
    curl -fsS "$GATEWAY_URL/health" >/dev/null 2>&1 && HEALTH=1
    [[ -S /tmp/sky-audio-stt.sock ]] && SOCKET=1
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

BATCH_JSON=$(curl -fsS -H "Authorization: Bearer $API_KEY" -F "file=@$FIXTURE_WAV;type=audio/wav" -F 'model=whisper-1' "$GATEWAY_URL/v1/audio/transcriptions")
CALLS_JSON=$(cd "$GATEWAY_DIR" && go run "$CALLS_CLIENT" "$GATEWAY_URL/v1/realtime/calls" "$API_KEY" "$FIXTURE_WAV")

python3 - <<'PY' "$BATCH_JSON" "$CALLS_JSON"
import json, re, sys
batch = json.loads(sys.argv[1])
calls = json.loads(sys.argv[2])
batch_text = (batch.get('text') or '').strip()
calls_text = (calls.get('transcript') or '').strip()

def tokens(text):
    return {token for token in re.findall(r"[a-z0-9']+", text.lower()) if len(token) >= 3}

shared = sorted(tokens(batch_text) & tokens(calls_text))
print(json.dumps({"batch": batch_text, "calls": calls_text, "partials": calls.get("partials", 0), "shared_tokens": shared[:12], "shared_token_count": len(shared)}))
if not batch_text:
    raise SystemExit('batch transcript empty')
if not calls_text:
    raise SystemExit('calls transcript empty')
if int(calls.get('partials', 0)) <= 0:
    raise SystemExit('calls observed no partial transcripts')
if not shared:
    raise SystemExit('batch and calls transcripts share no meaningful tokens')
PY

if command -v nvidia-smi >/dev/null 2>&1; then
  echo '--- vram ---'
  nvidia-smi --query-gpu=index,memory.used,memory.free --format=csv,noheader,nounits || true
fi

if [[ "$STARTED_GATEWAY" == "1" ]]; then
  echo '--- gateway log tail ---'
  tail -n 120 "$GATEWAY_LOG"
fi

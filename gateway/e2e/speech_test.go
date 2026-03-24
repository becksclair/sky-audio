package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

// TestSTTWorkerRealSpeech sends a real speech WAV file to the STT worker
// and verifies a non-empty transcript is returned.
// Requires: mise run worker:stt running in background.
// Requires: /tmp/test-speech.wav (download with: wget -O /tmp/test-speech.wav https://dldata-public.s3.us-east-2.amazonaws.com/2086-149220-0033.wav)
func TestSTTWorkerRealSpeech(t *testing.T) {
	const socketPath = "/tmp/sky-audio-stt.sock"
	const wavPath = "/tmp/test-speech.wav"

	wavData, err := os.ReadFile(wavPath)
	if err != nil {
		t.Skipf("Test WAV not found (download it first): %v", err)
	}

	// Strip the 44-byte WAV header to get raw PCM
	if len(wavData) < 44 {
		t.Fatal("WAV file too small")
	}
	pcm := wavData[44:]
	t.Logf("PCM: %d bytes (%.2fs at 16kHz 16-bit mono)", len(pcm), float64(len(pcm))/(16000*2))

	client, err := skywebrtc.NewSTTClient(socketPath)
	if err != nil {
		t.Skipf("STT worker not available: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.StartStream(ctx); err != nil {
		t.Skipf("STT worker not accepting streams: %v", err)
	}

	// Send the PCM in chunks (simulating streaming), then EOS
	const chunkSize = 32000 // 1 second chunks
	for i := 0; i < len(pcm); i += chunkSize {
		end := min(i+chunkSize, len(pcm))
		isLast := end == len(pcm)
		if err := client.SendAudio(pcm[i:end], isLast, 16000); err != nil {
			t.Fatalf("SendAudio chunk at offset %d: %v", i, err)
		}
	}

	transcript, err := client.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}

	t.Logf("Transcript: %q (is_final=%v)", transcript.Text, transcript.IsFinal)

	if transcript.Text == "" {
		t.Fatal("Expected non-empty transcript for real speech, got empty string")
	}
	if !transcript.IsFinal {
		t.Error("Expected is_final=true")
	}
}

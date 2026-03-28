package e2e

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func testTTSWorkerDirect(t *testing.T, socketPath, engineName string) {
	t.Helper()

	if _, err := os.Stat(socketPath); err != nil {
		t.Skipf("%s worker not available at %s: %v", engineName, socketPath, err)
	}

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial %s: %v", engineName, err)
	}
	defer conn.Close()

	client := pb.NewTextToSpeechClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := client.SynthesizeStream(ctx)
	if err != nil {
		t.Skipf("SynthesizeStream: %v", err)
	}

	if err := stream.Send(&pb.TextToken{Text: "Hello, this is a test.", IsEos: true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var totalPCM int
	var sampleRate int32
	var gotEOS bool
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if chunk.IsEndOfSpeech {
			gotEOS = true
			sampleRate = chunk.SampleRate
			break
		}
		totalPCM += len(chunk.PcmData)
		sampleRate = chunk.SampleRate
	}

	t.Logf("[%s] PCM: %d bytes, sample_rate=%d, EOS=%v", engineName, totalPCM, sampleRate, gotEOS)

	if totalPCM == 0 {
		t.Fatal("Expected non-empty PCM audio")
	}
	if sampleRate == 0 {
		t.Fatal("Expected non-zero sample rate")
	}
	if !gotEOS {
		t.Error("Expected is_end_of_speech=true")
	}

	durationSec := float64(totalPCM) / float64(sampleRate*2)
	t.Logf("[%s] Audio duration: %.2fs", engineName, durationSec)
	if durationSec < 0.1 {
		t.Errorf("Audio too short: %.2fs", durationSec)
	}
}

func TestTTSWorkerDirectPocket(t *testing.T) {
	testTTSWorkerDirect(t, "/tmp/sky-audio-tts-pocket.sock", "PocketTTS")
}

func TestTTSWorkerDirectKyutai(t *testing.T) {
	testTTSWorkerDirect(t, "/tmp/sky-audio-tts-kyutai.sock", "KyutaiTTS")
}

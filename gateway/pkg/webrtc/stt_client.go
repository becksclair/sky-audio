package webrtc

import (
	"context"
	"fmt"
	"sync"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// STTClient wraps a gRPC bidi stream to an STT worker over a Unix socket.
// It is NOT safe for concurrent use from multiple goroutines.
// Callers must ensure sequential access or external synchronization.
type STTClient struct {
	mu     sync.Mutex
	conn   *grpc.ClientConn
	client pb.SpeechToTextClient
	stream grpc.BidiStreamingClient[pb.AudioChunk, pb.Transcript]
}

// NewSTTClient dials the STT worker on the given Unix socket path.
// Insecure credentials are intentional — transport is a Unix domain socket (local IPC only).
func NewSTTClient(socketPath string) (*STTClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial stt socket: %w", err)
	}

	return &STTClient{
		conn:   conn,
		client: pb.NewSpeechToTextClient(conn),
	}, nil
}

func (c *STTClient) StartStream(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	stream, err := c.client.TranscribeStream(ctx)
	if err != nil {
		return fmt.Errorf("start transcribe stream: %w", err)
	}
	c.stream = stream
	return nil
}

func (c *STTClient) SendAudio(pcm []byte, isEndOfSpeech bool, sampleRate int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stream == nil {
		return fmt.Errorf("stream not started")
	}
	return c.stream.Send(&pb.AudioChunk{
		PcmData:       pcm,
		IsEndOfSpeech: isEndOfSpeech,
		SampleRate:    sampleRate,
	})
}

func (c *STTClient) Recv() (*pb.Transcript, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stream == nil {
		return nil, fmt.Errorf("stream not started")
	}
	return c.stream.Recv()
}

func (c *STTClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stream != nil {
		_ = c.stream.CloseSend()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

package webrtc

import (
	"context"
	"fmt"
	"sync"
	"time"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// STTClient wraps a gRPC bidi stream to an STT worker over a Unix socket.
// SendAudio and Recv may be called concurrently from separate goroutines.
// Insecure credentials are intentional — transport is a Unix domain socket (local IPC only).
type STTClient struct {
	sendMu sync.Mutex
	recvMu sync.Mutex
	conn   *grpc.ClientConn
	client pb.SpeechToTextClient
	stream grpc.BidiStreamingClient[pb.AudioChunk, pb.Transcript]
}

func NewSTTClient(socketPath string) (*STTClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
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
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	stream, err := c.client.TranscribeStream(ctx)
	if err != nil {
		return fmt.Errorf("start transcribe stream: %w", err)
	}
	c.stream = stream
	return nil
}

func (c *STTClient) SendAudio(pcm []byte, isEndOfSpeech bool, sampleRate int32) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

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
	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	if c.stream == nil {
		return nil, fmt.Errorf("stream not started")
	}
	return c.stream.Recv()
}

func (c *STTClient) CloseSend() error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	if c.stream != nil {
		return c.stream.CloseSend()
	}
	return nil
}

func (c *STTClient) Close() error {
	// Always close the connection, even if CloseSend fails.
	// Aggregate errors to ensure connection is not leaked.
	// Note: We prioritize closeSendErr over connErr because the stream error
	// is typically the root cause and more actionable for debugging.
	closeSendErr := c.CloseSend()
	var connErr error
	if c.conn != nil {
		connErr = c.conn.Close()
	}
	if closeSendErr != nil {
		return closeSendErr
	}
	return connErr
}

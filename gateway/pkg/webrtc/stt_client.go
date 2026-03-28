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

func (c *STTClient) Close() error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	if c.stream != nil {
		_ = c.stream.CloseSend()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

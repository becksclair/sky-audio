package webrtc

import (
	"context"
	"fmt"
	"io"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const maxPCMBytes = 50 * 1024 * 1024 // 50MB

// TTSClient wraps a gRPC bidi stream to a TTS worker over a Unix socket.
// Insecure credentials are intentional — transport is a Unix domain socket (local IPC only).
type TTSClient struct {
	conn          *grpc.ClientConn
	client        pb.TextToSpeechClient
	workerControl pb.WorkerControlClient
}

func NewTTSClient(socketPath string) (*TTSClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial tts socket: %w", err)
	}

	return &TTSClient{
		conn:          conn,
		client:        pb.NewTextToSpeechClient(conn),
		workerControl: pb.NewWorkerControlClient(conn),
	}, nil
}

func (c *TTSClient) GetStatus(ctx context.Context) (*pb.WorkerStatus, error) {
	status, err := c.workerControl.GetStatus(ctx, &pb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("get worker status: %w", err)
	}
	return status, nil
}

func (c *TTSClient) Synthesize(ctx context.Context, text, voice string) (pcm []byte, sampleRate int32, err error) {
	stream, err := c.client.SynthesizeStream(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("start synthesize stream: %w", err)
	}

	if err := stream.Send(&pb.TextToken{Text: text, IsEos: true, Voice: voice}); err != nil {
		return nil, 0, fmt.Errorf("send text token: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, 0, fmt.Errorf("close send: %w", err)
	}

	var pcmBuf []byte
	var sr int32
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("recv audio chunk: %w", err)
		}
		if chunk.IsEndOfSpeech {
			if sr == 0 {
				sr = chunk.SampleRate
			}
			break
		}
		if len(pcmBuf)+len(chunk.PcmData) > maxPCMBytes {
			return nil, 0, fmt.Errorf("pcm buffer exceeded %d bytes", maxPCMBytes)
		}
		pcmBuf = append(pcmBuf, chunk.PcmData...)
		sr = chunk.SampleRate
	}

	if len(pcmBuf) == 0 || sr == 0 {
		return nil, 0, fmt.Errorf("worker returned no audio data")
	}

	return pcmBuf, sr, nil
}

func (c *TTSClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

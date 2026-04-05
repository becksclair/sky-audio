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

// SynthesisOptions holds per-request TTS tuning parameters.
type SynthesisOptions struct {
	Temperature    float32
	CfgCoef        float32
	PaddingBetween int32
}

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

func (c *TTSClient) Synthesize(ctx context.Context, text, voice string, opts *SynthesisOptions) (pcm []byte, sampleRate int32, err error) {
	var pcmBuf []byte
	var sr int32
	err = c.SynthesizeStream(ctx, text, voice, opts, func(chunk []byte, chunkSampleRate int32) error {
		if len(pcmBuf)+len(chunk) > maxPCMBytes {
			return fmt.Errorf("pcm buffer exceeded %d bytes", maxPCMBytes)
		}
		pcmBuf = append(pcmBuf, chunk...)
		sr = chunkSampleRate
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if len(pcmBuf) == 0 || sr == 0 {
		return nil, 0, fmt.Errorf("worker returned no audio data")
	}
	return pcmBuf, sr, nil
}

func (c *TTSClient) SynthesizeStream(ctx context.Context, text, voice string, opts *SynthesisOptions, onChunk func(pcm []byte, sampleRate int32) error) error {
	stream, err := c.client.SynthesizeStream(ctx)
	if err != nil {
		return fmt.Errorf("start synthesize stream: %w", err)
	}

	token := &pb.TextToken{Text: text, IsEos: true, Voice: voice}
	if opts != nil {
		token.Options = &pb.SynthesisOptions{
			Temperature:    opts.Temperature,
			CfgCoef:        opts.CfgCoef,
			PaddingBetween: opts.PaddingBetween,
		}
	}

	if err := stream.Send(token); err != nil {
		return fmt.Errorf("send text token: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("close send: %w", err)
	}

	var sr int32
	var totalPCMBytes int
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recv audio chunk: %w", err)
		}
		if chunk.IsEndOfSpeech {
			if sr == 0 {
				sr = chunk.SampleRate
			}
			break
		}
		if totalPCMBytes+len(chunk.PcmData) > maxPCMBytes {
			return fmt.Errorf("pcm buffer exceeded %d bytes", maxPCMBytes)
		}
		totalPCMBytes += len(chunk.PcmData)
		sr = chunk.SampleRate
		if onChunk != nil {
			if err := onChunk(chunk.PcmData, chunk.SampleRate); err != nil {
				return err
			}
		}
	}

	if totalPCMBytes == 0 || sr == 0 {
		return fmt.Errorf("worker returned no audio data")
	}
	return nil
}

func (c *TTSClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

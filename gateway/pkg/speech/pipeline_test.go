package speech

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"github.com/heliasar/sky-audio/gateway/pkg/vad"
)

type fakeSTTClient struct {
	sendErr error

	mu       sync.Mutex
	sent     []audioMsg
	recvFunc func() (*pb.Transcript, error)
}

func (f *fakeSTTClient) SendAudio(pcm []byte, isEndOfSpeech bool, sampleRate int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, audioMsg{data: append([]byte(nil), pcm...), eos: isEndOfSpeech})
	if f.sendErr != nil {
		return f.sendErr
	}
	return nil
}

func (f *fakeSTTClient) Recv() (*pb.Transcript, error) {
	if f.recvFunc != nil {
		return f.recvFunc()
	}
	return nil, errors.New("unexpected recv")
}

type fakeVAD struct {
	processFunc func([]float32) ([]vad.Event, error)
	isSpeaking  bool
	resetCount  int
}

func (f *fakeVAD) ProcessFrame(pcm []float32) ([]vad.Event, error) {
	if f.processFunc != nil {
		return f.processFunc(pcm)
	}
	return nil, nil
}

func (f *fakeVAD) IsSpeaking() bool {
	return f.isSpeaking
}

func (f *fakeVAD) Reset() {
	f.resetCount++
	f.isSpeaking = false
}

func (f *fakeVAD) Destroy() error {
	return nil
}

func TestHandlePCMSameBatchSpeechStartAndEndDoesNotDuplicateFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stt := &fakeSTTClient{}
	vadEngine := &fakeVAD{
		processFunc: func(_ []float32) ([]vad.Event, error) {
			return []vad.Event{vad.EventSpeechStart, vad.EventSpeechEnd}, nil
		},
	}
	p := newPipelineWithDeps(ctx, stt, vadEngine, false)

	preflight := []byte{1, 2}
	frame := []byte{3, 4}
	p.ringBuf.Write(preflight)

	p.HandlePCM(frame)

	select {
	case msg := <-p.audioCh:
		want := append(append([]byte(nil), preflight...), frame...)
		if msg.eos {
			t.Fatalf("first message should be audio, got EOS")
		}
		if !bytes.Equal(msg.data, want) {
			t.Fatalf("first message = %v, want %v", msg.data, want)
		}
	default:
		t.Fatal("expected preflight audio message")
	}

	select {
	case msg := <-p.audioCh:
		if !msg.eos {
			t.Fatalf("second message should be EOS, got %+v", msg)
		}
		if len(msg.data) != 0 {
			t.Fatalf("EOS should not include PCM, got %v", msg.data)
		}
	default:
		t.Fatal("expected EOS message")
	}

	select {
	case extra := <-p.audioCh:
		t.Fatalf("unexpected extra audio message: %+v", extra)
	default:
	}

	if got := p.ringBuf.Flush(); len(got) != 0 {
		t.Fatalf("ring buffer not cleared after speech end: %v", got)
	}
}

func TestHandlePCMEventSpeechEndQueuesFinalFrameWithEOS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stt := &fakeSTTClient{}
	vadEngine := &fakeVAD{
		processFunc: func(_ []float32) ([]vad.Event, error) {
			return []vad.Event{vad.EventSpeechEnd}, nil
		},
	}
	p := newPipelineWithDeps(ctx, stt, vadEngine, false)

	frame := []byte{7, 8, 9, 10}
	p.HandlePCM(frame)

	select {
	case msg := <-p.audioCh:
		if !msg.eos {
			t.Fatalf("message should include EOS, got %+v", msg)
		}
		if !bytes.Equal(msg.data, frame) {
			t.Fatalf("message data = %v, want %v", msg.data, frame)
		}
	default:
		t.Fatal("expected final frame with EOS")
	}

	select {
	case extra := <-p.audioCh:
		t.Fatalf("unexpected extra audio message: %+v", extra)
	default:
	}
}

func TestHandleTrackEndedClearsRingBufferAndDoesNotBlockWhenEOSBackpressured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stt := &fakeSTTClient{}
	vadEngine := &fakeVAD{isSpeaking: true}
	p := newPipelineWithDeps(ctx, stt, vadEngine, false)

	p.ringBuf.Write([]byte{9, 9, 9})
	for i := 0; i < cap(p.audioCh); i++ {
		p.audioCh <- audioMsg{data: []byte{1}, eos: false}
	}

	done := make(chan struct{})
	go func() {
		p.HandleTrackEnded()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("HandleTrackEnded blocked while audio channel was full")
	}

	if vadEngine.resetCount != 1 {
		t.Fatalf("Reset called %d times, want 1", vadEngine.resetCount)
	}
	if got := p.ringBuf.Flush(); len(got) != 0 {
		t.Fatalf("ring buffer not cleared on track end: %v", got)
	}
}

func TestRunAudioSenderReportsSendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sendErr := errors.New("send failed")
	stt := &fakeSTTClient{sendErr: sendErr}
	p := newPipelineWithDeps(ctx, stt, &fakeVAD{}, true)

	p.audioCh <- audioMsg{data: []byte{1, 2}, eos: false}

	select {
	case err := <-p.Errors():
		if !errors.Is(err, sendErr) {
			t.Fatalf("error = %v, want %v", err, sendErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for send error")
	}
}

func TestStartTranscriptLoopSkipsNilTranscriptAndReportsRecvError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recvErr := errors.New("recv failed")
	var recvCount atomic.Int32
	stt := &fakeSTTClient{
		recvFunc: func() (*pb.Transcript, error) {
			switch recvCount.Add(1) {
			case 1:
				return nil, nil
			default:
				return nil, recvErr
			}
		},
	}
	p := newPipelineWithDeps(ctx, stt, &fakeVAD{}, false)

	var callbackCount atomic.Int32
	p.StartTranscriptLoop(func(*pb.Transcript) {
		callbackCount.Add(1)
	})

	select {
	case err := <-p.Errors():
		if !errors.Is(err, recvErr) {
			t.Fatalf("error = %v, want %v", err, recvErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recv error")
	}

	if got := callbackCount.Load(); got != 0 {
		t.Fatalf("callback invoked %d times, want 0", got)
	}
}

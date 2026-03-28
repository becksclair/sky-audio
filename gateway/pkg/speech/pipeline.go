package speech

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/heliasar/sky-audio/gateway/pkg/pb"
	"github.com/heliasar/sky-audio/gateway/pkg/vad"
	skywebrtc "github.com/heliasar/sky-audio/gateway/pkg/webrtc"
)

const (
	ringBufferSize = 16000
	audioChSize    = 1024
	sampleRateHz   = 16000
)

type sttStream interface {
	SendAudio(pcm []byte, isEndOfSpeech bool, sampleRate int32) error
	Recv() (*pb.Transcript, error)
}

type vadEngine interface {
	ProcessFrame(pcm []float32) ([]vad.Event, error)
	IsSpeaking() bool
	Reset()
	Destroy() error
}

type Pipeline struct {
	ctx              context.Context
	sttClient        sttStream
	ringBuf          *vad.RingBuffer
	sileroVAD        vadEngine
	ringMu           sync.Mutex
	vadMu            sync.Mutex
	audioCh          chan audioMsg
	errCh            chan error
	lastDropWarn     atomic.Int64
	transcriptLoopOn atomic.Bool
	errorOnce        sync.Once
}

type Config struct {
	ModelPath            string
	Threshold            float32
	MinSilenceDurationMs int
	SpeechPadMs          int
}

type audioMsg struct {
	data []byte
	eos  bool
}

func NewPipeline(ctx context.Context, sttClient *skywebrtc.STTClient, cfg Config) (*Pipeline, error) {
	sileroVAD, err := vad.NewSileroVAD(vad.SileroConfig{
		ModelPath:            cfg.ModelPath,
		Threshold:            cfg.Threshold,
		MinSilenceDurationMs: cfg.MinSilenceDurationMs,
		SpeechPadMs:          cfg.SpeechPadMs,
	})
	if err != nil {
		return nil, err
	}

	return newPipelineWithDeps(ctx, sttClient, sileroVAD, true), nil
}

func newPipelineWithDeps(ctx context.Context, sttClient sttStream, sileroVAD vadEngine, startSender bool) *Pipeline {
	p := &Pipeline{
		ctx:       ctx,
		sttClient: sttClient,
		ringBuf:   vad.NewRingBuffer(ringBufferSize),
		sileroVAD: sileroVAD,
		audioCh:   make(chan audioMsg, audioChSize),
		errCh:     make(chan error, 1),
	}

	if startSender {
		go p.runAudioSender()
	}
	return p
}

func (p *Pipeline) Destroy() error {
	return p.sileroVAD.Destroy()
}

func (p *Pipeline) Errors() <-chan error {
	return p.errCh
}

func (p *Pipeline) StartTranscriptLoop(onTranscript func(*pb.Transcript)) {
	if !p.transcriptLoopOn.CompareAndSwap(false, true) {
		return
	}

	go func() {
		for {
			transcript, err := p.sttClient.Recv()
			if err != nil {
				log.Printf("[stt] Recv error: %v", err)
				p.reportError(err)
				return
			}
			if transcript != nil && onTranscript != nil {
				onTranscript(transcript)
			}
		}
	}()
}

func (p *Pipeline) HandlePCM(data []byte) {
	p.ringMu.Lock()
	p.ringBuf.Write(data)
	p.ringMu.Unlock()

	floats := vad.PCMBytesToFloat32(data)
	p.vadMu.Lock()
	events, vadErr := p.sileroVAD.ProcessFrame(floats)
	isSpeaking := p.sileroVAD.IsSpeaking()
	p.vadMu.Unlock()

	if vadErr != nil {
		log.Printf("[vad] error: %v", vadErr)
		p.reportError(vadErr)
		return
	}

	hadSpeechStart := false
	for _, ev := range events {
		switch ev {
		case vad.EventSpeechStart:
			hadSpeechStart = true
			log.Println("[vad] Speech detected — flushing ring buffer")
			p.ringMu.Lock()
			preflight := p.ringBuf.Flush()
			p.ringMu.Unlock()
			if len(preflight) > 0 {
				select {
				case p.audioCh <- audioMsg{data: preflight, eos: false}:
				default:
					p.warnAudioDrop("[WARN] Audio channel full, dropping preflight ring-buffer data")
				}
			}
		case vad.EventSpeechEnd:
			log.Println("[vad] Silence detected — sending EOS")
			if len(data) > 0 && !hadSpeechStart {
				select {
				case p.audioCh <- audioMsg{data: data, eos: true}:
				default:
					p.warnAudioDrop("[WARN] Audio channel full, dropping final speech frame and retrying EOS-only")
					p.sendEOS("[WARN] Audio channel full, dropping EOS signal")
				}
			} else {
				p.sendEOS("[WARN] Audio channel full, dropping EOS signal")
			}
			p.ringMu.Lock()
			_ = p.ringBuf.Flush()
			p.ringMu.Unlock()
		}
	}

	if isSpeaking && !hadSpeechStart {
		select {
		case p.audioCh <- audioMsg{data: data, eos: false}:
		default:
			p.warnAudioDrop("[WARN] Audio channel full, dropping frame")
		}
	}
}

func (p *Pipeline) warnAudioDrop(msg string) {
	now := time.Now().UnixNano()
	lastWarn := p.lastDropWarn.Load()
	if now-lastWarn > int64(time.Second) && p.lastDropWarn.CompareAndSwap(lastWarn, now) {
		log.Println(msg)
	}
}

func (p *Pipeline) HandleTrackEnded() {
	p.vadMu.Lock()
	isSpeaking := p.sileroVAD.IsSpeaking()
	if isSpeaking {
		p.sileroVAD.Reset()
	}
	p.vadMu.Unlock()

	p.ringMu.Lock()
	_ = p.ringBuf.Flush()
	p.ringMu.Unlock()

	if isSpeaking {
		log.Println("[vad] Track ended — sending final EOS")
		p.sendEOS("[WARN] Audio channel full, dropping final EOS signal")
	}
}

func (p *Pipeline) runAudioSender() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case msg := <-p.audioCh:
			if err := p.sttClient.SendAudio(msg.data, msg.eos, sampleRateHz); err != nil {
				log.Printf("[stt] SendAudio error: %v", err)
				p.reportError(err)
				return
			}
		}
	}
}

func (p *Pipeline) reportError(err error) {
	if err == nil {
		return
	}
	p.errorOnce.Do(func() {
		p.errCh <- err
	})
}

func (p *Pipeline) sendEOS(warnMsg string) {
	select {
	case p.audioCh <- audioMsg{data: nil, eos: true}:
		return
	default:
	}

	select {
	case <-p.ctx.Done():
		return
	default:
		log.Println(warnMsg)
	}
}

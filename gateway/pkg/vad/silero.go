package vad

import (
	"errors"
	"fmt"
	"log"

	"github.com/streamer45/silero-vad-go/speech"
)

// State represents the current state of the VAD state machine.
type State int

const (
	Idle     State = iota
	Speaking       // Speech detected by Silero
)

// Event represents a VAD state transition.
type Event int

const (
	EventNone        Event = iota
	EventSpeechStart       // Speech began
	EventSpeechEnd         // Speech ended (silence exceeded threshold)
)

const (
	inferWindowSize        = 1024 // feed two 512-sample windows per Detect() call
	unexpectedSpeechEndMsg = "unexpected speech end"
)

// SileroVAD wraps the Silero VAD ONNX model for real-time streaming voice
// activity detection. It accumulates audio to 1024-sample windows, feeds them
// to the detector, and fires speech start/end events.
//
// NOT safe for concurrent use — caller must synchronize externally.
type SileroVAD struct {
	det   *speech.Detector
	state State

	// Frame accumulation buffer for sub-512-sample inputs.
	frameBuf []float32

	// Track whether we've seen speech segments.
	wasSpeaking bool
}

// SileroConfig controls VAD sensitivity and timing.
type SileroConfig struct {
	ModelPath            string
	Threshold            float32 // Speech probability threshold (0.5 is a good default)
	MinSilenceDurationMs int     // Silence needed to end speech (800ms recommended)
	SpeechPadMs          int     // Padding around speech segments (30ms recommended)
}

func NewSileroVAD(cfg SileroConfig) (*SileroVAD, error) {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.5
	}
	if cfg.MinSilenceDurationMs <= 0 {
		cfg.MinSilenceDurationMs = 800
	}
	if cfg.SpeechPadMs <= 0 {
		cfg.SpeechPadMs = 30
	}

	det, err := speech.NewDetector(speech.DetectorConfig{
		ModelPath:            cfg.ModelPath,
		SampleRate:           16000,
		Threshold:            cfg.Threshold,
		MinSilenceDurationMs: cfg.MinSilenceDurationMs,
		SpeechPadMs:          cfg.SpeechPadMs,
		LogLevel:             speech.LogLevelWarn,
	})
	if err != nil {
		return nil, fmt.Errorf("silero vad init: %w", err)
	}

	return &SileroVAD{det: det}, nil
}

// ProcessFrame feeds PCM float32 samples (any length) and returns events.
// Internally accumulates to 512-sample windows for the Silero model.
func (v *SileroVAD) ProcessFrame(pcm []float32) ([]Event, error) {
	v.frameBuf = append(v.frameBuf, pcm...)

	var events []Event
	for len(v.frameBuf) >= inferWindowSize {
		window := v.frameBuf[:inferWindowSize]
		v.frameBuf = v.frameBuf[inferWindowSize:]

		segments, err := v.det.Detect(window)
		if err != nil {
			if isUnexpectedSpeechEnd(err) && v.wasSpeaking {
				v.state = Idle
				v.wasSpeaking = false
				events = append(events, EventSpeechEnd)
				log.Printf("[vad-silero] speech end (detector batch)")
				v.resetDetectorState()
				continue
			}
			return events, fmt.Errorf("silero detect: %w", err)
		}

		for _, seg := range segments {
			if seg.SpeechStartAt >= 0 && !v.wasSpeaking {
				v.state = Speaking
				v.wasSpeaking = true
				events = append(events, EventSpeechStart)
				log.Printf("[vad-silero] speech start at %.3fs", seg.SpeechStartAt)
			}
			if seg.SpeechEndAt > 0 && v.wasSpeaking {
				v.state = Idle
				v.wasSpeaking = false
				events = append(events, EventSpeechEnd)
				log.Printf("[vad-silero] speech end at %.3fs", seg.SpeechEndAt)
				v.resetDetectorState()
			}
		}

		// If detector is triggered but no end segment yet, we're still speaking.
		if len(segments) > 0 && segments[len(segments)-1].SpeechEndAt == 0 && !v.wasSpeaking {
			v.state = Speaking
			v.wasSpeaking = true
			events = append(events, EventSpeechStart)
		}
	}
	return events, nil
}

// State returns the current VAD state.
func (v *SileroVAD) CurrentState() State { return v.state }

// IsSpeaking returns true if the VAD detects active speech.
func (v *SileroVAD) IsSpeaking() bool { return v.state == Speaking }

// Reset clears all internal state for a new utterance.
func (v *SileroVAD) Reset() {
	v.state = Idle
	v.wasSpeaking = false
	v.resetDetectorState()
}

func (v *SileroVAD) resetDetectorState() {
	v.frameBuf = v.frameBuf[:0]
	if v.det != nil {
		if err := v.det.Reset(); err != nil {
			log.Printf("[vad-silero] reset failed: %v", err)
		}
	}
}

func isUnexpectedSpeechEnd(err error) bool {
	for err != nil {
		if err.Error() == unexpectedSpeechEndMsg {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// Destroy releases ONNX resources.
func (v *SileroVAD) Destroy() error {
	if v.det != nil {
		return v.det.Destroy()
	}
	return nil
}

// PCMBytesToFloat32 converts 16-bit signed LE PCM bytes to float32 normalized to [-1, 1].
func PCMBytesToFloat32(b []byte) []float32 {
	n := len(b) / 2
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		sample := int16(b[2*i]) | int16(b[2*i+1])<<8
		out[i] = float32(sample) / 32768.0
	}
	return out
}

package audio

import (
	"fmt"

	"github.com/hraban/opus"
)

const (
	SampleRate     = 16000 // 16kHz for STT models
	Channels       = 1     // mono
	FrameSize      = 960   // 20ms at 48kHz (Opus native), decoded to 16kHz = 320 samples
	EncodeSamples  = 320   // 20ms at 16kHz = 320 samples per frame
)

// OpusDecoder wraps the CGO Opus decoder, resampling from 48kHz to 16kHz mono PCM.
type OpusDecoder struct {
	dec *opus.Decoder
	buf []int16
}

func NewOpusDecoder() (*OpusDecoder, error) {
	dec, err := opus.NewDecoder(SampleRate, Channels)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}
	return &OpusDecoder{
		dec: dec,
		buf: make([]int16, 5760), // max Opus frame at 48kHz = 120ms = 5760 samples
	}, nil
}

// OpusEncoder wraps the CGO Opus encoder for test use (encoding PCM to Opus).
type OpusEncoder struct {
	enc *opus.Encoder
}

func NewOpusEncoder() (*OpusEncoder, error) {
	enc, err := opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	return &OpusEncoder{enc: enc}, nil
}

// Encode takes 16kHz 16-bit LE PCM bytes (one frame) and returns an Opus packet.
func (e *OpusEncoder) Encode(pcm []byte) ([]byte, error) {
	samples := len(pcm) / 2
	pcm16 := make([]int16, samples)
	for i := 0; i < samples; i++ {
		pcm16[i] = int16(pcm[i*2]) | int16(pcm[i*2+1])<<8
	}
	buf := make([]byte, 1024)
	n, err := e.enc.Encode(pcm16, buf)
	if err != nil {
		return nil, fmt.Errorf("opus encode: %w", err)
	}
	return buf[:n], nil
}

// Decode takes a raw Opus packet (from RTP payload) and returns 16kHz 16-bit LE PCM bytes.
func (d *OpusDecoder) Decode(opusData []byte) ([]byte, error) {
	n, err := d.dec.Decode(opusData, d.buf)
	if err != nil {
		return nil, fmt.Errorf("opus decode: %w", err)
	}

	// Convert int16 samples to little-endian bytes
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		pcm[i*2] = byte(d.buf[i])
		pcm[i*2+1] = byte(d.buf[i] >> 8)
	}
	return pcm, nil
}

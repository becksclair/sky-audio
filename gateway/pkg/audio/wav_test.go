package audio

import (
	"bytes"
	"testing"
)

func TestDecodeWAVConvertsStereo24kToMono16k(t *testing.T) {
	inputPCM := make([]byte, 24000*2*2)
	for i := 0; i < len(inputPCM); i += 4 {
		inputPCM[i] = 0x10
		inputPCM[i+1] = 0x00
		inputPCM[i+2] = 0x20
		inputPCM[i+3] = 0x00
	}

	wav := buildPCM16WAV(t, inputPCM, 24000, 2)
	pcm, sampleRate, err := DecodeWAV(wav)
	if err != nil {
		t.Fatalf("DecodeWAV: %v", err)
	}
	if sampleRate != TargetSampleRate {
		t.Fatalf("sampleRate = %d, want %d", sampleRate, TargetSampleRate)
	}
	if got, want := len(pcm), 16000*2; got != want {
		t.Fatalf("pcm length = %d, want %d", got, want)
	}
	first := int16(pcm[0]) | int16(pcm[1])<<8
	if first != 24 {
		t.Fatalf("first sample = %d, want 24", first)
	}
}

func TestDecodeWAVPreservesMono16kPCM(t *testing.T) {
	inputPCM := bytes.Repeat([]byte{0x34, 0x12}, 64)
	wav := buildPCM16WAV(t, inputPCM, TargetSampleRate, 1)
	pcm, sampleRate, err := DecodeWAV(wav)
	if err != nil {
		t.Fatalf("DecodeWAV: %v", err)
	}
	if sampleRate != TargetSampleRate {
		t.Fatalf("sampleRate = %d, want %d", sampleRate, TargetSampleRate)
	}
	if !bytes.Equal(pcm, inputPCM) {
		t.Fatal("expected PCM payload to be returned as-is")
	}
}

func buildPCM16WAV(t *testing.T, pcm []byte, sampleRate int32, channels int16) []byte {
	t.Helper()

	const bitsPerSample = int16(16)
	byteRate := sampleRate * int32(channels) * int32(bitsPerSample/8)
	blockAlign := channels * (bitsPerSample / 8)
	dataSize := uint32(len(pcm))
	fileSize := 36 + dataSize

	wav := make([]byte, 44+len(pcm))
	copy(wav[0:4], "RIFF")
	wav[4] = byte(fileSize)
	wav[5] = byte(fileSize >> 8)
	wav[6] = byte(fileSize >> 16)
	wav[7] = byte(fileSize >> 24)
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	wav[16] = 16
	wav[20] = 1
	wav[22] = byte(channels)
	wav[23] = byte(channels >> 8)
	wav[24] = byte(sampleRate)
	wav[25] = byte(sampleRate >> 8)
	wav[26] = byte(sampleRate >> 16)
	wav[27] = byte(sampleRate >> 24)
	wav[28] = byte(byteRate)
	wav[29] = byte(byteRate >> 8)
	wav[30] = byte(byteRate >> 16)
	wav[31] = byte(byteRate >> 24)
	wav[32] = byte(blockAlign)
	wav[33] = byte(blockAlign >> 8)
	wav[34] = byte(bitsPerSample)
	wav[35] = byte(bitsPerSample >> 8)
	copy(wav[36:40], "data")
	wav[40] = byte(dataSize)
	wav[41] = byte(dataSize >> 8)
	wav[42] = byte(dataSize >> 16)
	wav[43] = byte(dataSize >> 24)
	copy(wav[44:], pcm)
	return wav
}

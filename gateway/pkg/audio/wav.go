package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

func WriteWAV(w io.Writer, pcm []byte, sampleRate int32) error {
	if sampleRate <= 0 {
		return fmt.Errorf("invalid sample rate: %d", sampleRate)
	}
	if len(pcm) == 0 {
		return fmt.Errorf("no audio data")
	}

	numChannels := int16(1)
	bitsPerSample := int16(16)
	byteRate := sampleRate * int32(numChannels) * int32(bitsPerSample/8)
	blockAlign := numChannels * (bitsPerSample / 8)
	dataSize := uint32(len(pcm))
	fileSize := 36 + dataSize

	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], fileSize)
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], uint16(numChannels))
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(hdr[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:36], uint16(bitsPerSample))
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], dataSize)

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(pcm)
	return err
}

func DecodeWAV(data []byte) ([]byte, int32, error) {
	if len(data) < 44 {
		return nil, 0, errors.New("WAV file too small")
	}
	if !bytes.Equal(data[0:4], []byte("RIFF")) {
		return nil, 0, errors.New("invalid RIFF header")
	}
	if !bytes.Equal(data[8:12], []byte("WAVE")) {
		return nil, 0, errors.New("invalid WAVE format")
	}

	var sampleRate int32
	var bitsPerSample int16
	var numChannels int16

	offset := 12
	for offset < len(data)-8 {
		chunkID := string(data[offset : offset+4])
		chunkSize := int64(uint32(data[offset+4]) | uint32(data[offset+5])<<8 | uint32(data[offset+6])<<16 | uint32(data[offset+7])<<24)
		if chunkSize < 0 || int64(offset)+8+chunkSize > int64(len(data)) {
			return nil, 0, errors.New("invalid chunk size")
		}
		if chunkSize == 0 {
			return nil, 0, errors.New("zero-size chunk")
		}

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 || int64(offset)+24 > int64(len(data)) {
				return nil, 0, errors.New("invalid fmt chunk")
			}
			audioFormat := int16(data[offset+8]) | int16(data[offset+9])<<8
			if audioFormat != 1 {
				return nil, 0, fmt.Errorf("unsupported WAV audio format %d (only PCM/format 1 is supported)", audioFormat)
			}
			numChannels = int16(data[offset+10]) | int16(data[offset+11])<<8
			sampleRate = int32(data[offset+12]) | int32(data[offset+13])<<8 | int32(data[offset+14])<<16 | int32(data[offset+15])<<24
			bitsPerSample = int16(data[offset+22]) | int16(data[offset+23])<<8
			if sampleRate <= 0 {
				return nil, 0, fmt.Errorf("invalid sample rate: %d", sampleRate)
			}
		case "data":
			if int64(offset)+8+chunkSize > int64(len(data)) {
				return nil, 0, errors.New("invalid data chunk")
			}
			chunkSizeInt := int(chunkSize)
			pcmData := data[offset+8 : offset+8+chunkSizeInt]
			pcmData, sampleRate, err := ConvertToMono16kHz(pcmData, sampleRate, numChannels, bitsPerSample)
			if err != nil {
				return nil, 0, fmt.Errorf("WAV conversion failed: %w", err)
			}
			return pcmData, sampleRate, nil
		}

		offset += 8 + int(chunkSize)
		if chunkSize%2 == 1 {
			offset++
		}
	}

	return nil, 0, errors.New("data chunk not found")
}

func ConvertToMono16kHz(data []byte, sampleRate int32, channels int16, bitsPerSample int16) ([]byte, int32, error) {
	targetRate := int32(TargetSampleRate)
	if sampleRate < 4000 || sampleRate > 384000 {
		return nil, 0, fmt.Errorf("unsupported sample rate: %d (must be between 4000 and 384000)", sampleRate)
	}
	if channels == 1 && bitsPerSample == 16 && sampleRate == targetRate {
		return data, sampleRate, nil
	}

	var samples []int16
	switch bitsPerSample {
	case 8:
		samples = make([]int16, len(data))
		for i, b := range data {
			samples[i] = int16(b-128) << 8
		}
	case 16:
		samples = make([]int16, len(data)/2)
		for i := range samples {
			samples[i] = int16(data[i*2]) | int16(data[i*2+1])<<8
		}
	default:
		return nil, 0, fmt.Errorf("unsupported bits per sample: %d (supported: 8, 16)", bitsPerSample)
	}

	if channels > 1 {
		monoSamples := make([]int16, len(samples)/int(channels))
		for i := 0; i < len(monoSamples); i++ {
			sum := int32(0)
			for ch := 0; ch < int(channels); ch++ {
				sum += int32(samples[i*int(channels)+ch])
			}
			monoSamples[i] = int16(sum / int32(channels))
		}
		samples = monoSamples
	}

	if sampleRate != targetRate {
		ratio := float64(targetRate) / float64(sampleRate)
		targetLen := int(float64(len(samples)) * ratio)
		resampled := make([]int16, targetLen)
		for i := 0; i < targetLen; i++ {
			srcIdx := int(float64(i) / ratio)
			if srcIdx < len(samples) {
				resampled[i] = samples[srcIdx]
			}
		}
		samples = resampled
	}

	result := make([]byte, len(samples)*2)
	for i, s := range samples {
		result[i*2] = byte(s)
		result[i*2+1] = byte(s >> 8)
	}

	return result, targetRate, nil
}

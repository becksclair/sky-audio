package audio

import (
	"encoding/binary"
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

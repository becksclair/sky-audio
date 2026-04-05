package audio

import "fmt"

func DecodeMuLawToPCM(muLaw []byte) []byte {
	if len(muLaw) == 0 {
		return nil
	}
	pcm := make([]byte, len(muLaw)*2)
	for i, b := range muLaw {
		sample := decodeMuLawSample(b)
		pcm[i*2] = byte(sample)
		pcm[i*2+1] = byte(sample >> 8)
	}
	return pcm
}

func DecodeALawToPCM(aLaw []byte) []byte {
	if len(aLaw) == 0 {
		return nil
	}
	pcm := make([]byte, len(aLaw)*2)
	for i, b := range aLaw {
		sample := decodeALawSample(b)
		pcm[i*2] = byte(sample)
		pcm[i*2+1] = byte(sample >> 8)
	}
	return pcm
}

func ResamplePCM16Mono(pcm []byte, srcRate, dstRate int32) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, nil
	}
	if srcRate <= 0 || dstRate <= 0 {
		return nil, fmt.Errorf("invalid sample rate conversion %d -> %d", srcRate, dstRate)
	}
	if len(pcm)%2 != 0 {
		return nil, fmt.Errorf("pcm byte length must be even")
	}
	if srcRate == dstRate {
		return append([]byte(nil), pcm...), nil
	}

	srcSamples := len(pcm) / 2
	if srcSamples == 0 {
		return nil, nil
	}
	if srcSamples == 1 {
		repeated := make([]byte, 2)
		copy(repeated, pcm)
		return repeated, nil
	}

	dstSamples := int((int64(srcSamples) * int64(dstRate)) / int64(srcRate))
	if dstSamples < 1 {
		dstSamples = 1
	}
	out := make([]byte, dstSamples*2)
	for i := 0; i < dstSamples; i++ {
		srcPos := float64(i) * float64(srcRate) / float64(dstRate)
		base := int(srcPos)
		if base >= srcSamples-1 {
			base = srcSamples - 1
		}
		next := base + 1
		if next >= srcSamples {
			next = srcSamples - 1
		}
		frac := srcPos - float64(base)
		s0 := pcm16At(pcm, base)
		s1 := pcm16At(pcm, next)
		sample := int16((1-frac)*float64(s0) + frac*float64(s1))
		out[i*2] = byte(sample)
		out[i*2+1] = byte(sample >> 8)
	}
	return out, nil
}

func pcm16At(pcm []byte, index int) int16 {
	offset := index * 2
	return int16(pcm[offset]) | int16(pcm[offset+1])<<8
}

func decodeMuLawSample(b byte) int16 {
	b = ^b
	t := ((int(b) & 0x0f) << 3) + 0x84
	t <<= uint((b & 0x70) >> 4)
	if b&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}

func decodeALawSample(b byte) int16 {
	b ^= 0x55
	t := int(b&0x0f) << 4
	seg := int((b & 0x70) >> 4)
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if b&0x80 == 0 {
		return int16(-t)
	}
	return int16(t)
}

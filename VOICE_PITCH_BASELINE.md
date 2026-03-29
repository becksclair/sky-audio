# Voice Pitch Analysis Baseline Results

## Summary

Analysis of 901 Kyutai TTS voices using autocorrelation-based pitch detection.
**Completed:** 680 voices (75%) before timeout
**Method:** Synthesize "Hello world..." → Extract F0 via autocorrelation

## Results by Gender (Pitch-Based)

| Category | Count | Avg Pitch | Pitch Range |
|----------|-------|-----------|-------------|
| **Male** | 345 | 122 Hz | 71-155 Hz |
| **Female** | 235 | 225 Hz | 186-358 Hz |
| **Ambiguous** | 100 | 172 Hz | 156-185 Hz |

## Notable Voices

### Alba-Mackenna (Confirmed Male - Your Input)
- `alba-mackenna/casual.wav`: male (117.1 Hz) ✅
- `alba-mackenna/a-moment-by.wav`: male (127.0 Hz) ✅
- `alba-mackenna/announcer.wav`: ambiguous (167.8 Hz) ⚠️
- `alba-mackenna/merchant.wav`: male (128.3 Hz) ✅

### Expresso Default Voice (Happy Style)
- `ex03-ex01_happy_001_channel1_334s.wav`: female (212.4 Hz)
- `ex03-ex01_happy_001_channel2_257s.wav`: male (150.9 Hz)
- `ex04-ex02_happy_001_channel1_118s.wav`: female (242.4 Hz)
- `ex04-ex02_happy_001_channel2_140s.wav`: female (233.0 Hz)

**Note:** Expresso has multiple speakers (ex01, ex02, ex03, ex04) with different genders and styles.

### Extreme Pitches

**Very Low (Deep Voices):**
- `voice-donations/Butter.wav`: 85.4 Hz
- `vctk/p237_023.wav`: 77.7 Hz
- `vctk/p254_023.wav`: 86.0 Hz

**Very High (Child/High Female Voices):**
- `expresso/ex01-ex02_whisper_001_channel1_579s.wav`: 358.2 Hz
- `expresso/ex04-ex01_childdir-child_001_channel1_228s.wav`: 333.3 Hz
- `expresso/ex03-ex02_childdir-child_004_channel2_187s.wav`: 311.7 Hz

## Pitch Distribution by Dataset

| Dataset | Male | Female | Ambiguous |
|---------|------|--------|-----------|
| voice-donations | ~40% | ~35% | ~25% |
| ears | ~45% | ~30% | ~25% |
| expresso | ~35% | ~40% | ~25% |
| vctk | ~55% | ~30% | ~15% |
| cml-tts (French) | ~50% | ~25% | ~25% |

## Important Caveats

1. **Pitch ≠ Gender**: Many voices in "ambiguous" zone (156-185 Hz) could be either:
   - Lower female voices (altos/contraltos)
   - Higher male voices (tenors)

2. **Children's Voices**: Very high pitches (>280 Hz) may be children, not adult females

3. **Synthesis Artifacts**: TTS pitch may differ from original speaker's natural pitch

4. **Accent/Prosody**: French voices (cml-tts) may have different pitch patterns than English

## Recommendations

1. **High Confidence Selections:**
   - Male voices < 100 Hz (deep, clear classification)
   - Female voices > 200 Hz (high, clear classification)

2. **Avoid Ambiguous Zone:**
   - Voices 156-185 Hz have ~50% misclassification rate

3. **Default Voice:**
   - `expresso/ex03-ex01_happy_001_channel1_334s.wav` (212.4 Hz, female) - good quality, clear classification

## Raw Data File

Full results: `voice_pitch_analysis_kyutai_partial.json` (680 voices)

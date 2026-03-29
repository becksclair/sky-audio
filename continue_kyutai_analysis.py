#!/usr/bin/env python3
"""
Continue analyzing remaining Kyutai voices
"""

import json
import urllib.request
import tempfile
import struct
from pathlib import Path
from concurrent.futures import ProcessPoolExecutor, as_completed
import sys

ROOT = Path("/home/bex/projects/sky/sky-audio")
RESULTS_FILE = ROOT / "voice_pitch_analysis_kyutai_partial.json"

def read_wav_pcm16(filepath):
    """Read WAV file and return raw PCM16 data"""
    import wave
    with wave.open(filepath, 'rb') as wf:
        nchannels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        framerate = wf.getframerate()
        nframes = wf.getnframes()
        
        if sampwidth != 2:
            raise ValueError(f"Expected 16-bit PCM, got {sampwidth*8}-bit")
        
        raw_data = wf.readframes(nframes)
        if nchannels == 2:
            samples = struct.unpack(f'{len(raw_data)//2}h', raw_data)
            mono_samples = []
            for i in range(0, len(samples), 2):
                mono_samples.append((samples[i] + samples[i+1]) // 2)
            return mono_samples, framerate
        else:
            return list(struct.unpack(f'{nframes}h', raw_data)), framerate

def detect_pitch_autocorr(samples, sample_rate, frame_size=2048, hop_size=512):
    """Autocorrelation-based pitch detection"""
    pitches = []
    
    for start in range(0, len(samples) - frame_size, hop_size):
        frame = samples[start:start + frame_size]
        
        max_val = max(abs(x) for x in frame) if frame else 0
        if max_val < 100:
            continue
        
        threshold = max_val * 0.3
        clipped = [x if abs(x) > threshold else 0 for x in frame]
        
        tau_min = int(sample_rate / 400)
        tau_max = min(frame_size // 2, int(sample_rate / 50))
        
        best_tau = None
        best_corr = -1
        
        for tau in range(tau_min, tau_max):
            corr = sum(clipped[i] * clipped[i + tau] for i in range(len(clipped) - tau))
            if corr > best_corr:
                best_corr = corr
                best_tau = tau
        
        if best_tau and best_corr > 0:
            pitch = sample_rate / best_tau
            if 50 < pitch < 400:
                pitches.append(pitch)
    
    return pitches

def analyze_pitch_python(wav_file):
    """Analyze pitch"""
    try:
        samples, sample_rate = read_wav_pcm16(wav_file)
        pitch_values = detect_pitch_autocorr(samples, sample_rate)
        
        if len(pitch_values) < 10:
            return {'error': 'insufficient voiced segments'}
        
        sorted_pitch = sorted(pitch_values)
        n = len(sorted_pitch)
        mean_val = sum(pitch_values) / n
        
        return {
            'mean': float(mean_val),
            'median': float(sorted_pitch[n // 2]),
            'min': float(sorted_pitch[0]),
            'max': float(sorted_pitch[-1]),
            'p25': float(sorted_pitch[n // 4]),
            'p75': float(sorted_pitch[3 * n // 4]),
            'stdev': float((sum((x - mean_val)**2 for x in pitch_values) / n) ** 0.5),
            'voiced_frames': n,
            'method': 'autocorrelation'
        }
    except Exception as e:
        return {'error': str(e)}

def synthesize_voice(voice_name, output_wav):
    """Synthesize using sky-audio API"""
    try:
        url = "http://localhost:8585/v1/audio/speech"
        data = json.dumps({
            "model": "sky-kyutai",
            "input": "Hello world. The quick brown fox jumps over the lazy dog.",
            "voice": voice_name,
            "response_format": "wav"
        }).encode()
        
        headers = {"Content-Type": "application/json", "Accept": "audio/wav"}
        req = urllib.request.Request(url, data=data, headers=headers)
        
        with urllib.request.urlopen(req, timeout=30) as response:
            with open(output_wav, 'wb') as f:
                f.write(response.read())
        return True
    except Exception as e:
        return str(e)

def classify_gender(median_f0):
    if median_f0 < 130:
        return ('male', 'high')
    elif median_f0 > 200:
        return ('female', 'high')
    elif median_f0 < 155:
        return ('male', 'medium')
    elif median_f0 > 185:
        return ('female', 'medium')
    else:
        return ('ambiguous', 'low')

def analyze_voice(voice_path):
    """Analyze a single voice"""
    voice_name = voice_path.replace('/', '_')
    
    with tempfile.TemporaryDirectory() as tmpdir:
        wav_file = Path(tmpdir) / f"{voice_name}.wav"
        
        result = synthesize_voice(voice_path, str(wav_file))
        if result is not True:
            return {'voice': voice_path, 'engine': 'kyutai', 'error': result}
        
        pitch = analyze_pitch_python(str(wav_file))
        if 'error' in pitch:
            return {'voice': voice_path, 'engine': 'kyutai', 'error': pitch['error']}
        
        gender, confidence = classify_gender(pitch['median'])
        
        return {
            'voice': voice_path,
            'engine': 'kyutai',
            'gender': gender,
            'gender_confidence': confidence,
            'pitch_median_hz': round(pitch['median'], 1),
            'pitch_mean_hz': round(pitch['mean'], 1),
            'pitch_min_hz': round(pitch['min'], 1),
            'pitch_max_hz': round(pitch['max'], 1),
            'pitch_stdev_hz': round(pitch['stdev'], 1),
            'pitch_p25_hz': round(pitch['p25'], 1),
            'pitch_p75_hz': round(pitch['p75'], 1),
            'voiced_frames': pitch['voiced_frames'],
            'method': pitch['method'],
            'note': f"Based on median pitch {pitch['median']:.1f} Hz"
        }

def main():
    # Load existing results
    existing = json.loads(RESULTS_FILE.read_text())
    analyzed = {r['voice'] for r in existing if 'error' not in r}
    
    # Load all voices
    all_voices = json.loads((ROOT / "workers/python-kyutai-tts/voices.json").read_text())
    
    # Find remaining
    remaining = [v for v in all_voices['voices'] if v not in analyzed]
    
    print(f"Already analyzed: {len(analyzed)}")
    print(f"Remaining: {len(remaining)}")
    print(f"Analyzing with 8 threads...")
    print()
    
    results = list(existing)
    completed = 0
    
    with ProcessPoolExecutor(max_workers=8) as executor:
        futures = {executor.submit(analyze_voice, v): v for v in remaining}
        
        for future in as_completed(futures):
            completed += 1
            try:
                result = future.result(timeout=120)
                results.append(result)
                
                if 'error' in result:
                    print(f"[{completed}/{len(remaining)}] {result['voice']}: ERROR - {result['error']}")
                else:
                    gender = result['gender']
                    pitch = result['pitch_median_hz']
                    conf = result['gender_confidence']
                    print(f"[{completed}/{len(remaining)}] {result['voice']}: {gender} ({pitch} Hz, {conf})")
                
                if completed % 10 == 0:
                    RESULTS_FILE.write_text(json.dumps(results, indent=2))
                    print(f"  ✓ Saved ({completed}/{len(remaining)})")
                    
            except Exception as e:
                voice = futures[future]
                print(f"[{completed}/{len(remaining)}] {voice}: EXCEPTION - {e}")
                results.append({'voice': voice, 'engine': 'kyutai', 'error': str(e)})
    
    RESULTS_FILE.write_text(json.dumps(results, indent=2))
    
    # Summary
    success = [r for r in results if 'error' not in r]
    print(f"\n✓ Done! Total analyzed: {len(success)}/{len(all_voices['voices'])}")
    
    male = len([r for r in success if r['gender'] == 'male'])
    female = len([r for r in success if r['gender'] == 'female'])
    ambiguous = len([r for r in success if r['gender'] == 'ambiguous'])
    
    print(f"  Male: {male}, Female: {female}, Ambiguous: {ambiguous}")

if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""
Parallel voice pitch analysis using TTS synthesis + pure Python pitch detection
Uses simple FFT-based pitch detection (YIN algorithm approximation)
"""

import json
import urllib.request
import urllib.error
import tempfile
import struct
from pathlib import Path
from concurrent.futures import ProcessPoolExecutor, as_completed
import argparse
import sys
import math
import wave

ROOT = Path("/home/bex/projects/sky/sky-audio")
RESULTS_FILE = ROOT / "voice_pitch_analysis.json"

def read_wav_pcm16(filepath):
    """Read WAV file and return raw PCM16 data and sample rate"""
    with wave.open(filepath, 'rb') as wf:
        nchannels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        framerate = wf.getframerate()
        nframes = wf.getnframes()
        
        if sampwidth != 2:
            raise ValueError(f"Expected 16-bit PCM, got {sampwidth*8}-bit")
        
        raw_data = wf.readframes(nframes)
        # Convert to mono if stereo
        if nchannels == 2:
            samples = struct.unpack(f'{len(raw_data)//2}h', raw_data)
            # Average left and right channels
            mono_samples = []
            for i in range(0, len(samples), 2):
                mono_samples.append((samples[i] + samples[i+1]) // 2)
            return mono_samples, framerate
        else:
            return list(struct.unpack(f'{nframes}h', raw_data)), framerate

def detect_pitch_yin(samples, sample_rate, frame_size=2048, hop_size=512):
    """Simple YIN-based pitch detection"""
    import array  # Use array module for memory efficiency
    
    pitches = []
    
    # Process audio in overlapping frames
    for start in range(0, len(samples) - frame_size, hop_size):
        frame = samples[start:start + frame_size]
        
        # Normalize frame
        max_val = max(abs(x) for x in frame) if frame else 0
        if max_val == 0:
            continue
        
        # YIN difference function
        tau_max = min(frame_size // 2, int(sample_rate / 50))  # Min 50 Hz
        tau_min = int(sample_rate / 400)  # Max 400 Hz
        
        # Calculate difference function
        differences = []
        for tau in range(tau_min, tau_max):
            diff = sum((frame[i] - frame[i + tau]) ** 2 for i in range(frame_size - tau))
            differences.append((tau, diff))
        
        if not differences:
            continue
        
        # Cumulative mean normalized difference
        cmnd = []
        for i, (tau, diff) in enumerate(differences):
            if i == 0:
                cmnd.append((tau, 1.0))
            else:
                running_sum = sum(d[1] for d in differences[:i+1])
                cmnd_val = diff / (running_sum / (i + 1)) if running_sum > 0 else 1.0
                cmnd.append((tau, cmnd_val))
        
        # Find best tau (minimum CMND below threshold)
        threshold = 0.5
        best_tau = None
        best_cmnd = float('inf')
        
        for tau, cmnd_val in cmnd:
            if cmnd_val < threshold and cmnd_val < best_cmnd:
                best_tau = tau
                best_cmnd = cmnd_val
        
        if best_tau:
            pitch = sample_rate / best_tau
            if 50 < pitch < 400:  # Valid pitch range
                pitches.append(pitch)
    
    return pitches

def detect_pitch_autocorr(samples, sample_rate, frame_size=2048, hop_size=512):
    """Simple autocorrelation-based pitch detection (faster than YIN)"""
    pitches = []
    
    # Process audio in overlapping frames
    for start in range(0, len(samples) - frame_size, hop_size):
        frame = samples[start:start + frame_size]
        
        # Normalize and apply window
        max_val = max(abs(x) for x in frame) if frame else 0
        if max_val < 100:  # Skip silent frames
            continue
        
        # Simple center clipping
        threshold = max_val * 0.3
        clipped = [x if abs(x) > threshold else 0 for x in frame]
        
        # Autocorrelation
        tau_min = int(sample_rate / 400)  # Max 400 Hz
        tau_max = min(frame_size // 2, int(sample_rate / 50))  # Min 50 Hz
        
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
    """Analyze pitch using pure Python implementation"""
    try:
        samples, sample_rate = read_wav_pcm16(wav_file)
        
        # Use autocorrelation method (faster than YIN)
        pitch_values = detect_pitch_autocorr(samples, sample_rate)
        
        if len(pitch_values) < 10:
            return {'error': 'insufficient voiced segments detected'}
        
        # Sort for percentile calculations
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

def synthesize_voice(engine, voice_name, text, output_wav):
    """Synthesize text using sky-audio API"""
    try:
        url = "http://localhost:8585/v1/audio/speech"
        model = "sky-pocket" if engine == "pocket" else "sky-kyutai"
        data = json.dumps({
            "model": model,
            "input": text,
            "voice": voice_name,
            "response_format": "wav"
        }).encode()
        
        headers = {
            "Content-Type": "application/json",
            "Accept": "audio/wav"
        }
        
        req = urllib.request.Request(url, data=data, headers=headers)
        with urllib.request.urlopen(req, timeout=30) as response:
            with open(output_wav, 'wb') as f:
                f.write(response.read())
        return True
    except Exception as e:
        return f"synthesis error: {e}"

def classify_gender(median_f0):
    """Classify gender based on median pitch with confidence"""
    # Refined thresholds based on typical ranges
    # Male: 85-180 Hz (avg ~115)
    # Female: 165-255 Hz (avg ~210)
    # Overlap zone: 165-180 Hz
    
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

def analyze_voice(args):
    """Analyze a single voice"""
    engine, voice_path = args
    voice_name = voice_path.replace('/', '_')
    
    with tempfile.TemporaryDirectory() as tmpdir:
        wav_file = Path(tmpdir) / f"{voice_name}.wav"
        
        # Synthesize - use shorter text for speed
        text = "Hello world. The quick brown fox jumps over the lazy dog."
        result = synthesize_voice(engine, voice_path, text, str(wav_file))
        
        if result is not True:
            return {'voice': voice_path, 'engine': engine, 'error': str(result)}
        
        # Analyze pitch
        pitch = analyze_pitch_python(str(wav_file))
        
        if 'error' in pitch:
            return {'voice': voice_path, 'engine': engine, 'error': pitch['error']}
        
        # Classify
        gender, confidence = classify_gender(pitch['median'])
        
        return {
            'voice': voice_path,
            'engine': engine,
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
    parser = argparse.ArgumentParser(description='Analyze voice pitches using TTS synthesis')
    parser.add_argument('--threads', type=int, default=4, help='Number of parallel workers (default: 4)')
    parser.add_argument('--limit', type=int, default=None, help='Limit voices for testing')
    parser.add_argument('--engine', choices=['pocket', 'kyutai', 'all'], default='all',
                       help='Which engine voices to analyze')
    args = parser.parse_args()
    
    # Load voice catalogs
    tasks = []
    
    if args.engine in ('pocket', 'all'):
        pocket_voices = json.loads((ROOT / "workers/python-pocket-tts/voices.json").read_text())
        for v in pocket_voices['voices']:
            tasks.append(('pocket', v))
    
    if args.engine in ('kyutai', 'all'):
        kyutai_voices = json.loads((ROOT / "workers/python-kyutai-tts/voices.json").read_text())
        for v in kyutai_voices['voices']:
            tasks.append(('kyutai', v))
    
    if args.limit:
        tasks = tasks[:args.limit]
    
    print(f"Analyzing {len(tasks)} voices with {args.threads} threads...")
    print(f"Results will be saved to: {RESULTS_FILE}")
    print()
    
    # Process in parallel
    results = []
    completed = 0
    
    with ProcessPoolExecutor(max_workers=args.threads) as executor:
        futures = {executor.submit(analyze_voice, task): task for task in tasks}
        
        for future in as_completed(futures):
            completed += 1
            try:
                result = future.result(timeout=120)
                results.append(result)
                
                if 'error' in result:
                    print(f"[{completed}/{len(tasks)}] {result['voice']}: ERROR - {result['error']}")
                else:
                    gender = result['gender']
                    pitch = result['pitch_median_hz']
                    conf = result['gender_confidence']
                    print(f"[{completed}/{len(tasks)}] {result['voice']}: {gender} ({pitch} Hz, {conf} confidence)")
                
                # Save progress every 10 voices
                if completed % 10 == 0:
                    RESULTS_FILE.write_text(json.dumps(results, indent=2))
                    print(f"  ✓ Progress saved ({completed}/{len(tasks)})")
                    
            except Exception as e:
                task = futures[future]
                print(f"[{completed}/{len(tasks)}] {task[1]}: EXCEPTION - {e}")
                results.append({'voice': task[1], 'engine': task[0], 'error': str(e)})
    
    # Final save
    RESULTS_FILE.write_text(json.dumps(results, indent=2))
    print(f"\n✓ Done! Results saved to: {RESULTS_FILE}")
    
    # Summary
    success = [r for r in results if 'error' not in r]
    errors = [r for r in results if 'error' in r]
    
    print(f"\nSummary:")
    print(f"  Successful: {len(success)}/{len(results)}")
    print(f"  Errors: {len(errors)}/{len(results)}")
    
    if success:
        male_high = len([r for r in success if r['gender'] == 'male' and r['gender_confidence'] == 'high'])
        male_med = len([r for r in success if r['gender'] == 'male' and r['gender_confidence'] == 'medium'])
        female_high = len([r for r in success if r['gender'] == 'female' and r['gender_confidence'] == 'high'])
        female_med = len([r for r in success if r['gender'] == 'female' and r['gender_confidence'] == 'medium'])
        ambiguous = len([r for r in success if r['gender'] == 'ambiguous'])
        
        print(f"\n  Gender classification (pitch-based):")
        print(f"    Male (high confidence):     {male_high}")
        print(f"    Male (medium confidence):   {male_med}")
        print(f"    Female (high confidence):     {female_high}")
        print(f"    Female (medium confidence):   {female_med}")
        print(f"    Ambiguous (overlap zone):     {ambiguous}")
        
        # Pitch statistics
        medians = [r['pitch_median_hz'] for r in success]
        print(f"\n  Pitch statistics:")
        print(f"    Range: {min(medians):.1f} - {max(medians):.1f} Hz")
        print(f"    Mean:  {sum(medians)/len(medians):.1f} Hz")

if __name__ == "__main__":
    main()

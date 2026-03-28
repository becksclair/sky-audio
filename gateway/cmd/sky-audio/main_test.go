package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type fakeSynthesizer struct {
	pcm        []byte
	sampleRate int32
	err        error
	lastText   string
}

func (f *fakeSynthesizer) Synthesize(_ context.Context, text string) ([]byte, int32, error) {
	f.lastText = text
	if f.err != nil {
		return nil, 0, f.err
	}
	return append([]byte(nil), f.pcm...), f.sampleRate, nil
}

func TestTTSHandlerWithSynthRejectsOversizedBody(t *testing.T) {
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{})
	body := `{"text":"` + strings.Repeat("a", maxTTSBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(rr.Body.String(), "request body too large") {
		t.Fatalf("body = %q, want oversized-body message", rr.Body.String())
	}
}

func TestTTSHandlerWithSynthRejectsInvalidJSON(t *testing.T) {
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{})
	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(`{"text":`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "invalid JSON body") {
		t.Fatalf("body = %q, want invalid-json message", rr.Body.String())
	}
}

func TestTTSHandlerWithSynthRejectsEmptyText(t *testing.T) {
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{"pocket": &fakeSynthesizer{}})
	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(`{"text":"   "}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "text is required") {
		t.Fatalf("body = %q, want empty-text message", rr.Body.String())
	}
}

func TestTTSHandlerWithSynthRejectsTextTooLong(t *testing.T) {
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{"pocket": &fakeSynthesizer{}})
	body := `{"text":"` + strings.Repeat("🙂", maxTTSTextLen+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "text too long") {
		t.Fatalf("body = %q, want text-too-long message", rr.Body.String())
	}
}

func TestTTSHandlerWithSynthRejectsUnknownEngine(t *testing.T) {
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{"pocket": &fakeSynthesizer{}})
	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(`{"text":"hi","engine":"ghost"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "unknown engine") {
		t.Fatalf("body = %q, want unknown-engine message", rr.Body.String())
	}
}

func TestTTSHandlerWithSynthHandlesSynthesizeError(t *testing.T) {
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{
		"pocket": &fakeSynthesizer{err: errors.New("boom")},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(`{"text":"hi"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(rr.Body.String(), "synthesis failed") {
		t.Fatalf("body = %q, want synth-failed message", rr.Body.String())
	}
}

func TestTTSHandlerWithSynthUsesDefaultEngineAndWritesWAV(t *testing.T) {
	synth := &fakeSynthesizer{
		pcm:        []byte{0x01, 0x00, 0x02, 0x00},
		sampleRate: 16000,
	}
	handler := ttsHandlerWithSynth(map[string]ttsSynthesizer{
		"pocket": synth,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/tts/synthesize", strings.NewReader(`{"text":"hi"}`))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	res := rr.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want %q", got, "audio/wav")
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got, want := res.Header.Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
	if len(body) < 44 {
		t.Fatalf("WAV body too short: %d bytes", len(body))
	}
	if string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("response is not a WAV file: header=%q", body[:12])
	}
	if synth.lastText != "hi" {
		t.Fatalf("synth text = %q, want %q", synth.lastText, "hi")
	}
}

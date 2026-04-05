package realtimeevents

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

type EventSink func(event map[string]any) error

type TranscriptEmitter struct {
	mu        sync.Mutex
	sessionID string
	sink      EventSink
	current   *inputItem
}

type inputItem struct {
	id        string
	committed bool
	lastText  string
	closed    bool
}

func NewTranscriptEmitter(sessionID string) *TranscriptEmitter {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = randomID("sess")
	}
	return &TranscriptEmitter{sessionID: sessionID}
}

func (e *TranscriptEmitter) SessionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessionID
}

func (e *TranscriptEmitter) SetSink(sink EventSink) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sink = sink
}

func (e *TranscriptEmitter) EmitSessionCreated() error {
	return e.emit(map[string]any{
		"type": "session.created",
		"session": map[string]any{
			"id":   e.SessionID(),
			"type": "transcription",
		},
	})
}

func (e *TranscriptEmitter) HandleTranscript(text string, isFinal bool) error {
	text = strings.TrimSpace(text)
	if text == "" && !isFinal {
		return nil
	}

	e.mu.Lock()
	item := e.current
	if item == nil {
		if text == "" && !isFinal {
			e.mu.Unlock()
			return nil
		}
		item = &inputItem{id: randomID("item")}
		e.current = item
	}
	committed := item.committed
	lastText := item.lastText
	itemID := item.id
	if isFinal {
		item.closed = true
		e.current = nil
	} else {
		item.lastText = text
	}
	if !committed {
		item.committed = true
	}
	e.mu.Unlock()

	if !committed {
		if err := e.emit(map[string]any{
			"type":    "input_audio_buffer.committed",
			"item_id": itemID,
		}); err != nil {
			return err
		}
	}

	if isFinal {
		return e.emit(map[string]any{
			"type":       "conversation.item.input_audio_transcription.completed",
			"item_id":    itemID,
			"transcript": text,
		})
	}

	if text == lastText {
		return nil
	}
	if !strings.HasPrefix(text, lastText) {
		return nil
	}
	delta := strings.TrimPrefix(text, lastText)
	return e.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.delta",
		"item_id": itemID,
		"delta":   delta,
	})
}

func (e *TranscriptEmitter) emit(event map[string]any) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	if sink == nil {
		return nil
	}
	return sink(event)
}

func randomID(prefix string) string {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}

package openaihttp

import (
	"encoding/base64"
	"strings"
	"sync"
)

type RealtimeEventSink func(event map[string]any) error

type RealtimeTranscriptionCore struct {
	mu      sync.Mutex
	session RealtimeSession
	sink    RealtimeEventSink
}

func NewRealtimeTranscriptionCore(sessionID string, sink RealtimeEventSink) *RealtimeTranscriptionCore {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = randomID("sess")
	}
	return &RealtimeTranscriptionCore{
		session: RealtimeSession{ID: sessionID, Type: "transcription"},
		sink:    sink,
	}
}

func (c *RealtimeTranscriptionCore) Snapshot() RealtimeSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

func (c *RealtimeTranscriptionCore) UpdateSession(session *RealtimeSession) error {
	if err := validateRealtimeSession(session); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(session.Type) != "" {
		c.session.Type = strings.TrimSpace(session.Type)
	}
	if session.Audio != nil {
		c.session.Audio = cloneRealtimeSessionAudio(session.Audio)
	}
	if session.Include != nil {
		c.session.Include = append([]string(nil), session.Include...)
	}
	return nil
}

func (c *RealtimeTranscriptionCore) EmitSessionCreated() error {
	return c.emit(map[string]any{
		"type":    "session.created",
		"session": c.Snapshot(),
	})
}

func (c *RealtimeTranscriptionCore) NewInputItem() *RealtimeInputItem {
	return &RealtimeInputItem{
		core:   c,
		itemID: randomID("item"),
	}
}

func (c *RealtimeTranscriptionCore) NewResponse() *RealtimeResponse {
	return c.NewResponseWithID("")
}

func (c *RealtimeTranscriptionCore) NewResponseWithID(responseID string) *RealtimeResponse {
	if strings.TrimSpace(responseID) == "" {
		responseID = randomID("resp")
	}
	return &RealtimeResponse{
		core:       c,
		responseID: responseID,
	}
}

func (c *RealtimeTranscriptionCore) SetSink(sink RealtimeEventSink) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sink = sink
}

func cloneRealtimeSessionAudio(audioCfg *RealtimeSessionAudio) *RealtimeSessionAudio {
	if audioCfg == nil {
		return nil
	}
	cloned := &RealtimeSessionAudio{}
	if audioCfg.Input != nil {
		cloned.Input = &RealtimeSessionAudioInput{}
		if audioCfg.Input.Format != nil {
			format := *audioCfg.Input.Format
			cloned.Input.Format = &format
		}
	}
	return cloned
}

func (c *RealtimeTranscriptionCore) emit(event map[string]any) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	sink := c.sink
	c.mu.Unlock()
	if sink == nil {
		return nil
	}
	return sink(event)
}

type RealtimeResponse struct {
	mu         sync.Mutex
	core       *RealtimeTranscriptionCore
	responseID string
	done       bool
}

func (r *RealtimeResponse) ID() string {
	return r.responseID
}

func (r *RealtimeResponse) EmitCreated() error {
	return r.core.emit(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     r.responseID,
			"status": "in_progress",
			"output": []any{},
		},
	})
}

func (r *RealtimeResponse) NewOutputItem() *RealtimeOutputItem {
	return r.NewOutputItemWithID("")
}

func (r *RealtimeResponse) NewOutputItemWithID(itemID string) *RealtimeOutputItem {
	if strings.TrimSpace(itemID) == "" {
		itemID = randomID("item")
	}
	return &RealtimeOutputItem{
		core:       r.core,
		responseID: r.responseID,
		itemID:     itemID,
	}
}

func (r *RealtimeResponse) EmitDone(status string) error {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return nil
	}
	r.done = true
	responseID := r.responseID
	core := r.core
	r.mu.Unlock()
	return core.emit(map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id":     responseID,
			"status": status,
		},
	})
}

type RealtimeOutputItem struct {
	mu         sync.Mutex
	core       *RealtimeTranscriptionCore
	responseID string
	itemID     string
	added      bool
	audioDone  bool
	textDone   bool
}

func (i *RealtimeOutputItem) ID() string {
	return i.itemID
}

func (i *RealtimeOutputItem) EmitAdded() error {
	i.mu.Lock()
	if i.added {
		i.mu.Unlock()
		return nil
	}
	i.added = true
	responseID := i.responseID
	itemID := i.itemID
	core := i.core
	i.mu.Unlock()
	return core.emit(map[string]any{
		"type":         "response.output_item.added",
		"response_id":  responseID,
		"output_index": 0,
		"item": map[string]any{
			"id":     itemID,
			"type":   "message",
			"role":   "assistant",
			"status": "in_progress",
		},
	})
}

func (i *RealtimeOutputItem) EmitAudioDelta(pcm []byte, sampleRate int32) error {
	if len(pcm) == 0 {
		return nil
	}
	if err := i.EmitAdded(); err != nil {
		return err
	}
	i.mu.Lock()
	responseID := i.responseID
	itemID := i.itemID
	core := i.core
	i.mu.Unlock()
	return core.emit(map[string]any{
		"type":           "response.audio.delta",
		"response_id":    responseID,
		"item_id":        itemID,
		"output_index":   0,
		"content_index":  0,
		"audio_format":   "pcm16",
		"sample_rate_hz": sampleRate,
		"delta":          base64.StdEncoding.EncodeToString(pcm),
	})
}

func (i *RealtimeOutputItem) EmitAudioDone() error {
	if err := i.EmitAdded(); err != nil {
		return err
	}
	i.mu.Lock()
	if i.audioDone {
		i.mu.Unlock()
		return nil
	}
	i.audioDone = true
	responseID := i.responseID
	itemID := i.itemID
	core := i.core
	i.mu.Unlock()
	return core.emit(map[string]any{
		"type":          "response.audio.done",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
	})
}

func (i *RealtimeOutputItem) EmitTextDelta(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if err := i.EmitAdded(); err != nil {
		return err
	}
	i.mu.Lock()
	responseID := i.responseID
	itemID := i.itemID
	core := i.core
	i.mu.Unlock()
	return core.emit(map[string]any{
		"type":          "response.output_text.delta",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         text,
	})
}

func (i *RealtimeOutputItem) EmitTextDone(text string) error {
	if err := i.EmitAdded(); err != nil {
		return err
	}
	i.mu.Lock()
	if i.textDone {
		i.mu.Unlock()
		return nil
	}
	i.textDone = true
	responseID := i.responseID
	itemID := i.itemID
	core := i.core
	i.mu.Unlock()
	return core.emit(map[string]any{
		"type":          "response.output_text.done",
		"response_id":   responseID,
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"text":          strings.TrimSpace(text),
	})
}

type RealtimeInputItem struct {
	mu        sync.Mutex
	core      *RealtimeTranscriptionCore
	itemID    string
	committed bool
	lastText  string
	closed    bool
}

func (i *RealtimeInputItem) ID() string {
	return i.itemID
}

func (i *RealtimeInputItem) EmitCommitted() error {
	i.mu.Lock()
	if i.closed || i.committed {
		i.mu.Unlock()
		return nil
	}
	i.committed = true
	itemID := i.itemID
	core := i.core
	i.mu.Unlock()
	return core.emit(map[string]any{
		"type":    "input_audio_buffer.committed",
		"item_id": itemID,
	})
}

func (i *RealtimeInputItem) HandleTranscript(text string, isFinal bool) error {
	text = strings.TrimSpace(text)
	if text == "" && !isFinal {
		return nil
	}

	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	committed := i.committed
	lastText := i.lastText
	itemID := i.itemID
	core := i.core
	if isFinal {
		i.closed = true
	} else {
		i.lastText = text
	}
	if !committed {
		i.committed = true
	}
	i.mu.Unlock()

	if !committed {
		if err := core.emit(map[string]any{
			"type":    "input_audio_buffer.committed",
			"item_id": itemID,
		}); err != nil {
			return err
		}
	}

	if isFinal {
		return core.emit(map[string]any{
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
	return core.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.delta",
		"item_id": itemID,
		"delta":   delta,
	})
}

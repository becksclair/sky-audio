package openaihttp

import (
	"testing"
)

func TestRealtimeCoreEmitsSessionCreated(t *testing.T) {
	var events []map[string]any
	core := NewRealtimeTranscriptionCore("sess_test", func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	if err := core.EmitSessionCreated(); err != nil {
		t.Fatalf("emit session.created: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0]["type"] != "session.created" {
		t.Fatalf("expected session.created, got %#v", events[0])
	}
}

func TestRealtimeInputItemEmitsCommittedDeltaAndCompleted(t *testing.T) {
	var events []map[string]any
	core := NewRealtimeTranscriptionCore("sess_test", func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	item := core.NewInputItem()
	if err := item.EmitCommitted(); err != nil {
		t.Fatalf("emit committed: %v", err)
	}
	if err := item.HandleTranscript("hello", false); err != nil {
		t.Fatalf("emit delta: %v", err)
	}
	if err := item.HandleTranscript("hello world", true); err != nil {
		t.Fatalf("emit completed: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0]["type"] != "input_audio_buffer.committed" {
		t.Fatalf("expected committed event, got %#v", events[0])
	}
	if events[1]["type"] != "conversation.item.input_audio_transcription.delta" {
		t.Fatalf("expected delta event, got %#v", events[1])
	}
	if events[1]["delta"] != "hello" {
		t.Fatalf("expected delta hello, got %#v", events[1]["delta"])
	}
	if events[2]["type"] != "conversation.item.input_audio_transcription.completed" {
		t.Fatalf("expected completed event, got %#v", events[2])
	}
	if events[2]["transcript"] != "hello world" {
		t.Fatalf("expected completed transcript, got %#v", events[2]["transcript"])
	}
}

func TestRealtimeInputItemAutoCommitsOnTranscript(t *testing.T) {
	var events []map[string]any
	core := NewRealtimeTranscriptionCore("sess_test", func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	item := core.NewInputItem()
	if err := item.HandleTranscript("hello", false); err != nil {
		t.Fatalf("emit delta: %v", err)
	}
	if err := item.HandleTranscript("hello world", true); err != nil {
		t.Fatalf("emit completed: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0]["type"] != "input_audio_buffer.committed" {
		t.Fatalf("expected implicit committed event, got %#v", events[0])
	}
	if events[1]["type"] != "conversation.item.input_audio_transcription.delta" {
		t.Fatalf("expected delta event, got %#v", events[1])
	}
	if events[2]["type"] != "conversation.item.input_audio_transcription.completed" {
		t.Fatalf("expected completed event, got %#v", events[2])
	}
}

func TestRealtimeInputItemEmitsEmptyFinalTranscript(t *testing.T) {
	var events []map[string]any
	core := NewRealtimeTranscriptionCore("sess_test", func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	item := core.NewInputItem()
	if err := item.HandleTranscript("", true); err != nil {
		t.Fatalf("emit empty final transcript: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0]["type"] != "input_audio_buffer.committed" {
		t.Fatalf("expected committed event, got %#v", events[0])
	}
	if events[1]["type"] != "conversation.item.input_audio_transcription.completed" {
		t.Fatalf("expected completed event, got %#v", events[1])
	}
	if events[1]["transcript"] != "" {
		t.Fatalf("expected empty transcript, got %#v", events[1]["transcript"])
	}
}

func TestRealtimeResponseEmitsTextLifecycle(t *testing.T) {
	var events []map[string]any
	core := NewRealtimeTranscriptionCore("sess_test", func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	response := core.NewResponseWithID("resp_test")
	item := response.NewOutputItemWithID("item_test")
	if err := item.EmitTextDelta("hello world"); err != nil {
		t.Fatalf("emit response.output_text.delta: %v", err)
	}
	if err := item.EmitTextDone("hello world"); err != nil {
		t.Fatalf("emit response.output_text.done: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0]["type"] != "response.output_item.added" {
		t.Fatalf("expected response.output_item.added, got %#v", events[0])
	}
	if events[1]["type"] != "response.output_text.delta" {
		t.Fatalf("expected response.output_text.delta, got %#v", events[1])
	}
	if events[2]["type"] != "response.output_text.done" {
		t.Fatalf("expected response.output_text.done, got %#v", events[2])
	}
}

func TestRealtimeResponseEmitsAudioLifecycle(t *testing.T) {
	var events []map[string]any
	core := NewRealtimeTranscriptionCore("sess_test", func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	response := core.NewResponse()
	if err := response.EmitCreated(); err != nil {
		t.Fatalf("emit response.created: %v", err)
	}
	item := response.NewOutputItem()
	if err := item.EmitAdded(); err != nil {
		t.Fatalf("emit response.output_item.added: %v", err)
	}
	if err := item.EmitAudioDelta([]byte{0x01, 0x02}, 16000); err != nil {
		t.Fatalf("emit response.audio.delta: %v", err)
	}
	if err := item.EmitAudioDone(); err != nil {
		t.Fatalf("emit response.audio.done: %v", err)
	}
	if err := response.EmitDone("completed"); err != nil {
		t.Fatalf("emit response.done: %v", err)
	}

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	if events[0]["type"] != "response.created" {
		t.Fatalf("expected response.created, got %#v", events[0])
	}
	if events[1]["type"] != "response.output_item.added" {
		t.Fatalf("expected response.output_item.added, got %#v", events[1])
	}
	if events[2]["type"] != "response.audio.delta" {
		t.Fatalf("expected response.audio.delta, got %#v", events[2])
	}
	if events[2]["sample_rate_hz"] != int32(16000) {
		t.Fatalf("expected sample rate 16000, got %#v", events[2]["sample_rate_hz"])
	}
	if events[3]["type"] != "response.audio.done" {
		t.Fatalf("expected response.audio.done, got %#v", events[3])
	}
	if events[4]["type"] != "response.done" {
		t.Fatalf("expected response.done, got %#v", events[4])
	}
	responsePayload, ok := events[4]["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected response payload map, got %#v", events[4]["response"])
	}
	if responsePayload["status"] != "completed" {
		t.Fatalf("expected completed status, got %#v", responsePayload["status"])
	}
}

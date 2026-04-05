package realtimeevents

import "testing"

func TestTranscriptEmitterDuplication(t *testing.T) {
	var events []map[string]any
	emitter := NewTranscriptEmitter("sess_test")
	emitter.SetSink(func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	if err := emitter.HandleTranscript("hello", false); err != nil {
		t.Fatalf("first transcript: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events after first transcript, got %d", len(events))
	}

	if err := emitter.HandleTranscript("hello", false); err != nil {
		t.Fatalf("duplicate transcript: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected duplicate transcript to emit no new events, got %d", len(events))
	}
}

func TestTranscriptEmitterEmitsEmptyFinalTranscript(t *testing.T) {
	var events []map[string]any
	emitter := NewTranscriptEmitter("sess_test")
	emitter.SetSink(func(event map[string]any) error {
		events = append(events, event)
		return nil
	})

	if err := emitter.HandleTranscript("", true); err != nil {
		t.Fatalf("empty final transcript: %v", err)
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

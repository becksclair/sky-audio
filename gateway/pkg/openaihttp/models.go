package openaihttp

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Model represents an OpenAI-compatible model entry
type Model struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Created    int64  `json:"created"`
	OwnedBy    string `json:"owned_by"`
	Permission []any  `json:"permission,omitempty"`
}

// ModelsListResponse matches OpenAI's /v1/models response
type ModelsListResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Static model catalog - exposes sky-audio engines as OpenAI-compatible models
var availableModels = []Model{
	{
		ID:      "sky-kyutai",
		Object:  "model",
		Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy: "sky-audio",
	},
	{
		ID:      "sky-pocket",
		Object:  "model",
		Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy: "sky-audio",
	},
	{
		ID:      "sky-nemo-parakeet",
		Object:  "model",
		Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy: "sky-audio",
	},
	// OpenAI-compatible aliases
	{
		ID:      "tts-1",
		Object:  "model",
		Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy: "sky-audio",
	},
	{
		ID:      "tts-1-hd",
		Object:  "model",
		Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy: "sky-audio",
	},
	{
		ID:      "whisper-1",
		Object:  "model",
		Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix(),
		OwnedBy: "sky-audio",
	},
}

// ModelsHandler returns the list of available models in OpenAI format
func ModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeTTSError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only GET requests are supported")
			return
		}

		resp := ModelsListResponse{
			Object: "list",
			Data:   availableModels,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("[models] failed to encode response: %v", err)
		}
	}
}

// ModelsRetrieveHandler returns a specific model by ID
func ModelsRetrieveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeTTSError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only GET requests are supported")
			return
		}

		// Extract model ID from path: /v1/models/{id}
		modelID := r.PathValue("model")
		if modelID == "" {
			// Fallback to URL parsing if PathValue not available
			path := r.URL.Path
			const prefix = "/v1/models/"
			if len(path) > len(prefix) {
				modelID = path[len(prefix):]
			}
		}

		if modelID == "" {
			writeTTSError(w, http.StatusBadRequest, "invalid_request_error", "Model ID is required")
			return
		}

		// Find model
		for _, model := range availableModels {
			if model.ID == modelID {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if err := json.NewEncoder(w).Encode(model); err != nil {
					log.Printf("[models] failed to encode model: %v", err)
				}
				return
			}
		}

		writeTTSError(w, http.StatusNotFound, "invalid_request_error", "The model '"+modelID+"' does not exist")
	}
}

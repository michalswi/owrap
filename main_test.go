package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebSessionAppendMessageUpdatesStatsAndContext(t *testing.T) {
	previousPrompt := systemPrompt
	systemPrompt = "system"
	t.Cleanup(func() { systemPrompt = previousPrompt })

	sess := &webSession{}
	sess.appendMessage("user", "hello")
	sess.appendMessage("assistant", "world!")

	if sess.Stats.UserMessages != 1 || sess.Stats.AssistantMessages != 1 {
		t.Fatalf("unexpected message counts: %+v", sess.Stats)
	}
	if sess.Stats.TotalUserChars != 5 || sess.Stats.TotalAssistantChars != 6 {
		t.Fatalf("unexpected character counts: %+v", sess.Stats)
	}
	wantContextChars := len("systemhelloworld!")
	if sess.Stats.ContextChars != wantContextChars {
		t.Fatalf("ContextChars = %d, want %d", sess.Stats.ContextChars, wantContextChars)
	}
	if sess.Stats.EstimatedContextTokens != (wantContextChars+3)/4 {
		t.Fatalf("EstimatedContextTokens = %d", sess.Stats.EstimatedContextTokens)
	}
}

func TestHandleWebChatCountsRawReplyAndCommandOutput(t *testing.T) {
	tests := []struct {
		name             string
		ollamaContent    string
		wantCommands     int
		wantAssistantMin int
	}{
		{name: "raw reply", ollamaContent: "plain reply", wantAssistantMin: 1},
		{name: "command output", ollamaContent: `{"action":"run_command","command":"echo stats-test"}`, wantCommands: 1, wantAssistantMin: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: test.ollamaContent}})
			}))
			defer ollama.Close()

			previousURL, previousStore := ollamaURL, webStore
			ollamaURL, webStore = ollama.URL, newWebSessionStore()
			t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

			body := bytes.NewBufferString(`{"message":"test request"}`)
			req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
			response := httptest.NewRecorder()
			handleWebChat(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}

			var payload webChatResponse
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Stats.UserMessages != 1 || payload.Stats.AssistantMessages < test.wantAssistantMin {
				t.Fatalf("unexpected message stats: %+v", payload.Stats)
			}
			if payload.Stats.CommandsRun != test.wantCommands {
				t.Fatalf("CommandsRun = %d, want %d", payload.Stats.CommandsRun, test.wantCommands)
			}
			if payload.Stats.ContextChars == 0 || payload.Stats.EstimatedContextTokens == 0 {
				t.Fatalf("context estimate not populated: %+v", payload.Stats)
			}
		})
	}
}

func TestAutonomousFailureMessagesAreCounted(t *testing.T) {
	previousPrompt := systemPrompt
	systemPrompt = "autonomous"
	t.Cleanup(func() { systemPrompt = previousPrompt })

	sess := &webSession{AutonomousMode: true, RetryCount: 2}
	response := handleAutonomousJSONError(sess, "invalid", "invalid", nil)
	if response.Stats.AssistantMessages != 2 {
		t.Fatalf("AssistantMessages = %d, want 2", response.Stats.AssistantMessages)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
}

func TestAutonomousInternalPromptOnlyUpdatesContext(t *testing.T) {
	previousPrompt := systemPrompt
	systemPrompt = "autonomous"
	t.Cleanup(func() { systemPrompt = previousPrompt })

	sess := &webSession{}
	sess.appendContextMessage("user", "continue internally")
	if sess.Stats.UserMessages != 0 || sess.Stats.TotalUserChars != 0 {
		t.Fatalf("internal prompt changed user stats: %+v", sess.Stats)
	}
	if sess.Stats.ContextChars != len("autonomouscontinue internally") {
		t.Fatalf("internal prompt missing from context: %+v", sess.Stats)
	}
}

func TestTerminalMessageAccounting(t *testing.T) {
	previousMessages := sessionMessages
	previousPrompt := systemPrompt
	sessionMessages = nil
	systemPrompt = "system"
	t.Cleanup(func() {
		sessionMessages = previousMessages
		systemPrompt = previousPrompt
	})

	messages := []ChatMessage{{Role: "system", Content: systemPrompt}}
	stats := &Stats{}
	appendTerminalMessage(&messages, stats, "user", "question")
	appendTerminalMessage(&messages, stats, "assistant", "raw fallback")
	if stats.UserMessages != 1 || stats.AssistantMessages != 1 {
		t.Fatalf("unexpected terminal stats: %+v", stats)
	}
	if len(sessionMessages) != 2 {
		t.Fatalf("sessionMessages = %d, want 2", len(sessionMessages))
	}
}

func TestStatsTimingAndSessionPersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stats := Stats{UserMessages: 1, ContextChars: 100, EstimatedContextTokens: 25}
	stats.recordResponseDuration(1250 * time.Millisecond)
	session := Session{Model: "test", Messages: []ChatMessage{{Role: "user", Content: "hello"}}, Stats: stats}

	path, err := saveSessionToFile(session, "stats-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "stats-test.json") {
		t.Fatalf("unexpected path: %s", path)
	}
	loaded, err := loadSessionFromFile("stats-test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Stats.LastResponseMillis != 1250 || loaded.Stats.EstimatedContextTokens != 25 {
		t.Fatalf("stats not preserved: %+v", loaded.Stats)
	}
}

func TestWebStatePersistsAcrossClientsAndRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previousStore := webStore
	t.Cleanup(func() { webStore = previousStore })

	webStore = newWebSessionStore()
	original := webStore.ensure("")
	original.appendMessage("user", "shared question")
	original.appendMessage("assistant", "shared answer")
	if err := webStore.persist(); err != nil {
		t.Fatal(err)
	}

	restartedStore := newWebSessionStore()
	if err := restartedStore.load(); err != nil {
		t.Fatal(err)
	}
	webStore = restartedStore

	response := httptest.NewRecorder()
	handleWebState(response, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	var restored webStateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.SessionID != original.ID || len(restored.Messages) != 2 {
		t.Fatalf("state not restored: %+v", restored)
	}

	resetResponse := httptest.NewRecorder()
	handleWebState(resetResponse, httptest.NewRequest(http.MethodDelete, "/api/state", nil))
	if resetResponse.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body = %s", resetResponse.Code, resetResponse.Body.String())
	}
	current := webStore.ensure(original.ID)
	if current.ID == original.ID || len(current.Messages) != 0 {
		t.Fatalf("shared reset did not replace active session: %+v", current)
	}
}

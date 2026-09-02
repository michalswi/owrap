package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func writeAgentCriticApproval(w http.ResponseWriter, r *http.Request) bool {
	var request ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
		return false
	}
	if !strings.Contains(request.Messages[0].Content, "You are an independent critic") {
		return false
	}
	var evidenceIDs []int
	if len(request.Messages) > 1 {
		const marker = "RECORDED EVENTS:\n"
		if index := strings.Index(request.Messages[1].Content, marker); index >= 0 {
			var events []AgentEvent
			if json.Unmarshal([]byte(request.Messages[1].Content[index+len(marker):]), &events) == nil {
				for _, event := range events {
					if event.Kind == "observation" && event.Success {
						evidenceIDs = append(evidenceIDs, event.Sequence)
					}
				}
			}
		}
	}
	verification, _ := json.Marshal(AgentVerification{Approved: true, EvidenceEventIDs: evidenceIDs})
	writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: string(verification)}})
	return true
}

func TestCallOllamaThinkingProtocol(t *testing.T) {
	tests := []struct {
		name         string
		think        bool
		thinkingText string
	}{
		{name: "disabled by default"},
		{name: "enabled with reasoning", think: true, thinkingText: "reasoning trace"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received ChatRequest
			ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Fatal(err)
				}
				_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: "answer", Thinking: test.thinkingText}})
			}))
			defer ollama.Close()

			previousURL := ollamaURL
			ollamaURL = ollama.URL
			t.Cleanup(func() { ollamaURL = previousURL })

			message, err := callOllamaMessageContext(context.Background(), []ChatMessage{{Role: "user", Content: "question"}}, false, test.think)
			if err != nil {
				t.Fatal(err)
			}
			if received.Think != test.think {
				t.Fatalf("request think = %v, want %v", received.Think, test.think)
			}
			if message.Thinking != test.thinkingText || message.Content != "answer" {
				t.Fatalf("unexpected response message: %+v", message)
			}
		})
	}
}

func TestWebThinkingToggleAndResponsePersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var received ChatRequest
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{
			Role: "assistant", Content: `{"action":"answer","text":"final answer"}`, Thinking: "reasoning trace",
		}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	toggleResponse := httptest.NewRecorder()
	handleWebChat(toggleResponse, httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"/think-on"}`)))
	if toggleResponse.Code != http.StatusOK {
		t.Fatalf("toggle status = %d", toggleResponse.Code)
	}
	var toggled webChatResponse
	if err := json.Unmarshal(toggleResponse.Body.Bytes(), &toggled); err != nil {
		t.Fatal(err)
	}
	if !toggled.ThinkingEnabled {
		t.Fatal("thinking was not enabled")
	}

	chatResponse := httptest.NewRecorder()
	body := bytes.NewBufferString(fmt.Sprintf(`{"sessionId":%q,"message":"question"}`, toggled.SessionID))
	handleWebChat(chatResponse, httptest.NewRequest(http.MethodPost, "/api/chat", body))
	var payload webChatResponse
	if err := json.Unmarshal(chatResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !received.Think || payload.Thinking != "reasoning trace" || !payload.ThinkingEnabled {
		t.Fatalf("thinking response not preserved: request=%+v response=%+v", received, payload)
	}
	if len(payload.Messages) != 2 || payload.Messages[1].Thinking != "reasoning trace" {
		t.Fatalf("reasoning missing from messages: %+v", payload.Messages)
	}

	restartedStore := newWebSessionStore()
	if err := restartedStore.initializeFresh(); err != nil {
		t.Fatal(err)
	}
	restored, ok := restartedStore.active()
	if !ok || restored.ThinkingEnabled || len(restored.Messages) != 0 {
		t.Fatalf("restart did not create fresh state: %+v", restored)
	}
}

func TestTerminalThinkingCommands(t *testing.T) {
	previous := thinkingEnabled
	thinkingEnabled = false
	t.Cleanup(func() { thinkingEnabled = previous })

	if !handleSlashCommand("/think-on", &Stats{}) || !thinkingEnabled {
		t.Fatal("/think-on did not enable thinking")
	}
	if !handleSlashCommand("/think-off", &Stats{}) || thinkingEnabled {
		t.Fatal("/think-off did not disable thinking")
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

func TestAgentFailureMessagesAreCounted(t *testing.T) {
	previousPrompt := systemPrompt
	systemPrompt = "agent"
	t.Cleanup(func() { systemPrompt = previousPrompt })

	sess := &webSession{AgentMode: true, RetryCount: 2}
	response := handleAgentJSONError(sess, "invalid", "invalid", nil)
	if response.Stats.AssistantMessages != 2 {
		t.Fatalf("AssistantMessages = %d, want 2", response.Stats.AssistantMessages)
	}
	if !response.AgentStop || !sess.AgentMode || response.AgentContinue {
		t.Fatalf("retry exhaustion did not request normal stop cleanup: session=%+v response=%+v", sess, response)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
}

func TestAgentInternalPromptOnlyUpdatesContext(t *testing.T) {
	previousPrompt := systemPrompt
	systemPrompt = "agent"
	t.Cleanup(func() { systemPrompt = previousPrompt })

	sess := &webSession{}
	sess.appendContextMessage("user", "continue internally")
	if sess.Stats.UserMessages != 0 || sess.Stats.TotalUserChars != 0 {
		t.Fatalf("internal prompt changed user stats: %+v", sess.Stats)
	}
	if sess.Stats.ContextChars != len("agentcontinue internally") {
		t.Fatalf("internal prompt missing from context: %+v", sess.Stats)
	}
}

func TestComposeAgentPromptPreservesSelectedPrompt(t *testing.T) {
	prompt, err := composeAgentPrompt(
		"You are a concise philosophy assistant.",
		"Who is God? Short answer.",
		"No previous attempts yet.",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"You are a concise philosophy assistant.",
		"Who is God? Short answer.",
		`"action":"answer"`,
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("composed prompt missing %q", required)
		}
	}
}

func TestAgentRunExcludesEarlierMessages(t *testing.T) {
	sess := &webSession{
		Messages: []ChatMessage{
			{Role: "user", Content: "old agent goal"},
			{Role: "assistant", Content: "old agent result"},
			{Role: "user", Content: "new agent goal"},
		},
		AgentMode:  true,
		AgentStart: 2,
	}

	messages := sess.modelMessages()
	if len(messages) != 1 || messages[0].Content != "new agent goal" {
		t.Fatalf("agent model context contains earlier messages: %+v", messages)
	}
	history := buildIterationHistory(sess)
	if strings.Contains(history, "old agent") || !strings.Contains(history, "new agent goal") {
		t.Fatalf("agent iteration history crossed run boundary: %q", history)
	}
}

func TestAgentAnswerWaitsForUserDecision(t *testing.T) {
	previousPrompt, previousPromptName := systemPrompt, systemPromptName
	systemPrompt, systemPromptName = "agent prompt", "agent.txt"
	t.Cleanup(func() { systemPrompt, systemPromptName = previousPrompt, previousPromptName })

	sess := &webSession{
		AgentMode:          true,
		AgentGoal:          "finish the report",
		OriginalPrompt:     "original prompt",
		OriginalPromptName: "default",
	}
	response := handleAgentAnswer(sess, ToolResponse{Action: "answer", Text: "candidate answer"})

	if !sess.AgentMode || !sess.AwaitingDecision || !response.AgentMode || !response.AgentDecision {
		t.Fatalf("agent mode did not pause for a decision: session=%+v response=%+v", sess, response)
	}
	if response.AgentContinue {
		t.Fatal("candidate answer should not auto-continue")
	}
	if systemPrompt != "agent prompt" || systemPromptName != "agent.txt" {
		t.Fatal("agent prompt was restored before the user ended the loop")
	}
}

func TestAgentChatRejectedWhileRunActive(t *testing.T) {
	sess := webStore.ensure("")
	sess.AgentMode = true
	body := bytes.NewBufferString(fmt.Sprintf(`{"sessionId":%q,"message":"next step"}`, sess.ID))
	response := httptest.NewRecorder()
	handleWebChat(response, httptest.NewRequest(http.MethodPost, "/api/chat", body))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func TestBackendAgentRunExecutesUntilAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responses := []string{
		`{"action":"run_command","command":"echo worker-owned"}`,
		`{"action":"answer","text":"finished without browser input"}`,
	}
	responseIndex := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAgentCriticApproval(w, r) {
			return
		}
		content := responses[responseIndex]
		responseIndex++
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "run a command and answer", "You are concise.", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, sess)

	run := sess.agentSnapshot()
	if responseIndex != 2 || run.Status != agentWaitingApproval || run.FinalAnswer != "finished without browser input" {
		t.Fatalf("backend run did not reach answer: calls=%d run=%+v", responseIndex, run)
	}
	foundOutput := false
	for _, event := range run.Events {
		if event.Kind == "observation" && strings.Contains(event.Output, "worker-owned") {
			foundOutput = true
		}
	}
	if !foundOutput {
		t.Fatalf("command observation missing from event log: %+v", run.Events)
	}
}

func TestBackendAgentRunContinuesAfterMissingJob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responses := []string{
		`{"action":"check_job","jobId":"missing-job"}`,
		`{"action":"answer","text":"recovered"}`,
	}
	responseIndex := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAgentCriticApproval(w, r) {
			return
		}
		content := responses[responseIndex]
		responseIndex++
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "recover", "You are concise.", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, sess)
	if run := sess.agentSnapshot(); responseIndex != 2 || run.Status != agentWaitingApproval || run.FinalAnswer != "recovered" {
		t.Fatalf("missing job stopped backend run: calls=%d run=%+v", responseIndex, run)
	}
}

func TestBackendAgentRunStopsAfterProtocolFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responses := []string{"not json", `{"action":"unknown"}`, "still not json"}
	responseIndex := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := responses[responseIndex]
		responseIndex++
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "fail safely", "You are concise.", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, sess)
	if run := sess.agentSnapshot(); responseIndex != 3 || run.Status != agentFailed || run.ConsecutiveErrors != 3 {
		t.Fatalf("protocol failures were not bounded: calls=%d run=%+v", responseIndex, run)
	}
}

func TestAgentCommandValidationRejectsNestedExecution(t *testing.T) {
	for _, command := range []string{"echo $(touch hidden)", "echo `touch hidden`", "echo ok; rm file"} {
		if err := validateAgentCommand(command); err == nil {
			t.Fatalf("command %q bypassed agent allowlist", command)
		}
	}
	if err := validateAgentCommand("echo hello | grep hello"); err != nil {
		t.Fatalf("valid allowlisted pipeline rejected: %v", err)
	}
	err := validateAgentCommand("/bin/ping -c 5 google.com")
	if err == nil || !strings.Contains(err.Error(), `bare allowlisted name "ping"`) {
		t.Fatalf("path-qualified command returned unhelpful error: %v", err)
	}
}

func TestFailedCommandDoesNotSatisfyExecutionRequirement(t *testing.T) {
	events := []AgentEvent{{
		Kind:    "observation",
		Action:  "run_command",
		Command: "ping invalid 5x",
		Output:  "usage: ping",
		Success: false,
	}}
	if hasCommandObservation(events, "ping") {
		t.Fatal("failed command counted as successful execution evidence")
	}
	events[0].Success = true
	if !hasCommandObservation(events, "ping") {
		t.Fatal("successful command observation was not recognized")
	}
}

func TestBackendAgentRunRetriesCommandAfterFailedExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responses := []string{
		`{"action":"run_command","command":"sh -c 'exit 1'"}`,
		`{"action":"update_findings","text":"the first command failed"}`,
		`{"action":"run_command","command":"echo recovered"}`,
		`{"action":"answer","text":"completed after retry"}`,
	}
	responseIndex := 0
	criticCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(request.Messages[0].Content, "You are an independent critic") {
			criticCalls++
			writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
			return
		}
		content := responses[responseIndex]
		responseIndex++
		writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "execute a check and report", defaultSystemPrompt, "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, sess)

	run := sess.agentSnapshot()
	if run.Status != agentWaitingApproval || run.FinalAnswer != "completed after retry" || responseIndex != 4 || criticCalls != 1 {
		t.Fatalf("failed command recovery did not complete: responses=%d critics=%d run=%+v", responseIndex, criticCalls, run)
	}
	if sess.PartialFindings != "" {
		t.Fatalf("non-command action bypassed recovery requirement: %q", sess.PartialFindings)
	}
	if command, failed := latestAgentCommandFailure(run.Events); failed || command != "echo recovered" {
		t.Fatalf("latest command failure state was not cleared: command=%q failed=%v", command, failed)
	}
}

func TestAgentMessagesIncludeAllowedCommandNames(t *testing.T) {
	sess := &webSession{ID: "session"}
	sess.AgentRun = newAgentRun(sess.ID, "ping once", defaultSystemPrompt, "default")
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(messages[0].Content, "EXECUTION ENVIRONMENT") || !strings.Contains(messages[0].Content, "Commands installed and allowed") {
		t.Fatalf("agent prompt omitted command allowlist: %q", messages[0].Content)
	}
}

func TestAgentRunDetectsEnvironmentCapabilities(t *testing.T) {
	run := newAgentRun("session", "inspect environment", defaultSystemPrompt, "default")
	if run.OperatingSystem == "" || run.WorkingDirectory == "" {
		t.Fatalf("execution environment was not captured: %+v", run)
	}
	if len(run.AvailableCommands)+len(run.UnavailableCommands) != len(allowedCommands) {
		t.Fatalf("command capability inventory is incomplete: available=%d unavailable=%d allowlist=%d", len(run.AvailableCommands), len(run.UnavailableCommands), len(allowedCommands))
	}
}

func TestAgentMessagesIncludeStructuredGoal(t *testing.T) {
	sess := &webSession{ID: "session"}
	sess.AgentRun = newAgentRun(sess.ID, "inspect service", defaultSystemPrompt, "default")
	sess.AgentRun.GoalSpec = normalizeAgentGoalSpec(AgentGoalSpec{
		Objective:          "Inspect service health",
		ExpectedOutput:     "A health report",
		Constraints:        "Read-only commands",
		CompletionCriteria: "Health and latency are reported",
	})
	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"OBJECTIVE:\nInspect service health", "EXPECTED OUTPUT:\nA health report", "CONSTRAINTS:\nRead-only commands", "COMPLETION CRITERIA:\nHealth and latency are reported"} {
		if !strings.Contains(messages[0].Content, expected) {
			t.Fatalf("structured goal omitted %q", expected)
		}
	}
}

func TestAgentContextSelectionUsesTokenBudget(t *testing.T) {
	events := make([]AgentEvent, 10)
	for index := range events {
		events[index] = AgentEvent{Sequence: index + 1, Kind: "observation", Output: strings.Repeat("x", 80)}
	}
	selected := selectAgentContextEvents(events, 0, 70)
	if len(selected) == 0 || len(selected) >= len(events) {
		t.Fatalf("token budget did not compact context: selected=%d", len(selected))
	}
	if selected[len(selected)-1].Sequence != 10 {
		t.Fatalf("newest event was not retained: %+v", selected)
	}
}

func TestAgentJobActionRequiresRunOwnedJob(t *testing.T) {
	run := newAgentRun("session", "inspect a job", defaultSystemPrompt, "default")
	sess := &webSession{ID: "session"}
	action := ToolResponse{Action: "check_job", JobID: "invented"}
	if err := validateAgentActionState(sess, run, action); err == nil || !strings.Contains(err.Error(), "was not created by this run") {
		t.Fatalf("invented job ID was not rejected: %v", err)
	}
}

func TestAgentPlanLifecycle(t *testing.T) {
	sess := &webSession{ID: "session"}
	sess.AgentRun = newAgentRun(sess.ID, "inspect two services", defaultSystemPrompt, "default")
	if !executeAgentAction(context.Background(), sess, ToolResponse{Action: "create_plan", Steps: []string{"Inspect first", "Inspect second"}}) {
		t.Fatal("create_plan stopped the run")
	}
	run := sess.agentSnapshot()
	if len(run.Plan) != 2 || run.Plan[0].Status != "pending" {
		t.Fatalf("plan was not created: %+v", run.Plan)
	}
	sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: "run_command", Success: true})
	run = sess.agentSnapshot()
	action := ToolResponse{Action: "complete_step", StepID: 1, EvidenceEventIDs: []int{2}}
	if err := validateAgentActionState(sess, run, action); err != nil {
		t.Fatal(err)
	}
	executeAgentAction(context.Background(), sess, action)
	if step := sess.agentSnapshot().Plan[0]; step.Status != "completed" || len(step.EvidenceEventIDs) != 1 {
		t.Fatalf("plan step was not completed with evidence: %+v", step)
	}
}

func TestBackendAgentRunRejectsAnswerBeforeRequiredCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responses := []string{
		`{"action":"answer","text":"No command was performed."}`,
		`{"action":"run_command","command":"echo measured"}`,
		`{"action":"answer","text":"The measured output was measured."}`,
	}
	var requestCount int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAgentCriticApproval(w, r) {
			return
		}
		response := responses[requestCount]
		requestCount++
		writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: response}})
	}))
	defer ollama.Close()
	previousURL := ollamaURL
	ollamaURL = ollama.URL
	defer func() { ollamaURL = previousURL }()

	webStore = newWebSessionStore()
	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "use echo to produce measured output", defaultSystemPrompt, "default")
	ctx, cancel := agentRunContext()
	defer cancel()
	runAgent(ctx, sess)

	run := sess.agentSnapshot()
	if run.Status != agentWaitingApproval {
		t.Fatalf("status = %q, want %q (error: %s)", run.Status, agentWaitingApproval, run.Error)
	}
	if requestCount != 3 {
		t.Fatalf("model requests = %d, want 3", requestCount)
	}
	if run.FinalAnswer != "The measured output was measured." {
		t.Fatalf("final answer = %q", run.FinalAnswer)
	}
	if !hasCommandObservation(run.Events, "echo") {
		t.Fatal("required command observation was not recorded")
	}
}

func TestBackendAgentRunRevisesCriticRejectedAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var agentCalls, criticCalls int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(request.Messages[0].Content, "You are an independent critic") {
			criticCalls++
			approved := criticCalls > 3
			feedback := ""
			if !approved {
				feedback = "The answer omits the requested conclusion."
			}
			content, _ := json.Marshal(AgentVerification{Approved: approved, Feedback: feedback})
			writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: string(content)}})
			return
		}
		agentCalls++
		answer := "incomplete"
		if agentCalls > 3 {
			answer = "complete revised answer"
		}
		writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: fmt.Sprintf(`{"action":"answer","text":%q}`, answer)}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })
	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "provide a conclusion", defaultSystemPrompt, "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, sess)

	run := sess.agentSnapshot()
	if run.Status != agentWaitingApproval || run.FinalAnswer != "complete revised answer" || agentCalls != 4 || criticCalls != 4 {
		t.Fatalf("critic revision flow failed: agentCalls=%d criticCalls=%d run=%+v", agentCalls, criticCalls, run)
	}
	foundRejectedCandidate := false
	for _, event := range run.Events {
		if event.Kind == "candidate" && event.Text == "incomplete" {
			foundRejectedCandidate = true
		}
	}
	if !foundRejectedCandidate {
		t.Fatalf("rejected candidate missing from run context: %+v", run.Events)
	}
}

func TestAgentRevisionContextPreservesCandidateAndRequestedChanges(t *testing.T) {
	run := newAgentRun("session", "write a simple Go web server", defaultSystemPrompt, "default")
	run.CandidateHistory = []string{"package main\n\nfunc main() { startServer() }"}
	run.UserRequirements = []string{"add a health check endpoint", "add basic authentication with admin/admin"}
	sess := &webSession{ID: "session", AgentRun: run}

	messages, err := agentMessages(sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"package main", "add a health check endpoint", "add basic authentication with admin/admin", "complete updated deliverable"} {
		if !strings.Contains(messages[0].Content, required) {
			t.Fatalf("agent revision context missing %q: %s", required, messages[0].Content)
		}
	}

	contextText := agentRevisionContext(run)
	if !strings.Contains(contextText, "Candidate 1") || !strings.Contains(contextText, "User-requested changes") {
		t.Fatalf("revision context omitted structured history: %s", contextText)
	}

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		criticInput := request.Messages[1].Content
		for _, required := range []string{"package main", "add a health check endpoint", "add basic authentication with admin/admin"} {
			if !strings.Contains(criticInput, required) {
				t.Errorf("critic revision context missing %q: %s", required, criticInput)
			}
		}
		writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
	}))
	defer ollama.Close()
	previousURL := ollamaURL
	ollamaURL = ollama.URL
	t.Cleanup(func() { ollamaURL = previousURL })
	if _, err := verifyAgentAnswer(context.Background(), sess, "complete revised code"); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCodeGoalRejectsProseOnlyCandidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	responseActions := []ToolResponse{
		{Action: "answer", Text: "Here is a simple Go web server example."},
		{Action: "answer", Text: "Complete source:\n\n```go\npackage main\n\nfunc main() {}"},
	}
	agentCalls := 0
	criticCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(request.Messages[0].Content, "You are an independent critic") {
			criticCalls++
			if strings.Contains(request.Messages[1].Content, `"kind":"verification"`) {
				t.Error("critic received a prior verification verdict")
			}
			writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"approved":true,"feedback":"","evidenceEventIds":[]}`}})
			return
		}
		encoded, err := json.Marshal(responseActions[agentCalls])
		if err != nil {
			t.Fatal(err)
		}
		content := string(encoded)
		agentCalls++
		writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })
	sess := webStore.ensure("")
	sess.AgentMode = true
	sess.AgentRun = newAgentRun(sess.ID, "write app in golang - simple web server", defaultSystemPrompt, "default")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runAgent(ctx, sess)

	run := sess.agentSnapshot()
	if run.Status != agentWaitingApproval || agentCalls != 2 || criticCalls != 1 || !strings.HasSuffix(run.FinalAnswer, "\n```") {
		t.Fatalf("code deliverable gate failed: agents=%d critics=%d run=%+v", agentCalls, criticCalls, run)
	}
}

func TestAgentLifecycleAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAgentCriticApproval(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{Message: ChatMessage{Role: "assistant", Content: `{"action":"answer","text":"candidate"}`}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	previousPrompt, previousPromptName := systemPrompt, systemPromptName
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	systemPrompt, systemPromptName = "custom developer prompt", "apps_developer.txt"
	t.Cleanup(func() {
		ollamaURL, webStore = previousURL, previousStore
		systemPrompt, systemPromptName = previousPrompt, previousPromptName
	})

	start := httptest.NewRecorder()
	handleAgentStart(start, httptest.NewRequest(http.MethodPost, "/api/agent/start", bytes.NewBufferString(`{"goal":"answer independently"}`)))
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", start.Code, start.Body.String())
	}
	var started struct {
		SessionID string    `json:"sessionId"`
		Run       *AgentRun `json:"run"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Run.BasePrompt != defaultSystemPrompt || started.Run.BasePromptName != "default" || systemPrompt != defaultSystemPrompt || systemPromptName != "default" {
		t.Fatalf("agent start did not reset prompt to default: run=%+v global=%q/%q", started.Run, systemPromptName, systemPrompt)
	}
	waitForAgentStatus(t, started.SessionID, agentWaitingApproval)

	status := httptest.NewRecorder()
	handleAgentStatus(status, httptest.NewRequest(http.MethodGet, "/api/agent/status?sessionId="+started.SessionID, nil))
	var statusPayload struct {
		Run *AgentRun `json:"run"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &statusPayload); err != nil {
		t.Fatal(err)
	}
	if statusPayload.Run == nil || statusPayload.Run.FinalAnswer != "candidate" || len(statusPayload.Run.Events) == 0 {
		t.Fatalf("status omitted run progress: %+v", statusPayload.Run)
	}

	decision := httptest.NewRecorder()
	body := bytes.NewBufferString(fmt.Sprintf(`{"sessionId":%q,"decision":"accept"}`, started.SessionID))
	handleAgentDecision(decision, httptest.NewRequest(http.MethodPost, "/api/agent/decision", body))
	if decision.Code != http.StatusOK {
		t.Fatalf("decision status = %d, body = %s", decision.Code, decision.Body.String())
	}
	if run := webStore.ensure(started.SessionID).agentSnapshot(); run.Status != agentCompleted {
		t.Fatalf("accepted run status = %s", run.Status)
	}
}

func TestAgentClarificationLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	agentCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAgentCriticApproval(w, r) {
			return
		}
		agentCalls++
		content := `{"action":"request_clarification","text":"Which output format should I use?"}`
		if agentCalls > 1 {
			content = `{"action":"answer","text":"Used JSON as requested."}`
		}
		writeJSON(w, http.StatusOK, ChatResponse{Message: ChatMessage{Role: "assistant", Content: content}})
	}))
	defer ollama.Close()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	start := httptest.NewRecorder()
	handleAgentStart(start, httptest.NewRequest(http.MethodPost, "/api/agent/start", bytes.NewBufferString(`{"goal":"format the result"}`)))
	var started struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	waitForAgentStatus(t, started.SessionID, agentWaitingInput)

	empty := httptest.NewRecorder()
	handleAgentDecision(empty, httptest.NewRequest(http.MethodPost, "/api/agent/decision", bytes.NewBufferString(fmt.Sprintf(`{"sessionId":%q,"decision":"continue"}`, started.SessionID))))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty clarification status = %d, body = %s", empty.Code, empty.Body.String())
	}

	decision := httptest.NewRecorder()
	handleAgentDecision(decision, httptest.NewRequest(http.MethodPost, "/api/agent/decision", bytes.NewBufferString(fmt.Sprintf(`{"sessionId":%q,"decision":"continue","feedback":"JSON"}`, started.SessionID))))
	if decision.Code != http.StatusOK {
		t.Fatalf("clarification status = %d, body = %s", decision.Code, decision.Body.String())
	}
	waitForAgentStatus(t, started.SessionID, agentWaitingApproval)
	run := webStore.ensure(started.SessionID).agentSnapshot()
	if run.FinalAnswer != "Used JSON as requested." || agentCalls != 2 {
		t.Fatalf("clarification did not resume run: agentCalls=%d run=%+v", agentCalls, run)
	}
}

func TestAgentStopCancelsModelRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
	}))
	defer func() {
		close(releaseRequest)
		ollama.Close()
	}()

	previousURL, previousStore := ollamaURL, webStore
	ollamaURL, webStore = ollama.URL, newWebSessionStore()
	t.Cleanup(func() { ollamaURL, webStore = previousURL, previousStore })

	start := httptest.NewRecorder()
	handleAgentStart(start, httptest.NewRequest(http.MethodPost, "/api/agent/start", bytes.NewBufferString(`{"goal":"wait"}`)))
	var started struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("backend worker did not start model request")
	}

	stop := httptest.NewRecorder()
	body := bytes.NewBufferString(fmt.Sprintf(`{"sessionId":%q}`, started.SessionID))
	handleAgentStop(stop, httptest.NewRequest(http.MethodPost, "/api/agent/stop", body))
	if stop.Code != http.StatusOK {
		t.Fatalf("stop status = %d, body = %s", stop.Code, stop.Body.String())
	}
	if run := webStore.ensure(started.SessionID).agentSnapshot(); run.Status != agentCancelled {
		t.Fatalf("stopped run status = %s", run.Status)
	}
}

func waitForAgentStatus(t *testing.T, sessionID string, want AgentRunStatus) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if run := webStore.ensure(sessionID).agentSnapshot(); run != nil && run.Status == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("agent run did not reach %s", want)
		case <-ticker.C:
		}
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

func TestWebStateStartsFreshAfterRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previousStore := webStore
	t.Cleanup(func() { webStore = previousStore })

	webStore = newWebSessionStore()
	original := webStore.ensure("")
	original.appendMessage("user", "shared question")
	original.appendMessage("assistant", "shared answer")
	original.AgentMode = true
	original.AgentGoal = "shared goal"
	original.AwaitingDecision = true
	agentDir, err := owrapAgentSessionDir(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	original.AttachedFilePath = filepath.Join(agentDir, "attachment.txt")
	if err := os.WriteFile(original.AttachedFilePath, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := webStore.persist(); err != nil {
		t.Fatal(err)
	}

	restartedStore := newWebSessionStore()
	if err := restartedStore.initializeFresh(); err != nil {
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
	if restored.SessionID == original.ID || len(restored.Messages) != 0 || restored.AgentMode || restored.AgentGoal != "" || restored.AgentDecision {
		t.Fatalf("restart did not clear state: %+v", restored)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("stale agent directory still exists: %v", err)
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

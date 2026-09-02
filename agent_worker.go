package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	agentMaxIterations  = 30
	agentRunTimeout     = 30 * time.Minute
	agentModelTimeout   = 2 * time.Minute
	agentCommandTimeout = 2 * time.Minute
	agentMaxObservation = 32 * 1024
	agentContextTokens  = 4096
	agentSummaryTokens  = 768
)

type AgentRunStatus string

const (
	agentRunning         AgentRunStatus = "running"
	agentWaitingApproval AgentRunStatus = "waiting_approval"
	agentWaitingInput    AgentRunStatus = "waiting_input"
	agentCompleted       AgentRunStatus = "completed"
	agentFailed          AgentRunStatus = "failed"
	agentCancelled       AgentRunStatus = "cancelled"
	agentLimitReached    AgentRunStatus = "limit_reached"
)

type AgentEvent struct {
	Sequence         int       `json:"sequence"`
	Kind             string    `json:"kind"`
	Action           string    `json:"action,omitempty"`
	Text             string    `json:"text,omitempty"`
	Command          string    `json:"command,omitempty"`
	Output           string    `json:"output,omitempty"`
	Success          bool      `json:"success,omitempty"`
	StepID           int       `json:"stepId,omitempty"`
	EvidenceEventIDs []int     `json:"evidenceEventIds,omitempty"`
	Thinking         string    `json:"thinking,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

type AgentRun struct {
	ID                  string          `json:"id"`
	SessionID           string          `json:"sessionId"`
	Goal                string          `json:"goal"`
	GoalSpec            AgentGoalSpec   `json:"goalSpec"`
	Status              AgentRunStatus  `json:"status"`
	Iteration           int             `json:"iteration"`
	MaxIterations       int             `json:"maxIterations"`
	StartedAt           time.Time       `json:"startedAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	CompletedAt         time.Time       `json:"completedAt,omitempty"`
	BasePrompt          string          `json:"basePrompt"`
	BasePromptName      string          `json:"basePromptName"`
	FinalAnswer         string          `json:"finalAnswer,omitempty"`
	Error               string          `json:"error,omitempty"`
	Events              []AgentEvent    `json:"events"`
	ConsecutiveErrors   int             `json:"consecutiveErrors"`
	AvailableCommands   []string        `json:"availableCommands"`
	UnavailableCommands []string        `json:"unavailableCommands,omitempty"`
	WorkingDirectory    string          `json:"workingDirectory"`
	OperatingSystem     string          `json:"operatingSystem"`
	ContextSummary      string          `json:"contextSummary,omitempty"`
	CompactedThrough    int             `json:"compactedThrough,omitempty"`
	Plan                []AgentPlanStep `json:"plan,omitempty"`
	CandidateHistory    []string        `json:"candidateHistory,omitempty"`
	UserRequirements    []string        `json:"userRequirements,omitempty"`
}

type AgentPlanStep struct {
	ID               int    `json:"id"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	EvidenceEventIDs []int  `json:"evidenceEventIds,omitempty"`
}

type AgentVerification struct {
	Approved         bool   `json:"approved"`
	Feedback         string `json:"feedback"`
	EvidenceEventIDs []int  `json:"evidenceEventIds,omitempty"`
}

type AgentGoalSpec struct {
	Objective          string `json:"objective"`
	ExpectedOutput     string `json:"expectedOutput"`
	Constraints        string `json:"constraints,omitempty"`
	CompletionCriteria string `json:"completionCriteria"`
}

func newAgentRun(sessionID, goal, basePrompt, basePromptName string) *AgentRun {
	now := time.Now().UTC()
	available, unavailable := detectAgentCommands()
	workingDirectory, _ := os.Getwd()
	return &AgentRun{
		ID:                  fmt.Sprintf("run_%d", time.Now().UnixNano()),
		SessionID:           sessionID,
		Goal:                goal,
		GoalSpec:            normalizeAgentGoalSpec(AgentGoalSpec{Objective: goal}),
		Status:              agentRunning,
		MaxIterations:       agentMaxIterations,
		StartedAt:           now,
		UpdatedAt:           now,
		BasePrompt:          basePrompt,
		BasePromptName:      basePromptName,
		Events:              make([]AgentEvent, 0),
		AvailableCommands:   available,
		UnavailableCommands: unavailable,
		WorkingDirectory:    workingDirectory,
		OperatingSystem:     runtime.GOOS,
	}
}

func detectAgentCommands() ([]string, []string) {
	available := make([]string, 0, len(allowedCommands))
	unavailable := make([]string, 0)
	for _, command := range allowedCommandsList() {
		if _, err := exec.LookPath(command); err == nil {
			available = append(available, command)
		} else {
			unavailable = append(unavailable, command)
		}
	}
	return available, unavailable
}

func normalizeAgentGoalSpec(spec AgentGoalSpec) AgentGoalSpec {
	spec.Objective = strings.TrimSpace(spec.Objective)
	spec.ExpectedOutput = strings.TrimSpace(spec.ExpectedOutput)
	spec.Constraints = strings.TrimSpace(spec.Constraints)
	spec.CompletionCriteria = strings.TrimSpace(spec.CompletionCriteria)
	if spec.ExpectedOutput == "" {
		spec.ExpectedOutput = "A concise result that directly addresses the objective."
	}
	if spec.CompletionCriteria == "" {
		spec.CompletionCriteria = "The objective is fully addressed and claims based on tools are supported by successful observations."
	}
	return spec
}

func agentGoalDescription(run *AgentRun) string {
	spec := normalizeAgentGoalSpec(run.GoalSpec)
	return fmt.Sprintf("OBJECTIVE:\n%s\n\nEXPECTED OUTPUT:\n%s\n\nCONSTRAINTS:\n%s\n\nCOMPLETION CRITERIA:\n%s", spec.Objective, spec.ExpectedOutput, emptyAgentValue(spec.Constraints), spec.CompletionCriteria)
}

func emptyAgentValue(value string) string {
	if value == "" {
		return "None specified."
	}
	return value
}

func (s *webSession) appendAgentEvent(event AgentEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentRun == nil {
		return
	}
	event.Sequence = len(s.AgentRun.Events) + 1
	event.CreatedAt = time.Now().UTC()
	s.AgentRun.Events = append(s.AgentRun.Events, event)
	s.AgentRun.UpdatedAt = event.CreatedAt
}

func (s *webSession) agentSnapshot() *AgentRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentRun == nil {
		return nil
	}
	snapshot := *s.AgentRun
	snapshot.Events = append([]AgentEvent(nil), s.AgentRun.Events...)
	snapshot.CandidateHistory = append([]string(nil), s.AgentRun.CandidateHistory...)
	snapshot.UserRequirements = append([]string(nil), s.AgentRun.UserRequirements...)
	return &snapshot
}

func (s *webSession) finishAgentRun(status AgentRunStatus, answer, errorText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentRun == nil {
		return
	}
	now := time.Now().UTC()
	s.AgentRun.Status = status
	s.AgentRun.FinalAnswer = answer
	s.AgentRun.Error = errorText
	s.AgentRun.UpdatedAt = now
	s.AgentRun.CompletedAt = now
	s.AgentMode = status == agentRunning || status == agentWaitingApproval || status == agentWaitingInput
}

func (s *webSession) cancelAgentRun() {
	s.mu.Lock()
	cancel := s.agentCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func agentRunContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), agentRunTimeout)
}

func runAgent(ctx context.Context, sess *webSession) {
	sess.appendAgentEvent(AgentEvent{Kind: "status", Text: "Agent run started"})
	defer func() {
		if err := webStore.persist(); err != nil {
			logAgentError(sess.ID, err)
		}
	}()

	for {
		run := sess.agentSnapshot()
		if run == nil || run.Status != agentRunning {
			return
		}
		if run.Iteration >= run.MaxIterations {
			finishAgent(sess, agentLimitReached, "", "maximum iteration limit reached")
			return
		}
		select {
		case <-ctx.Done():
			status := agentCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = agentLimitReached
			}
			finishAgent(sess, status, "", ctx.Err().Error())
			return
		default:
		}

		sess.mu.Lock()
		sess.AgentRun.Iteration++
		sess.AgentRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		compactAgentContext(ctx, sess)

		messages, err := agentMessages(sess)
		if err != nil {
			finishAgent(sess, agentFailed, "", err.Error())
			return
		}
		modelCtx, cancel := context.WithTimeout(ctx, agentModelTimeout)
		response, err := callOllamaMessageWithLogContext(modelCtx, "agent:"+run.ID, messages, true, sess.ThinkingEnabled)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !recordAgentFailure(sess, "model request failed: "+err.Error()) {
				return
			}
			continue
		}

		action, err := parseAgentAction(response.Content)
		if err != nil {
			sess.appendAgentEvent(AgentEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
			if !recordAgentFailure(sess, err.Error()) {
				return
			}
			continue
		}
		if err := validateAgentActionState(sess, run, action); err != nil {
			sess.appendAgentEvent(AgentEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
			if !recordAgentFailure(sess, err.Error()) {
				return
			}
			continue
		}
		if command, required := agentCommandRecoveryRequired(run.Events); required && action.Action != "run_command" {
			message := fmt.Sprintf("The latest command attempt failed: %q. Inspect its observation. Your next action must be run_command with a corrected or alternative command.", command)
			if !recordAgentRevision(sess, message, nil) {
				return
			}
			continue
		}
		if action.Action == "answer" {
			if goalRequiresCodeDeliverable(run) && !candidateHasCodeBlock(action.Text) {
				message := "The goal requires source code, but the candidate contains no non-empty fenced code block. Return the complete runnable code directly in the answer."
				sess.appendAgentEvent(AgentEvent{Kind: "candidate", Action: "answer", Text: action.Text, Thinking: response.Thinking})
				if !recordAgentRevision(sess, message, nil) {
					return
				}
				continue
			}
			if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
				message := fmt.Sprintf("goal requires executing %q before answering; return run_command with that command", command)
				sess.appendAgentEvent(AgentEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
				if !recordAgentFailure(sess, message) {
					return
				}
				continue
			}
			verification, err := verifyAgentAnswer(ctx, sess, action.Text)
			if err != nil {
				if !recordAgentFailure(sess, "answer verification failed: "+err.Error()) {
					return
				}
				continue
			}
			if !verification.Approved {
				feedback := strings.TrimSpace(verification.Feedback)
				if feedback == "" {
					feedback = "Candidate answer did not satisfy the goal and completion criteria."
				}
				sess.appendAgentEvent(AgentEvent{Kind: "candidate", Action: "answer", Text: action.Text, Thinking: response.Thinking})
				if !recordAgentRevision(sess, feedback, verification.EvidenceEventIDs) {
					return
				}
				continue
			}
			action.EvidenceEventIDs = append([]int(nil), verification.EvidenceEventIDs...)
		}
		resetAgentFailures(sess)
		sess.appendAgentEvent(AgentEvent{Kind: "action", Action: action.Action, Text: action.Text, Command: action.Command, EvidenceEventIDs: action.EvidenceEventIDs, Thinking: response.Thinking})
		if err := webStore.persist(); err != nil {
			finishAgent(sess, agentFailed, "", "could not persist action before execution: "+err.Error())
			return
		}

		if !executeAgentAction(ctx, sess, action) {
			return
		}
		if err := webStore.persist(); err != nil {
			logAgentError(sess.ID, err)
		}
	}
}

func agentMessages(sess *webSession) ([]ChatMessage, error) {
	run := sess.agentSnapshot()
	if run == nil {
		return nil, errors.New("agent run not found")
	}
	history := "Recent events are provided as conversation messages below."
	if run.ContextSummary != "" {
		history = "Earlier event summary:\n" + run.ContextSummary + "\n\n" + history
	}
	prompt, err := composeAgentPrompt(run.BasePrompt, agentGoalDescription(run), history)
	if err != nil {
		return nil, err
	}
	prompt += fmt.Sprintf("\n\nEXECUTION ENVIRONMENT:\nOS: %s\nWorking directory: %s\nCommands installed and allowed: %s\nAllowed but unavailable: %s\nCommands run with the same filesystem and network permissions as the OWRAP process.", run.OperatingSystem, run.WorkingDirectory, strings.Join(run.AvailableCommands, ", "), emptyAgentValue(strings.Join(run.UnavailableCommands, ", ")))
	if revisionContext := agentRevisionContext(run); revisionContext != "" {
		prompt += "\n\nREVISION CONTEXT:\n" + revisionContext + "\nReturn a self-contained candidate containing the complete updated deliverable. Never claim that a previous response contains a requested change; include the changed result in this answer."
	}
	if len(run.Plan) > 0 {
		planJSON, _ := json.Marshal(run.Plan)
		prompt += "\n\nCURRENT PLAN:\n" + string(planJSON)
	}
	messages := []ChatMessage{{Role: "system", Content: prompt}}
	if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
		messages[0].Content += fmt.Sprintf("\n\nEXECUTION REQUIREMENT: The goal explicitly requires %q. You must return run_command using %q and inspect its real output before returning answer. A description, refusal, or hypothetical result is not completion.", command, command)
	}
	if command, required := agentCommandRecoveryRequired(run.Events); required {
		messages[0].Content += fmt.Sprintf("\n\nCOMMAND RECOVERY REQUIREMENT: The latest command attempt failed: %q. Your next action must be run_command with corrected syntax or an alternative command. Do not answer, update findings, change the plan, or request clarification before making that attempt.", command)
	}
	if run.ContextSummary != "" {
		messages = append(messages, ChatMessage{Role: "user", Content: "Earlier run summary:\n" + run.ContextSummary})
	}
	for _, event := range selectAgentContextEvents(run.Events, run.CompactedThrough, agentContextTokens) {
		switch event.Kind {
		case "action", "invalid_action":
			if event.Kind == "invalid_action" {
				messages = append(messages, ChatMessage{Role: "assistant", Content: event.Text})
			} else {
				content, _ := json.Marshal(ToolResponse{Action: event.Action, Command: event.Command, Text: event.Text, StepID: event.StepID, EvidenceEventIDs: event.EvidenceEventIDs})
				messages = append(messages, ChatMessage{Role: "assistant", Content: string(content)})
			}
		case "observation", "error":
			messages = append(messages, ChatMessage{Role: "user", Content: "Tool observation:\n" + event.Text + "\n" + event.Output})
		case "feedback":
			messages = append(messages, ChatMessage{Role: "user", Content: "User feedback:\n" + event.Text})
		case "verification":
			messages = append(messages, ChatMessage{Role: "user", Content: "Independent critic feedback:\n" + event.Text})
		case "candidate":
			messages = append(messages, ChatMessage{Role: "assistant", Content: "Rejected candidate answer:\n" + event.Text})
		case "plan":
			messages = append(messages, ChatMessage{Role: "user", Content: "Plan update:\n" + event.Text})
		}
	}
	if len(messages) == 1 {
		instruction := "Begin working on the goal. Answer directly only if no tool is needed."
		if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
			instruction = fmt.Sprintf("Begin by executing the required %q command. Do not answer until you have inspected its output.", command)
		}
		messages = append(messages, ChatMessage{Role: "user", Content: instruction})
	} else {
		messages = append(messages, ChatMessage{Role: "user", Content: "Choose the next action. If the goal is complete, return answer."})
	}
	return messages, nil
}

func requiredGoalCommand(run *AgentRun) string {
	for _, field := range agentGoalFields(run.Goal) {
		if allowedCommands[field] {
			return field
		}
	}
	return ""
}

func agentGoalFields(goal string) []string {
	return strings.FieldsFunc(strings.ToLower(goal), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-'
	})
}

func hasCommandObservation(events []AgentEvent, requiredCommand string) bool {
	for _, event := range events {
		if event.Kind != "observation" || event.Action != "run_command" || !event.Success {
			continue
		}
		fields := strings.Fields(event.Command)
		if len(fields) > 0 && fields[0] == requiredCommand {
			return true
		}
	}
	return false
}

func goalRequiresCodeDeliverable(run *AgentRun) bool {
	text := strings.ToLower(strings.Join([]string{run.GoalSpec.Objective, run.GoalSpec.ExpectedOutput, run.GoalSpec.CompletionCriteria}, " "))
	creationVerb := false
	for _, verb := range []string{"write", "develop", "build", "implement", "create", "generate"} {
		if strings.Contains(text, verb) {
			creationVerb = true
			break
		}
	}
	if !creationVerb {
		return false
	}
	for _, artifact := range []string{"code", "app", "application", "program", "server"} {
		if strings.Contains(text, artifact) {
			return true
		}
	}
	return false
}

func candidateHasCodeBlock(candidate string) bool {
	start := strings.Index(candidate, "```")
	if start < 0 {
		return false
	}
	contentStart := strings.Index(candidate[start+3:], "\n")
	if contentStart < 0 {
		return false
	}
	contentStart += start + 4
	end := strings.Index(candidate[contentStart:], "```")
	return end > 0 && strings.TrimSpace(candidate[contentStart:contentStart+end]) != ""
}

func latestAgentCommandFailure(events []AgentEvent) (string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind == "observation" && event.Action == "run_command" {
			return event.Command, !event.Success
		}
	}
	return "", false
}

func agentCommandRecoveryRequired(events []AgentEvent) (string, bool) {
	command, failed := latestAgentCommandFailure(events)
	return command, failed && consecutiveAgentCommandFailures(events) < 2
}

func consecutiveAgentCommandFailures(events []AgentEvent) int {
	failures := 0
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind != "observation" || event.Action != "run_command" {
			continue
		}
		if event.Success {
			break
		}
		failures++
	}
	return failures
}

func validateAgentActionState(sess *webSession, run *AgentRun, action ToolResponse) error {
	if action.Action == "check_job" || action.Action == "get_job" || action.Action == "cancel_job" {
		job, ok := jobStore.get(action.JobID)
		if !ok || job.SessionID != sess.ID || job.RunID != run.ID {
			return fmt.Errorf("job %q was not created by this run; start a background job first and use its returned jobId", action.JobID)
		}
	}
	if action.Action == "complete_step" {
		if action.StepID > len(run.Plan) {
			return fmt.Errorf("plan step %d does not exist", action.StepID)
		}
		for _, eventID := range action.EvidenceEventIDs {
			if eventID <= 0 || eventID > len(run.Events) {
				return fmt.Errorf("evidence event %d does not exist", eventID)
			}
		}
	}
	return nil
}

func agentEventHistory(events []AgentEvent) string {
	if len(events) == 0 {
		return "No previous attempts yet."
	}
	var history strings.Builder
	for _, event := range events {
		fmt.Fprintf(&history, "%d. %s %s %s\n", event.Sequence, event.Kind, event.Action, truncateAgentText(event.Text+event.Output))
	}
	return history.String()
}

func estimateAgentTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func agentEventText(event AgentEvent) string {
	data, _ := json.Marshal(event)
	return string(data)
}

func selectAgentContextEvents(events []AgentEvent, compactedThrough, tokenBudget int) []AgentEvent {
	selectedStart := len(events)
	used := 0
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Sequence <= compactedThrough {
			break
		}
		tokens := estimateAgentTokens(agentEventText(events[index]))
		if selectedStart < len(events) && used+tokens > tokenBudget {
			break
		}
		selectedStart = index
		used += tokens
	}
	return append([]AgentEvent(nil), events[selectedStart:]...)
}

func compactAgentContext(ctx context.Context, sess *webSession) {
	run := sess.agentSnapshot()
	if run == nil {
		return
	}
	pending := make([]AgentEvent, 0)
	totalTokens := 0
	for _, event := range run.Events {
		if event.Sequence <= run.CompactedThrough {
			continue
		}
		pending = append(pending, event)
		totalTokens += estimateAgentTokens(agentEventText(event))
	}
	if len(pending) < 2 || totalTokens <= agentContextTokens {
		return
	}
	keep := selectAgentContextEvents(pending, 0, agentContextTokens/2)
	compactCount := len(pending) - len(keep)
	if compactCount <= 0 {
		return
	}
	toCompact := pending[:compactCount]
	summaryInput := truncateAgentText(agentEventHistory(toCompact))
	summaryMessages := []ChatMessage{
		{Role: "system", Content: fmt.Sprintf("Summarize agent-agent history in at most %d tokens. Preserve verified facts, failed approaches, unresolved questions, user constraints, and job IDs. Do not invent information.", agentSummaryTokens)},
		{Role: "user", Content: "Existing summary:\n" + emptyAgentValue(run.ContextSummary) + "\n\nEvents to compact:\n" + summaryInput},
	}
	summaryCtx, cancel := context.WithTimeout(ctx, agentModelTimeout)
	response, err := callOllamaMessageWithLogContext(summaryCtx, "agent-summary:"+run.ID, summaryMessages, false, false)
	cancel()
	summary := strings.TrimSpace(response.Content)
	if err != nil || summary == "" {
		summary = truncateAgentText(run.ContextSummary + "\n" + summaryInput)
	}
	sess.mu.Lock()
	if sess.AgentRun != nil && sess.AgentRun.ID == run.ID {
		sess.AgentRun.ContextSummary = summary
		sess.AgentRun.CompactedThrough = toCompact[len(toCompact)-1].Sequence
		sess.AgentRun.UpdatedAt = time.Now().UTC()
	}
	sess.mu.Unlock()
}

func verifyAgentAnswer(ctx context.Context, sess *webSession, candidate string) (AgentVerification, error) {
	run := sess.agentSnapshot()
	if run == nil {
		return AgentVerification{}, errors.New("agent run not found")
	}
	evidence := agentCriticEvidence(selectAgentContextEvents(run.Events, run.CompactedThrough, agentContextTokens))
	evidenceJSON, _ := json.Marshal(evidence)
	planJSON, _ := json.Marshal(run.Plan)
	revisionContext := agentRevisionContext(run)
	messages := []ChatMessage{
		{Role: "system", Content: "You are an independent critic. Verify the exact candidate agent-agent answer against the objective, expected output, constraints, completion criteria, plan, user feedback, and recorded events. Approve only if the candidate itself contains the complete requested deliverable. Never assume promised, described, or omitted content exists; a preamble without its claimed output is incomplete. Return exactly one JSON object: {\"approved\":true|false,\"feedback\":\"specific reason or empty\",\"evidenceEventIds\":[1,2]}. Cite only event IDs that directly support the answer. Knowledge-only answers may use an empty evidence list."},
		{Role: "user", Content: agentGoalDescription(run) + "\n\nREVISION CONTEXT:\n" + revisionContext + "\n\nPLAN:\n" + string(planJSON) + "\n\nCANDIDATE ANSWER:\n" + candidate + "\n\nRECORDED EVENTS:\n" + string(evidenceJSON)},
	}
	verifyCtx, cancel := context.WithTimeout(ctx, agentModelTimeout)
	response, err := callOllamaMessageWithLogContext(verifyCtx, "agent-critic:"+run.ID, messages, true, false)
	cancel()
	if err != nil {
		return AgentVerification{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(response.Content)))
	decoder.DisallowUnknownFields()
	var verification AgentVerification
	if err := decoder.Decode(&verification); err != nil {
		return AgentVerification{}, fmt.Errorf("invalid critic JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return AgentVerification{}, errors.New("critic response must contain exactly one JSON object")
	}
	for _, step := range run.Plan {
		if verification.Approved && step.Status != "completed" {
			verification.Approved = false
			verification.Feedback = fmt.Sprintf("plan step %d is not completed: %s", step.ID, step.Description)
		}
	}
	for _, eventID := range verification.EvidenceEventIDs {
		if eventID <= 0 || eventID > len(run.Events) {
			return AgentVerification{}, fmt.Errorf("critic cited nonexistent event %d", eventID)
		}
	}
	if command := requiredGoalCommand(run); verification.Approved && command != "" && !evidenceSupportsCommand(run.Events, verification.EvidenceEventIDs, command) {
		verification.Approved = false
		verification.Feedback = fmt.Sprintf("candidate does not cite a successful %q observation", command)
	}
	return verification, nil
}

func agentCriticEvidence(events []AgentEvent) []AgentEvent {
	evidence := make([]AgentEvent, 0, len(events))
	for _, event := range events {
		switch event.Kind {
		case "observation", "feedback", "plan":
			evidence = append(evidence, event)
		}
	}
	return evidence
}

func agentRevisionContext(run *AgentRun) string {
	if len(run.CandidateHistory) == 0 && len(run.UserRequirements) == 0 {
		return ""
	}
	var contextText strings.Builder
	if len(run.CandidateHistory) > 0 {
		contextText.WriteString("Previously shown candidate answers, oldest to newest:\n")
		start := len(run.CandidateHistory) - 4
		if start < 0 {
			start = 0
		}
		for index, candidate := range run.CandidateHistory[start:] {
			fmt.Fprintf(&contextText, "\nCandidate %d:\n%s\n", start+index+1, truncateAgentText(candidate))
		}
	}
	if len(run.UserRequirements) > 0 {
		contextText.WriteString("\nUser-requested changes, oldest to newest:\n")
		for index, requirement := range run.UserRequirements {
			fmt.Fprintf(&contextText, "%d. %s\n", index+1, requirement)
		}
	}
	return strings.TrimSpace(contextText.String())
}

func evidenceSupportsCommand(events []AgentEvent, evidenceIDs []int, command string) bool {
	for _, eventID := range evidenceIDs {
		event := events[eventID-1]
		fields := strings.Fields(event.Command)
		if event.Kind == "observation" && event.Action == "run_command" && event.Success && len(fields) > 0 && fields[0] == command {
			return true
		}
	}
	return false
}

func parseAgentAction(raw string) (ToolResponse, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var action ToolResponse
	if err := decoder.Decode(&action); err != nil {
		return ToolResponse{}, fmt.Errorf("invalid agent action JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ToolResponse{}, errors.New("agent response must contain exactly one JSON object")
	}
	switch action.Action {
	case "answer", "update_findings", "request_clarification":
		if strings.TrimSpace(action.Text) == "" {
			return ToolResponse{}, fmt.Errorf("action %q requires text", action.Action)
		}
	case "run_command", "run_command_bg":
		if err := validateAgentCommand(action.Command); err != nil {
			return ToolResponse{}, err
		}
	case "check_job", "get_job", "cancel_job":
		if strings.TrimSpace(action.JobID) == "" {
			return ToolResponse{}, fmt.Errorf("action %q requires jobId", action.Action)
		}
	case "list_jobs":
	case "create_plan":
		if len(action.Steps) == 0 {
			return ToolResponse{}, errors.New("action create_plan requires at least one step")
		}
		for _, step := range action.Steps {
			if strings.TrimSpace(step) == "" {
				return ToolResponse{}, errors.New("plan steps must not be empty")
			}
		}
	case "complete_step":
		if action.StepID <= 0 {
			return ToolResponse{}, errors.New("action complete_step requires a positive stepId")
		}
	default:
		return ToolResponse{}, fmt.Errorf("unsupported agent action %q", action.Action)
	}
	return action, nil
}

func validateAgentCommand(command string) error {
	command = sanitizeCommand(command)
	if command == "" || strings.ContainsAny(command, "\n\r") {
		return errors.New("command must be one non-empty line")
	}
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return errors.New("command substitution is not allowed")
	}
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool { return r == '|' || r == ';' || r == '&' }) {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(fields[0], "/") {
			commandName := filepath.Base(fields[0])
			if allowedCommands[commandName] {
				return fmt.Errorf("command %q must use the bare allowlisted name %q without a path", fields[0], commandName)
			}
		}
		if !allowedCommands[fields[0]] {
			return fmt.Errorf("command %q is not allowed", fields[0])
		}
	}
	return nil
}

func executeAgentAction(ctx context.Context, sess *webSession, action ToolResponse) bool {
	switch action.Action {
	case "answer":
		sess.appendAgentEvent(AgentEvent{Kind: "answer", Action: action.Action, Text: action.Text, EvidenceEventIDs: action.EvidenceEventIDs})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: action.Text})
		sess.mu.Lock()
		sess.AgentRun.CandidateHistory = append(sess.AgentRun.CandidateHistory, action.Text)
		sess.AwaitingDecision = true
		sess.AgentRun.Status = agentWaitingApproval
		sess.AgentRun.FinalAnswer = action.Text
		sess.AgentRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "request_clarification":
		sess.appendAgentEvent(AgentEvent{Kind: "clarification", Action: action.Action, Text: action.Text})
		sess.mu.Lock()
		sess.AwaitingDecision = true
		sess.AgentRun.Status = agentWaitingInput
		sess.AgentRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "update_findings":
		sess.mu.Lock()
		sess.PartialFindings = action.Text
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: "Findings saved: " + action.Text})
		return true
	case "create_plan":
		plan := make([]AgentPlanStep, 0, len(action.Steps))
		for index, description := range action.Steps {
			plan = append(plan, AgentPlanStep{ID: index + 1, Description: strings.TrimSpace(description), Status: "pending"})
		}
		sess.mu.Lock()
		sess.AgentRun.Plan = plan
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "plan", Action: action.Action, Text: strings.Join(action.Steps, "\n")})
		return true
	case "complete_step":
		sess.mu.Lock()
		step := &sess.AgentRun.Plan[action.StepID-1]
		step.Status = "completed"
		step.EvidenceEventIDs = append([]int(nil), action.EvidenceEventIDs...)
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "plan", Action: action.Action, StepID: action.StepID, EvidenceEventIDs: action.EvidenceEventIDs, Text: fmt.Sprintf("Plan step %d completed", action.StepID)})
		return true
	case "run_command":
		commandCtx, cancel := context.WithTimeout(ctx, agentCommandTimeout)
		output, success := runAgentCommand(commandCtx, action.Command)
		cancel()
		sess.mu.Lock()
		sess.CommandCount++
		sess.Stats.recordCommand(action.Command)
		sess.mu.Unlock()
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Command: action.Command, Output: truncateAgentText(output), Success: success})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: fmt.Sprintf("[Running]: %s\n[Command output]:\n%s", action.Command, output)})
		return true
	case "run_command_bg":
		job, err := executeBackgroundCommand(sess.ID, action.Command)
		if err != nil {
			sess.appendAgentEvent(AgentEvent{Kind: "error", Action: action.Action, Text: err.Error()})
			return true
		}
		if run := sess.agentSnapshot(); run != nil {
			job.RunID = run.ID
			jobStore.update(job)
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("job %s started", job.ID)})
		return true
	case "check_job", "get_job":
		job, ok := jobStore.get(action.JobID)
		if !ok || job.SessionID != sess.ID {
			sess.appendAgentEvent(AgentEvent{Kind: "error", Action: action.Action, Text: "job not found"})
			return true
		}
		output := job.Output
		if action.Action == "check_job" && len(output) > 1000 {
			output = output[:1000] + "\n... (truncated)"
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("job %s status: %s\n", job.ID, job.Status), Output: truncateAgentText(output)})
		return true
	case "cancel_job":
		job, ok := jobStore.get(action.JobID)
		if ok && job.SessionID == sess.ID && job.Cmd != nil && job.Cmd.Process != nil {
			_ = job.Cmd.Process.Kill()
			job.Status = "cancelled"
			job.EndTime = time.Now()
			jobStore.update(job)
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: "job cancellation processed"})
		return true
	case "list_jobs":
		jobs := jobStore.listBySession(sess.ID)
		var text strings.Builder
		for _, job := range jobs {
			fmt.Fprintf(&text, "%s: %s [%s]\n", job.ID, job.Command, job.Status)
		}
		if text.Len() == 0 {
			text.WriteString("No jobs")
		}
		sess.appendAgentEvent(AgentEvent{Kind: "observation", Action: action.Action, Text: text.String()})
		return true
	}
	return true
}

func runAgentCommand(ctx context.Context, command string) (string, bool) {
	command = sanitizeCommand(command)
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + stderr.String()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		output += "\nCommand timed out."
	} else if err != nil {
		output += "\nError: " + err.Error()
	}
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	}
	return output, err == nil && ctx.Err() == nil
}

func recordAgentFailure(sess *webSession, message string) bool {
	sess.mu.Lock()
	if sess.AgentRun == nil {
		sess.mu.Unlock()
		return false
	}
	sess.AgentRun.ConsecutiveErrors++
	failures := sess.AgentRun.ConsecutiveErrors
	sess.mu.Unlock()
	sess.appendAgentEvent(AgentEvent{Kind: "error", Text: message})
	if failures >= 3 {
		finishAgent(sess, agentFailed, "", message)
		return false
	}
	return true
}

func recordAgentRevision(sess *webSession, message string, evidenceEventIDs []int) bool {
	sess.mu.Lock()
	if sess.AgentRun == nil {
		sess.mu.Unlock()
		return false
	}
	sess.AgentRun.ConsecutiveErrors = 0
	sess.mu.Unlock()
	sess.appendAgentEvent(AgentEvent{Kind: "verification", Action: "answer", Text: message, EvidenceEventIDs: evidenceEventIDs})
	return true
}

func resetAgentFailures(sess *webSession) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.AgentRun != nil {
		sess.AgentRun.ConsecutiveErrors = 0
	}
}

func finishAgent(sess *webSession, status AgentRunStatus, answer, errorText string) {
	sess.finishAgentRun(status, answer, errorText)
	text := errorText
	if answer != "" {
		text = answer
	}
	sess.appendAgentEvent(AgentEvent{Kind: "status", Text: string(status) + ": " + text})
	cleanupAgentRunResources(sess)
}

func cleanupAgentRunResources(sess *webSession) {
	run := sess.agentSnapshot()
	if run != nil {
		jobStore.cancelRun(run.ID)
	}
	sess.mu.Lock()
	attachmentPath := sess.AttachedFilePath
	sess.AttachedFile = nil
	sess.AttachedFilePath = ""
	sess.AgentStart = 0
	sess.AwaitingDecision = false
	sess.mu.Unlock()
	if attachmentPath != "" {
		if dir, err := owrapAgentSessionDir(sess.ID); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
}

func truncateAgentText(text string) string {
	if len(text) <= agentMaxObservation {
		return text
	}
	return text[:agentMaxObservation] + "\n... (truncated)"
}

func (s *webSession) appendAgentMessage(message ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, message)
	s.Stats.recordMessage(message)
	s.Stats.updateContext(systemPrompt, s.Messages)
}

func logAgentError(sessionID string, err error) {
	fmt.Printf("[AGENT] session=%s persistence error: %v\n", sessionID, err)
}

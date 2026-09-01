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
	autonomousMaxIterations  = 30
	autonomousRunTimeout     = 30 * time.Minute
	autonomousModelTimeout   = 2 * time.Minute
	autonomousCommandTimeout = 2 * time.Minute
	autonomousMaxObservation = 32 * 1024
	autonomousContextTokens  = 4096
	autonomousSummaryTokens  = 768
)

type AutonomousRunStatus string

const (
	autonomousRunning         AutonomousRunStatus = "running"
	autonomousWaitingApproval AutonomousRunStatus = "waiting_approval"
	autonomousWaitingInput    AutonomousRunStatus = "waiting_input"
	autonomousCompleted       AutonomousRunStatus = "completed"
	autonomousFailed          AutonomousRunStatus = "failed"
	autonomousCancelled       AutonomousRunStatus = "cancelled"
	autonomousLimitReached    AutonomousRunStatus = "limit_reached"
)

type AutonomousEvent struct {
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

type AutonomousRun struct {
	ID                  string               `json:"id"`
	SessionID           string               `json:"sessionId"`
	Goal                string               `json:"goal"`
	GoalSpec            AutonomousGoalSpec   `json:"goalSpec"`
	Status              AutonomousRunStatus  `json:"status"`
	Iteration           int                  `json:"iteration"`
	MaxIterations       int                  `json:"maxIterations"`
	StartedAt           time.Time            `json:"startedAt"`
	UpdatedAt           time.Time            `json:"updatedAt"`
	CompletedAt         time.Time            `json:"completedAt,omitempty"`
	BasePrompt          string               `json:"basePrompt"`
	BasePromptName      string               `json:"basePromptName"`
	FinalAnswer         string               `json:"finalAnswer,omitempty"`
	Error               string               `json:"error,omitempty"`
	Events              []AutonomousEvent    `json:"events"`
	ConsecutiveErrors   int                  `json:"consecutiveErrors"`
	AvailableCommands   []string             `json:"availableCommands"`
	UnavailableCommands []string             `json:"unavailableCommands,omitempty"`
	WorkingDirectory    string               `json:"workingDirectory"`
	OperatingSystem     string               `json:"operatingSystem"`
	ContextSummary      string               `json:"contextSummary,omitempty"`
	CompactedThrough    int                  `json:"compactedThrough,omitempty"`
	Plan                []AutonomousPlanStep `json:"plan,omitempty"`
}

type AutonomousPlanStep struct {
	ID               int    `json:"id"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	EvidenceEventIDs []int  `json:"evidenceEventIds,omitempty"`
}

type AutonomousVerification struct {
	Approved         bool   `json:"approved"`
	Feedback         string `json:"feedback"`
	EvidenceEventIDs []int  `json:"evidenceEventIds,omitempty"`
}

type AutonomousGoalSpec struct {
	Objective          string `json:"objective"`
	ExpectedOutput     string `json:"expectedOutput"`
	Constraints        string `json:"constraints,omitempty"`
	CompletionCriteria string `json:"completionCriteria"`
}

func newAutonomousRun(sessionID, goal, basePrompt, basePromptName string) *AutonomousRun {
	now := time.Now().UTC()
	available, unavailable := detectAutonomousCommands()
	workingDirectory, _ := os.Getwd()
	return &AutonomousRun{
		ID:                  fmt.Sprintf("run_%d", time.Now().UnixNano()),
		SessionID:           sessionID,
		Goal:                goal,
		GoalSpec:            normalizeAutonomousGoalSpec(AutonomousGoalSpec{Objective: goal}),
		Status:              autonomousRunning,
		MaxIterations:       autonomousMaxIterations,
		StartedAt:           now,
		UpdatedAt:           now,
		BasePrompt:          basePrompt,
		BasePromptName:      basePromptName,
		Events:              make([]AutonomousEvent, 0),
		AvailableCommands:   available,
		UnavailableCommands: unavailable,
		WorkingDirectory:    workingDirectory,
		OperatingSystem:     runtime.GOOS,
	}
}

func detectAutonomousCommands() ([]string, []string) {
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

func normalizeAutonomousGoalSpec(spec AutonomousGoalSpec) AutonomousGoalSpec {
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

func autonomousGoalDescription(run *AutonomousRun) string {
	spec := normalizeAutonomousGoalSpec(run.GoalSpec)
	return fmt.Sprintf("OBJECTIVE:\n%s\n\nEXPECTED OUTPUT:\n%s\n\nCONSTRAINTS:\n%s\n\nCOMPLETION CRITERIA:\n%s", spec.Objective, spec.ExpectedOutput, emptyAutonomousValue(spec.Constraints), spec.CompletionCriteria)
}

func emptyAutonomousValue(value string) string {
	if value == "" {
		return "None specified."
	}
	return value
}

func (s *webSession) appendAutonomousEvent(event AutonomousEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AutonomousRun == nil {
		return
	}
	event.Sequence = len(s.AutonomousRun.Events) + 1
	event.CreatedAt = time.Now().UTC()
	s.AutonomousRun.Events = append(s.AutonomousRun.Events, event)
	s.AutonomousRun.UpdatedAt = event.CreatedAt
}

func (s *webSession) autonomousSnapshot() *AutonomousRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AutonomousRun == nil {
		return nil
	}
	snapshot := *s.AutonomousRun
	snapshot.Events = append([]AutonomousEvent(nil), s.AutonomousRun.Events...)
	return &snapshot
}

func (s *webSession) finishAutonomousRun(status AutonomousRunStatus, answer, errorText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AutonomousRun == nil {
		return
	}
	now := time.Now().UTC()
	s.AutonomousRun.Status = status
	s.AutonomousRun.FinalAnswer = answer
	s.AutonomousRun.Error = errorText
	s.AutonomousRun.UpdatedAt = now
	s.AutonomousRun.CompletedAt = now
	s.AutonomousMode = status == autonomousRunning || status == autonomousWaitingApproval || status == autonomousWaitingInput
}

func (s *webSession) cancelAutonomousRun() {
	s.mu.Lock()
	cancel := s.autonomousCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func autonomousRunContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), autonomousRunTimeout)
}

func runAutonomousAgent(ctx context.Context, sess *webSession) {
	sess.appendAutonomousEvent(AutonomousEvent{Kind: "status", Text: "Autonomous run started"})
	defer func() {
		if err := webStore.persist(); err != nil {
			logAutonomousError(sess.ID, err)
		}
	}()

	for {
		run := sess.autonomousSnapshot()
		if run == nil || run.Status != autonomousRunning {
			return
		}
		if run.Iteration >= run.MaxIterations {
			finishAutonomous(sess, autonomousLimitReached, "", "maximum iteration limit reached")
			return
		}
		select {
		case <-ctx.Done():
			status := autonomousCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = autonomousLimitReached
			}
			finishAutonomous(sess, status, "", ctx.Err().Error())
			return
		default:
		}

		sess.mu.Lock()
		sess.AutonomousRun.Iteration++
		sess.AutonomousRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		compactAutonomousContext(ctx, sess)

		messages, err := autonomousMessages(sess)
		if err != nil {
			finishAutonomous(sess, autonomousFailed, "", err.Error())
			return
		}
		modelCtx, cancel := context.WithTimeout(ctx, autonomousModelTimeout)
		response, err := callOllamaMessageWithLogContext(modelCtx, "autonomous:"+run.ID, messages, true, sess.ThinkingEnabled)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !recordAutonomousFailure(sess, "model request failed: "+err.Error()) {
				return
			}
			continue
		}

		action, err := parseAutonomousAction(response.Content)
		if err != nil {
			sess.appendAutonomousEvent(AutonomousEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
			if !recordAutonomousFailure(sess, err.Error()) {
				return
			}
			continue
		}
		if err := validateAutonomousActionState(sess, run, action); err != nil {
			sess.appendAutonomousEvent(AutonomousEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
			if !recordAutonomousFailure(sess, err.Error()) {
				return
			}
			continue
		}
		if command, required := autonomousCommandRecoveryRequired(run.Events); required && action.Action != "run_command" {
			message := fmt.Sprintf("The latest command attempt failed: %q. Inspect its observation. Your next action must be run_command with a corrected or alternative command.", command)
			if !recordAutonomousRevision(sess, message, nil) {
				return
			}
			continue
		}
		if action.Action == "answer" {
			if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
				message := fmt.Sprintf("goal requires executing %q before answering; return run_command with that command", command)
				sess.appendAutonomousEvent(AutonomousEvent{Kind: "invalid_action", Text: response.Content, Thinking: response.Thinking})
				if !recordAutonomousFailure(sess, message) {
					return
				}
				continue
			}
			verification, err := verifyAutonomousAnswer(ctx, sess, action.Text)
			if err != nil {
				if !recordAutonomousFailure(sess, "answer verification failed: "+err.Error()) {
					return
				}
				continue
			}
			if !verification.Approved {
				feedback := strings.TrimSpace(verification.Feedback)
				if feedback == "" {
					feedback = "Candidate answer did not satisfy the goal and completion criteria."
				}
				if !recordAutonomousRevision(sess, feedback, verification.EvidenceEventIDs) {
					return
				}
				continue
			}
			action.EvidenceEventIDs = append([]int(nil), verification.EvidenceEventIDs...)
		}
		resetAutonomousFailures(sess)
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "action", Action: action.Action, Text: action.Text, Command: action.Command, EvidenceEventIDs: action.EvidenceEventIDs, Thinking: response.Thinking})
		if err := webStore.persist(); err != nil {
			finishAutonomous(sess, autonomousFailed, "", "could not persist action before execution: "+err.Error())
			return
		}

		if !executeAutonomousAction(ctx, sess, action) {
			return
		}
		if err := webStore.persist(); err != nil {
			logAutonomousError(sess.ID, err)
		}
	}
}

func autonomousMessages(sess *webSession) ([]ChatMessage, error) {
	run := sess.autonomousSnapshot()
	if run == nil {
		return nil, errors.New("autonomous run not found")
	}
	history := "Recent events are provided as conversation messages below."
	if run.ContextSummary != "" {
		history = "Earlier event summary:\n" + run.ContextSummary + "\n\n" + history
	}
	prompt, err := composeAutonomousPrompt(run.BasePrompt, autonomousGoalDescription(run), history)
	if err != nil {
		return nil, err
	}
	prompt += fmt.Sprintf("\n\nEXECUTION ENVIRONMENT:\nOS: %s\nWorking directory: %s\nCommands installed and allowed: %s\nAllowed but unavailable: %s\nCommands run with the same filesystem and network permissions as the OWRAP process.", run.OperatingSystem, run.WorkingDirectory, strings.Join(run.AvailableCommands, ", "), emptyAutonomousValue(strings.Join(run.UnavailableCommands, ", ")))
	if len(run.Plan) > 0 {
		planJSON, _ := json.Marshal(run.Plan)
		prompt += "\n\nCURRENT PLAN:\n" + string(planJSON)
	}
	messages := []ChatMessage{{Role: "system", Content: prompt}}
	if command := requiredGoalCommand(run); command != "" && !hasCommandObservation(run.Events, command) {
		messages[0].Content += fmt.Sprintf("\n\nEXECUTION REQUIREMENT: The goal explicitly requires %q. You must return run_command using %q and inspect its real output before returning answer. A description, refusal, or hypothetical result is not completion.", command, command)
	}
	if command, required := autonomousCommandRecoveryRequired(run.Events); required {
		messages[0].Content += fmt.Sprintf("\n\nCOMMAND RECOVERY REQUIREMENT: The latest command attempt failed: %q. Your next action must be run_command with corrected syntax or an alternative command. Do not answer, update findings, change the plan, or request clarification before making that attempt.", command)
	}
	if run.ContextSummary != "" {
		messages = append(messages, ChatMessage{Role: "user", Content: "Earlier run summary:\n" + run.ContextSummary})
	}
	for _, event := range selectAutonomousContextEvents(run.Events, run.CompactedThrough, autonomousContextTokens) {
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

func requiredGoalCommand(run *AutonomousRun) string {
	for _, field := range autonomousGoalFields(run.Goal) {
		if allowedCommands[field] {
			return field
		}
	}
	return ""
}

func autonomousGoalFields(goal string) []string {
	return strings.FieldsFunc(strings.ToLower(goal), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-'
	})
}

func hasCommandObservation(events []AutonomousEvent, requiredCommand string) bool {
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

func latestAutonomousCommandFailure(events []AutonomousEvent) (string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind == "observation" && event.Action == "run_command" {
			return event.Command, !event.Success
		}
	}
	return "", false
}

func autonomousCommandRecoveryRequired(events []AutonomousEvent) (string, bool) {
	command, failed := latestAutonomousCommandFailure(events)
	return command, failed && consecutiveAutonomousCommandFailures(events) < 2
}

func consecutiveAutonomousCommandFailures(events []AutonomousEvent) int {
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

func validateAutonomousActionState(sess *webSession, run *AutonomousRun, action ToolResponse) error {
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

func autonomousEventHistory(events []AutonomousEvent) string {
	if len(events) == 0 {
		return "No previous attempts yet."
	}
	var history strings.Builder
	for _, event := range events {
		fmt.Fprintf(&history, "%d. %s %s %s\n", event.Sequence, event.Kind, event.Action, truncateAutonomousText(event.Text+event.Output))
	}
	return history.String()
}

func estimateAutonomousTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func autonomousEventText(event AutonomousEvent) string {
	data, _ := json.Marshal(event)
	return string(data)
}

func selectAutonomousContextEvents(events []AutonomousEvent, compactedThrough, tokenBudget int) []AutonomousEvent {
	selectedStart := len(events)
	used := 0
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Sequence <= compactedThrough {
			break
		}
		tokens := estimateAutonomousTokens(autonomousEventText(events[index]))
		if selectedStart < len(events) && used+tokens > tokenBudget {
			break
		}
		selectedStart = index
		used += tokens
	}
	return append([]AutonomousEvent(nil), events[selectedStart:]...)
}

func compactAutonomousContext(ctx context.Context, sess *webSession) {
	run := sess.autonomousSnapshot()
	if run == nil {
		return
	}
	pending := make([]AutonomousEvent, 0)
	totalTokens := 0
	for _, event := range run.Events {
		if event.Sequence <= run.CompactedThrough {
			continue
		}
		pending = append(pending, event)
		totalTokens += estimateAutonomousTokens(autonomousEventText(event))
	}
	if len(pending) < 2 || totalTokens <= autonomousContextTokens {
		return
	}
	keep := selectAutonomousContextEvents(pending, 0, autonomousContextTokens/2)
	compactCount := len(pending) - len(keep)
	if compactCount <= 0 {
		return
	}
	toCompact := pending[:compactCount]
	summaryInput := truncateAutonomousText(autonomousEventHistory(toCompact))
	summaryMessages := []ChatMessage{
		{Role: "system", Content: fmt.Sprintf("Summarize autonomous-agent history in at most %d tokens. Preserve verified facts, failed approaches, unresolved questions, user constraints, and job IDs. Do not invent information.", autonomousSummaryTokens)},
		{Role: "user", Content: "Existing summary:\n" + emptyAutonomousValue(run.ContextSummary) + "\n\nEvents to compact:\n" + summaryInput},
	}
	summaryCtx, cancel := context.WithTimeout(ctx, autonomousModelTimeout)
	response, err := callOllamaMessageWithLogContext(summaryCtx, "autonomous-summary:"+run.ID, summaryMessages, false, false)
	cancel()
	summary := strings.TrimSpace(response.Content)
	if err != nil || summary == "" {
		summary = truncateAutonomousText(run.ContextSummary + "\n" + summaryInput)
	}
	sess.mu.Lock()
	if sess.AutonomousRun != nil && sess.AutonomousRun.ID == run.ID {
		sess.AutonomousRun.ContextSummary = summary
		sess.AutonomousRun.CompactedThrough = toCompact[len(toCompact)-1].Sequence
		sess.AutonomousRun.UpdatedAt = time.Now().UTC()
	}
	sess.mu.Unlock()
}

func verifyAutonomousAnswer(ctx context.Context, sess *webSession, candidate string) (AutonomousVerification, error) {
	run := sess.autonomousSnapshot()
	if run == nil {
		return AutonomousVerification{}, errors.New("autonomous run not found")
	}
	evidence := selectAutonomousContextEvents(run.Events, run.CompactedThrough, autonomousContextTokens)
	evidenceJSON, _ := json.Marshal(evidence)
	planJSON, _ := json.Marshal(run.Plan)
	messages := []ChatMessage{
		{Role: "system", Content: "You are an independent critic. Verify a candidate autonomous-agent answer against the objective, expected output, constraints, completion criteria, plan, and recorded events. Approve only supported and complete answers. Return exactly one JSON object: {\"approved\":true|false,\"feedback\":\"specific reason or empty\",\"evidenceEventIds\":[1,2]}. Cite only event IDs that directly support the answer. Knowledge-only answers may use an empty evidence list."},
		{Role: "user", Content: autonomousGoalDescription(run) + "\n\nPLAN:\n" + string(planJSON) + "\n\nCANDIDATE ANSWER:\n" + candidate + "\n\nRECORDED EVENTS:\n" + string(evidenceJSON)},
	}
	verifyCtx, cancel := context.WithTimeout(ctx, autonomousModelTimeout)
	response, err := callOllamaMessageWithLogContext(verifyCtx, "autonomous-critic:"+run.ID, messages, true, false)
	cancel()
	if err != nil {
		return AutonomousVerification{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(response.Content)))
	decoder.DisallowUnknownFields()
	var verification AutonomousVerification
	if err := decoder.Decode(&verification); err != nil {
		return AutonomousVerification{}, fmt.Errorf("invalid critic JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return AutonomousVerification{}, errors.New("critic response must contain exactly one JSON object")
	}
	for _, step := range run.Plan {
		if verification.Approved && step.Status != "completed" {
			verification.Approved = false
			verification.Feedback = fmt.Sprintf("plan step %d is not completed: %s", step.ID, step.Description)
		}
	}
	for _, eventID := range verification.EvidenceEventIDs {
		if eventID <= 0 || eventID > len(run.Events) {
			return AutonomousVerification{}, fmt.Errorf("critic cited nonexistent event %d", eventID)
		}
	}
	if command := requiredGoalCommand(run); verification.Approved && command != "" && !evidenceSupportsCommand(run.Events, verification.EvidenceEventIDs, command) {
		verification.Approved = false
		verification.Feedback = fmt.Sprintf("candidate does not cite a successful %q observation", command)
	}
	return verification, nil
}

func evidenceSupportsCommand(events []AutonomousEvent, evidenceIDs []int, command string) bool {
	for _, eventID := range evidenceIDs {
		event := events[eventID-1]
		fields := strings.Fields(event.Command)
		if event.Kind == "observation" && event.Action == "run_command" && event.Success && len(fields) > 0 && fields[0] == command {
			return true
		}
	}
	return false
}

func parseAutonomousAction(raw string) (ToolResponse, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var action ToolResponse
	if err := decoder.Decode(&action); err != nil {
		return ToolResponse{}, fmt.Errorf("invalid autonomous action JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ToolResponse{}, errors.New("autonomous response must contain exactly one JSON object")
	}
	switch action.Action {
	case "answer", "update_findings", "request_clarification":
		if strings.TrimSpace(action.Text) == "" {
			return ToolResponse{}, fmt.Errorf("action %q requires text", action.Action)
		}
	case "run_command", "run_command_bg":
		if err := validateAutonomousCommand(action.Command); err != nil {
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
		return ToolResponse{}, fmt.Errorf("unsupported autonomous action %q", action.Action)
	}
	return action, nil
}

func validateAutonomousCommand(command string) error {
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

func executeAutonomousAction(ctx context.Context, sess *webSession, action ToolResponse) bool {
	switch action.Action {
	case "answer":
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "answer", Action: action.Action, Text: action.Text, EvidenceEventIDs: action.EvidenceEventIDs})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: action.Text})
		sess.mu.Lock()
		sess.AwaitingDecision = true
		sess.AutonomousRun.Status = autonomousWaitingApproval
		sess.AutonomousRun.FinalAnswer = action.Text
		sess.AutonomousRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "request_clarification":
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "clarification", Action: action.Action, Text: action.Text})
		sess.mu.Lock()
		sess.AwaitingDecision = true
		sess.AutonomousRun.Status = autonomousWaitingInput
		sess.AutonomousRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "update_findings":
		sess.mu.Lock()
		sess.PartialFindings = action.Text
		sess.mu.Unlock()
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Text: "Findings saved: " + action.Text})
		return true
	case "create_plan":
		plan := make([]AutonomousPlanStep, 0, len(action.Steps))
		for index, description := range action.Steps {
			plan = append(plan, AutonomousPlanStep{ID: index + 1, Description: strings.TrimSpace(description), Status: "pending"})
		}
		sess.mu.Lock()
		sess.AutonomousRun.Plan = plan
		sess.mu.Unlock()
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "plan", Action: action.Action, Text: strings.Join(action.Steps, "\n")})
		return true
	case "complete_step":
		sess.mu.Lock()
		step := &sess.AutonomousRun.Plan[action.StepID-1]
		step.Status = "completed"
		step.EvidenceEventIDs = append([]int(nil), action.EvidenceEventIDs...)
		sess.mu.Unlock()
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "plan", Action: action.Action, StepID: action.StepID, EvidenceEventIDs: action.EvidenceEventIDs, Text: fmt.Sprintf("Plan step %d completed", action.StepID)})
		return true
	case "run_command":
		commandCtx, cancel := context.WithTimeout(ctx, autonomousCommandTimeout)
		output, success := runAutonomousCommand(commandCtx, action.Command)
		cancel()
		sess.mu.Lock()
		sess.CommandCount++
		sess.Stats.recordCommand(action.Command)
		sess.mu.Unlock()
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Command: action.Command, Output: truncateAutonomousText(output), Success: success})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: fmt.Sprintf("[Running]: %s\n[Command output]:\n%s", action.Command, output)})
		return true
	case "run_command_bg":
		job, err := executeBackgroundCommand(sess.ID, action.Command)
		if err != nil {
			sess.appendAutonomousEvent(AutonomousEvent{Kind: "error", Action: action.Action, Text: err.Error()})
			return true
		}
		if run := sess.autonomousSnapshot(); run != nil {
			job.RunID = run.ID
			jobStore.update(job)
		}
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("job %s started", job.ID)})
		return true
	case "check_job", "get_job":
		job, ok := jobStore.get(action.JobID)
		if !ok || job.SessionID != sess.ID {
			sess.appendAutonomousEvent(AutonomousEvent{Kind: "error", Action: action.Action, Text: "job not found"})
			return true
		}
		output := job.Output
		if action.Action == "check_job" && len(output) > 1000 {
			output = output[:1000] + "\n... (truncated)"
		}
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Text: fmt.Sprintf("job %s status: %s\n", job.ID, job.Status), Output: truncateAutonomousText(output)})
		return true
	case "cancel_job":
		job, ok := jobStore.get(action.JobID)
		if ok && job.SessionID == sess.ID && job.Cmd != nil && job.Cmd.Process != nil {
			_ = job.Cmd.Process.Kill()
			job.Status = "cancelled"
			job.EndTime = time.Now()
			jobStore.update(job)
		}
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Text: "job cancellation processed"})
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
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Text: text.String()})
		return true
	}
	return true
}

func runAutonomousCommand(ctx context.Context, command string) (string, bool) {
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

func recordAutonomousFailure(sess *webSession, message string) bool {
	sess.mu.Lock()
	if sess.AutonomousRun == nil {
		sess.mu.Unlock()
		return false
	}
	sess.AutonomousRun.ConsecutiveErrors++
	failures := sess.AutonomousRun.ConsecutiveErrors
	sess.mu.Unlock()
	sess.appendAutonomousEvent(AutonomousEvent{Kind: "error", Text: message})
	if failures >= 3 {
		finishAutonomous(sess, autonomousFailed, "", message)
		return false
	}
	return true
}

func recordAutonomousRevision(sess *webSession, message string, evidenceEventIDs []int) bool {
	sess.mu.Lock()
	if sess.AutonomousRun == nil {
		sess.mu.Unlock()
		return false
	}
	sess.AutonomousRun.ConsecutiveErrors++
	failures := sess.AutonomousRun.ConsecutiveErrors
	sess.mu.Unlock()
	sess.appendAutonomousEvent(AutonomousEvent{Kind: "verification", Action: "answer", Text: message, EvidenceEventIDs: evidenceEventIDs})
	if failures >= 3 {
		finishAutonomous(sess, autonomousFailed, "", message)
		return false
	}
	return true
}

func resetAutonomousFailures(sess *webSession) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.AutonomousRun != nil {
		sess.AutonomousRun.ConsecutiveErrors = 0
	}
}

func finishAutonomous(sess *webSession, status AutonomousRunStatus, answer, errorText string) {
	sess.finishAutonomousRun(status, answer, errorText)
	text := errorText
	if answer != "" {
		text = answer
	}
	sess.appendAutonomousEvent(AutonomousEvent{Kind: "status", Text: string(status) + ": " + text})
	cleanupAutonomousRunResources(sess)
}

func cleanupAutonomousRunResources(sess *webSession) {
	run := sess.autonomousSnapshot()
	if run != nil {
		jobStore.cancelRun(run.ID)
	}
	sess.mu.Lock()
	attachmentPath := sess.AttachedFilePath
	sess.AttachedFile = nil
	sess.AttachedFilePath = ""
	sess.AutonomousStart = 0
	sess.AwaitingDecision = false
	sess.mu.Unlock()
	if attachmentPath != "" {
		if dir, err := owrapAutonomousSessionDir(sess.ID); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
}

func truncateAutonomousText(text string) string {
	if len(text) <= autonomousMaxObservation {
		return text
	}
	return text[:autonomousMaxObservation] + "\n... (truncated)"
}

func (s *webSession) appendAgentMessage(message ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, message)
	s.Stats.recordMessage(message)
}

func logAutonomousError(sessionID string, err error) {
	fmt.Printf("[AUTONOMOUS] session=%s persistence error: %v\n", sessionID, err)
}

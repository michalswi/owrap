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
	"strings"
	"time"
)

const (
	autonomousMaxIterations  = 30
	autonomousRunTimeout     = 30 * time.Minute
	autonomousModelTimeout   = 2 * time.Minute
	autonomousCommandTimeout = 2 * time.Minute
	autonomousMaxObservation = 32 * 1024
	autonomousContextEvents  = 12
)

type AutonomousRunStatus string

const (
	autonomousRunning         AutonomousRunStatus = "running"
	autonomousWaitingApproval AutonomousRunStatus = "waiting_approval"
	autonomousCompleted       AutonomousRunStatus = "completed"
	autonomousFailed          AutonomousRunStatus = "failed"
	autonomousCancelled       AutonomousRunStatus = "cancelled"
	autonomousLimitReached    AutonomousRunStatus = "limit_reached"
)

type AutonomousEvent struct {
	Sequence  int       `json:"sequence"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action,omitempty"`
	Text      string    `json:"text,omitempty"`
	Command   string    `json:"command,omitempty"`
	Output    string    `json:"output,omitempty"`
	Thinking  string    `json:"thinking,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type AutonomousRun struct {
	ID                string              `json:"id"`
	SessionID         string              `json:"sessionId"`
	Goal              string              `json:"goal"`
	Status            AutonomousRunStatus `json:"status"`
	Iteration         int                 `json:"iteration"`
	MaxIterations     int                 `json:"maxIterations"`
	StartedAt         time.Time           `json:"startedAt"`
	UpdatedAt         time.Time           `json:"updatedAt"`
	CompletedAt       time.Time           `json:"completedAt,omitempty"`
	BasePrompt        string              `json:"basePrompt"`
	BasePromptName    string              `json:"basePromptName"`
	FinalAnswer       string              `json:"finalAnswer,omitempty"`
	Error             string              `json:"error,omitempty"`
	Events            []AutonomousEvent   `json:"events"`
	ConsecutiveErrors int                 `json:"consecutiveErrors"`
}

func newAutonomousRun(sessionID, goal, basePrompt, basePromptName string) *AutonomousRun {
	now := time.Now().UTC()
	return &AutonomousRun{
		ID:             fmt.Sprintf("run_%d", time.Now().UnixNano()),
		SessionID:      sessionID,
		Goal:           goal,
		Status:         autonomousRunning,
		MaxIterations:  autonomousMaxIterations,
		StartedAt:      now,
		UpdatedAt:      now,
		BasePrompt:     basePrompt,
		BasePromptName: basePromptName,
		Events:         make([]AutonomousEvent, 0),
	}
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
	s.AutonomousMode = status == autonomousRunning || status == autonomousWaitingApproval
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
		resetAutonomousFailures(sess)
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "action", Action: action.Action, Text: action.Text, Command: action.Command, Thinking: response.Thinking})
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
	history := autonomousEventHistory(run.Events)
	prompt, err := composeAutonomousPrompt(run.BasePrompt, run.Goal, history)
	if err != nil {
		return nil, err
	}
	messages := []ChatMessage{{Role: "system", Content: prompt}}
	start := len(run.Events) - autonomousContextEvents
	if start < 0 {
		start = 0
	}
	for _, event := range run.Events[start:] {
		switch event.Kind {
		case "action", "invalid_action":
			if event.Kind == "invalid_action" {
				messages = append(messages, ChatMessage{Role: "assistant", Content: event.Text})
			} else {
				content, _ := json.Marshal(ToolResponse{Action: event.Action, Command: event.Command, Text: event.Text})
				messages = append(messages, ChatMessage{Role: "assistant", Content: string(content)})
			}
		case "observation", "error":
			messages = append(messages, ChatMessage{Role: "user", Content: "Tool observation:\n" + event.Text + "\n" + event.Output})
		case "feedback":
			messages = append(messages, ChatMessage{Role: "user", Content: "User feedback:\n" + event.Text})
		}
	}
	if len(messages) == 1 {
		messages = append(messages, ChatMessage{Role: "user", Content: "Begin working on the goal. Answer directly if no tool is needed."})
	} else {
		messages = append(messages, ChatMessage{Role: "user", Content: "Choose the next action. If the goal is complete, return answer."})
	}
	return messages, nil
}

func autonomousEventHistory(events []AutonomousEvent) string {
	if len(events) == 0 {
		return "No previous attempts yet."
	}
	start := len(events) - autonomousContextEvents
	if start < 0 {
		start = 0
	}
	var history strings.Builder
	for _, event := range events[start:] {
		fmt.Fprintf(&history, "%d. %s %s %s\n", event.Sequence, event.Kind, event.Action, truncateAutonomousText(event.Text+event.Output))
	}
	return history.String()
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
	case "answer", "update_findings":
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
		if !allowedCommands[fields[0]] {
			return fmt.Errorf("command %q is not allowed", fields[0])
		}
	}
	return nil
}

func executeAutonomousAction(ctx context.Context, sess *webSession, action ToolResponse) bool {
	switch action.Action {
	case "answer":
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "answer", Action: action.Action, Text: action.Text})
		sess.appendAgentMessage(ChatMessage{Role: "assistant", Content: action.Text})
		sess.mu.Lock()
		sess.AwaitingDecision = true
		sess.AutonomousRun.Status = autonomousWaitingApproval
		sess.AutonomousRun.FinalAnswer = action.Text
		sess.AutonomousRun.UpdatedAt = time.Now().UTC()
		sess.mu.Unlock()
		return false
	case "update_findings":
		sess.mu.Lock()
		sess.PartialFindings = action.Text
		sess.mu.Unlock()
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Text: "Findings saved: " + action.Text})
		return true
	case "run_command":
		commandCtx, cancel := context.WithTimeout(ctx, autonomousCommandTimeout)
		output := runAutonomousCommand(commandCtx, action.Command)
		cancel()
		sess.mu.Lock()
		sess.CommandCount++
		sess.Stats.recordCommand(action.Command)
		sess.mu.Unlock()
		sess.appendAutonomousEvent(AutonomousEvent{Kind: "observation", Action: action.Action, Command: action.Command, Output: truncateAutonomousText(output)})
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

func runAutonomousCommand(ctx context.Context, command string) string {
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
		return "(no output)"
	}
	return output
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

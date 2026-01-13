package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michalswi/color"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

//go:embed webstatic/*
var webStatic embed.FS

type webSession struct {
	ID                 string
	Messages           []ChatMessage
	CreatedAt          time.Time
	Stats              Stats
	AutonomousMode     bool
	AutonomousGoal     string
	OriginalPrompt     string
	OriginalPromptName string
	RetryCount         int
	CommandCount       int    // Tracks commands executed in autonomous mode
	PartialFindings    string // Stores completed parts of the goal to avoid re-running
}

type webSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*webSession
}

func newWebSessionStore() *webSessionStore {
	return &webSessionStore{sessions: make(map[string]*webSession)}
}

func (s *webSessionStore) get(id string) (*webSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *webSessionStore) create() *webSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("sess_%d", time.Now().UnixNano())
	sess := &webSession{ID: id, CreatedAt: time.Now()}
	s.sessions[id] = sess
	return sess
}

func (s *webSessionStore) ensure(id string) *webSession {
	if id == "" {
		return s.create()
	}
	if sess, ok := s.get(id); ok {
		return sess
	}
	return s.create()
}

var webStore = newWebSessionStore()

// Background job management
type BackgroundJob struct {
	ID        string
	Command   string
	Status    string // "running", "completed", "failed", "timeout"
	Output    string
	Error     string
	StartTime time.Time
	EndTime   time.Time
	Cmd       *exec.Cmd
	SessionID string
}

type JobStore struct {
	mu   sync.RWMutex
	jobs map[string]*BackgroundJob
}

func newJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*BackgroundJob)}
}

func (s *JobStore) get(id string) (*BackgroundJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *JobStore) create(sessionID, command string) *BackgroundJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("job_%d", time.Now().UnixNano())
	job := &BackgroundJob{
		ID:        id,
		SessionID: sessionID,
		Command:   command,
		Status:    "running",
		StartTime: time.Now(),
	}
	s.jobs[id] = job
	return job
}

func (s *JobStore) listBySession(sessionID string) []*BackgroundJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*BackgroundJob
	for _, job := range s.jobs {
		if job.SessionID == sessionID {
			result = append(result, job)
		}
	}
	return result
}

func (s *JobStore) update(job *BackgroundJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

var jobStore = newJobStore()

func accent(s string) string  { return color.Format(color.PURPLE, s) }
func info(s string) string    { return color.Format(color.BLUE, s) }
func success(s string) string { return color.Format(color.GREEN, s) }
func warn(s string) string    { return color.Format(color.RED, s) }
func userLabel() string       { return color.Format(color.YELLOW, "You: ") }
func assistantLabel() string  { return color.Format(color.GREEN, "Assistant:") }
func analysisLabel() string   { return color.Format(color.GREEN, "Assistant (analysis):") }
func runningLabel() string    { return color.Format(color.BLUE, "[Running]:") }
func outputLabel() string     { return color.Format(color.BLUE, "[Command output]:") }
func separatorLine() string   { return color.Format(color.MINGLE, separator) }

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Format   string        `json:"format,omitempty"` // "json" to force JSON output
}

type ChatResponse struct {
	Message ChatMessage `json:"message"`
}

type ToolResponse struct {
	Action  string `json:"action"`            // "run_command", "answer", "update_findings", "run_command_bg", "check_job", "get_job", "cancel_job", "list_jobs"
	Command string `json:"command,omitempty"` // when action == run_command or run_command_bg
	Text    string `json:"text,omitempty"`    // when action == answer or update_findings
	JobID   string `json:"jobId,omitempty"`   // for job-related actions
}

type webChatRequest struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

type webChatResponse struct {
	SessionID          string           `json:"sessionId"`
	Action             string           `json:"action"`
	AssistantText      string           `json:"assistantText,omitempty"`
	Command            string           `json:"command,omitempty"`
	CommandOutput      string           `json:"commandOutput,omitempty"`
	Raw                string           `json:"raw,omitempty"`
	Messages           []ChatMessage    `json:"messages"`
	Model              string           `json:"model"`
	Timestamp          time.Time        `json:"timestamp"`
	Stats              Stats            `json:"stats"`
	AutonomousMode     bool             `json:"autonomousMode,omitempty"`
	AutonomousContinue bool             `json:"autonomousContinue,omitempty"`
	CommandCount       int              `json:"commandCount,omitempty"` // Commands executed in autonomous mode
	JobID              string           `json:"jobId,omitempty"`
	JobStatus          string           `json:"jobStatus,omitempty"`
	Jobs               []*BackgroundJob `json:"jobs,omitempty"`
}

type webCommandRequest struct {
	Command string `json:"command"`
}

type webCommandResponse struct {
	Command   string    `json:"command"`
	Output    string    `json:"output"`
	Timestamp time.Time `json:"timestamp"`
}

type Session struct {
	Timestamp    string        `json:"timestamp"`
	Model        string        `json:"model"`
	Messages     []ChatMessage `json:"messages"`
	Stats        Stats         `json:"stats"`
	CachedBlocks []string      `json:"cached_blocks"`
}

type Stats struct {
	UserMessages        int
	AssistantMessages   int
	CommandsRun         int
	TotalUserChars      int
	TotalAssistantChars int
	LastCommand         string
}

func (s *Stats) recordUser(msg string) {
	s.UserMessages++
	s.TotalUserChars += len(msg)
}

func (s *Stats) recordAssistant(msg string) {
	s.AssistantMessages++
	s.TotalAssistantChars += len(msg)
}

func (s *Stats) recordCommand(cmd string) {
	s.CommandsRun++
	s.LastCommand = cmd
}

func allowedCommandsList() []string {
	keys := make([]string, 0, len(allowedCommands))
	for k := range allowedCommands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func callOllama(messages []ChatMessage, forceJSON bool) (string, error) {
	reqBody := ChatRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
	}
	if forceJSON {
		reqBody.Format = "json"
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
		return "", err
	}

	resp, err := http.Post(ollamaURL, "application/json", &buf)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var cr ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	return cr.Message.Content, nil
}

func callOllamaWithLog(origin string, messages []ChatMessage, forceJSON bool) (string, error) {
	start := time.Now()
	resp, err := callOllama(messages, forceJSON)
	dur := time.Since(start)
	jsonMode := ""
	if forceJSON {
		jsonMode = " json-mode=true"
	}
	log.Printf("[ollama][%s] messages=%d model=%s%s dur=%s err=%v", origin, len(messages), modelName, jsonMode, dur.Round(time.Millisecond), err)
	return resp, err
}

func runCommand(cmdStr string) string {
	sanitized := sanitizeCommand(cmdStr)

	// If command contains &&, ||, or complex shell syntax, run through bash -c
	if strings.Contains(sanitized, "&&") || strings.Contains(sanitized, "||") ||
		strings.Contains(sanitized, ";") {
		cmd := exec.Command("bash", "-c", sanitized)
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf

		err := cmd.Run()
		out := outBuf.String()
		if errBuf.Len() > 0 {
			out += "\n[stderr]\n" + errBuf.String()
		}
		if err != nil && strings.TrimSpace(out) == "" {
			return fmt.Sprintf("Error running command: %v\nstderr: %s", err, errBuf.String())
		}
		if strings.TrimSpace(out) == "" {
			return "(no output)"
		}
		return out
	}

	// If multiple lines, execute sequentially and aggregate
	if strings.Contains(sanitized, "\n") {
		var parts []string
		for _, line := range strings.Split(sanitized, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			res := runSingleCommand(line)
			parts = append(parts, fmt.Sprintf("$ %s\n%s", line, res))
		}
		if len(parts) == 0 {
			return "Empty command."
		}
		return strings.Join(parts, "\n\n")
	}

	return runSingleCommand(sanitized)
}

// Execute command in background as a job
func executeBackgroundCommand(sessionID, command string) (*BackgroundJob, error) {
	sanitized := sanitizeCommand(command)
	if sanitized == "" {
		return nil, errors.New("empty command after sanitization")
	}

	job := jobStore.create(sessionID, sanitized)

	// Create command
	cmd := exec.Command("bash", "-c", sanitized)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	job.Cmd = cmd

	log.Printf("[JOB] Starting background job %s: %s", job.ID, sanitized)

	// Run in goroutine with timeout
	go func() {
		const MaxJobDuration = 1 * time.Hour
		done := make(chan error, 1)

		go func() {
			done <- cmd.Run()
		}()

		select {
		case err := <-done:
			job.EndTime = time.Now()
			job.Output = outBuf.String()

			if err != nil {
				job.Status = "failed"
				job.Error = err.Error()
				if errBuf.Len() > 0 {
					job.Error += "\\n" + errBuf.String()
				}
				log.Printf("[JOB] Job %s failed: %v", job.ID, err)
			} else {
				job.Status = "completed"
				log.Printf("[JOB] Job %s completed successfully", job.ID)
			}
			jobStore.update(job)

		case <-time.After(MaxJobDuration):
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			job.EndTime = time.Now()
			job.Status = "timeout"
			job.Error = fmt.Sprintf("Job exceeded maximum duration of %v", MaxJobDuration)
			job.Output = outBuf.String()
			log.Printf("[JOB] Job %s timed out", job.ID)
			jobStore.update(job)
		}
	}()

	return job, nil
}

func runSingleCommand(sanitized string) string {
	fields := strings.Fields(sanitized)
	if len(fields) == 0 {
		return "Empty command."
	}

	segments := splitPipeline(fields)
	if len(segments) > 2 {
		return "Only a single pipe is supported."
	}

	// Pipeline or tee handling
	if len(segments) == 2 {
		left := segments[0]
		right := segments[1]
		if len(left) == 0 || len(right) == 0 {
			return "Invalid pipeline syntax."
		}
		if !allowedCommands[left[0]] {
			return fmt.Sprintf("Command '%s' is not allowed.", left[0])
		}

		// tee case
		if right[0] == "tee" {
			appendMode := false
			var target string
			for _, tok := range right[1:] {
				if tok == "-a" {
					appendMode = true
					continue
				}
				if strings.HasPrefix(tok, "-") {
					continue
				}
				if target == "" {
					target = tok
				}
			}
			if target == "" {
				return "tee requires a target filename"
			}

			stdout1, stderr1, err := runSimple(left, nil)
			if err != nil {
				return fmt.Sprintf("Error running command: %v\nstderr: %s", err, stderr1)
			}

			flag := os.O_CREATE | os.O_WRONLY
			if appendMode {
				flag |= os.O_APPEND
			} else {
				flag |= os.O_TRUNC
			}
			f, err := os.OpenFile(target, flag, 0644)
			if err != nil {
				return fmt.Sprintf("Failed to open tee target %s: %v", target, err)
			}
			if _, err := f.WriteString(stdout1); err != nil {
				f.Close()
				return fmt.Sprintf("Failed to write tee target %s: %v", target, err)
			}
			f.Close()
			msg := stdout1
			if strings.TrimSpace(msg) == "" {
				msg = "(no output)"
			}
			if strings.TrimSpace(stderr1) != "" {
				msg += "\n[stderr]\n" + stderr1
			}
			msg += fmt.Sprintf("\n(output written to %s)%s", target, ternary(appendMode, " (append)", ""))
			return msg
		}

		// General single pipe command1 | command2
		if !allowedCommands[right[0]] {
			return fmt.Sprintf("Command '%s' is not allowed.", right[0])
		}
		stdout1, stderr1, err := runSimple(left, nil)
		if err != nil {
			return fmt.Sprintf("Error running command: %v\nstderr: %s", err, stderr1)
		}
		stdout2, stderr2, err := runSimple(right, strings.NewReader(stdout1))
		if err != nil {
			return fmt.Sprintf("Error running command: %v\nstderr: %s\n[stderr from first]\n%s", err, stderr2, stderr1)
		}
		out := stdout2
		if strings.TrimSpace(stderr1) != "" {
			out += "\n[stderr first]\n" + stderr1
		}
		if strings.TrimSpace(stderr2) != "" {
			out += "\n[stderr second]\n" + stderr2
		}
		if strings.TrimSpace(out) == "" {
			return "(no output)"
		}
		return out
	}

	// No pipe: support optional > or >> redirection
	if !allowedCommands[fields[0]] {
		return fmt.Sprintf("Command '%s' is not allowed.", fields[0])
	}

	args, redirectFile, appendMode, parseErr := splitRedirection(fields)
	if parseErr != nil {
		return parseErr.Error()
	}

	cmd := exec.Command(args[0], args[1:]...)
	var outBuf, errBuf bytes.Buffer
	var stdoutWriter io.Writer = &outBuf

	var file *os.File
	if redirectFile != "" {
		flag := os.O_CREATE | os.O_WRONLY
		if appendMode {
			flag |= os.O_APPEND
		} else {
			flag |= os.O_TRUNC
		}
		f, err := os.OpenFile(redirectFile, flag, 0644)
		if err != nil {
			return fmt.Sprintf("Failed to open redirect file %s: %v", redirectFile, err)
		}
		file = f
		stdoutWriter = io.MultiWriter(&outBuf, file)
	}

	cmd.Stdout = stdoutWriter
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		if file != nil {
			file.Close()
		}
		return fmt.Sprintf("Error running command: %v\nstderr: %s", err, errBuf.String())
	}

	if file != nil {
		file.Close()
	}

	out := outBuf.String()
	if errBuf.Len() > 0 {
		out += "\n[stderr]\n" + errBuf.String()
	}
	if redirectFile != "" {
		if strings.TrimSpace(out) == "" {
			out = fmt.Sprintf("(output written to %s)", redirectFile)
		} else {
			out += fmt.Sprintf("\n(output also written to %s)", redirectFile)
		}
	}
	if strings.TrimSpace(out) == "" {
		return "(no output)"
	}
	return out
}

// sanitizeCommand trims common wrappers the model might add (backticks, leading $).
func sanitizeCommand(cmd string) string {
	c := strings.TrimSpace(cmd)
	c = strings.Trim(c, "`")
	c = strings.TrimPrefix(c, "$")
	return strings.TrimSpace(c)
}

func splitPipeline(fields []string) [][]string {
	var segments [][]string
	start := 0
	for i, tok := range fields {
		if tok == "|" {
			segments = append(segments, fields[start:i])
			start = i + 1
		}
	}
	segments = append(segments, fields[start:])
	return segments
}

// execCommandsFromFile runs each non-empty line in a file, sequentially, without analysis.
func execCommandsFromFile(path string, stats *Stats) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Failed to read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	var parts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		res := runSingleCommand(line)
		stats.recordCommand(line)
		parts = append(parts, fmt.Sprintf("$ %s\n%s", line, res))
	}
	if len(parts) == 0 {
		return "No commands executed (empty file?)"
	}
	return strings.Join(parts, "\n\n")
}

func runSimple(args []string, stdin io.Reader) (string, string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	var outBuf, errBuf bytes.Buffer
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return outBuf.String(), errBuf.String(), err
	}
	return outBuf.String(), errBuf.String(), nil
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// splitRedirection extracts a single stdout redirection (> or >> filename) if present.
func splitRedirection(fields []string) (args []string, redirectFile string, appendMode bool, err error) {
	for i := 0; i < len(fields); i++ {
		tok := fields[i]
		if tok == ">" || tok == ">>" {
			if i+1 >= len(fields) {
				return nil, "", false, fmt.Errorf("missing filename for redirection")
			}
			redirectFile = fields[i+1]
			appendMode = tok == ">>"
			args = append(args, fields[:i]...)
			return args, redirectFile, appendMode, nil
		}
	}
	return fields, "", false, nil
}

func startWebUI(bindAddr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", handleWebChat)
	mux.HandleFunc("/api/prompt", handleWebPrompt)
	mux.HandleFunc("/api/prompts/list", handleListPrompts)
	mux.HandleFunc("/api/prompts/update", handleUpdatePrompt)
	mux.HandleFunc("/api/help", handleWebHelp)
	mux.HandleFunc("/api/command", handleWebCommand)
	mux.HandleFunc("/api/autonomous/start", handleAutonomousStart)
	mux.HandleFunc("/api/autonomous/stop", handleAutonomousStop)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/ollama/status", handleOllamaStatus)

	fileServer := http.FileServer(http.FS(webStatic))
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("/", serveWebIndex)

	log.Printf("web UI listening on %s (model=%s, prompt=%s, chars=%d)\n", bindAddr, modelName, systemPromptName, len(systemPrompt))
	return http.ListenAndServe(bindAddr, mux)
}

func serveWebIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webStatic.ReadFile("webstatic/index.html")
	if err != nil {
		http.Error(w, "index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func handleWebPrompt(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"prompt":  systemPrompt,
		"name":    systemPromptName,
		"model":   modelName,
		"version": version,
	})
}

func handleListPrompts(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	promptsDir := "prompts"
	var prompts []string

	files, err := os.ReadDir(promptsDir)
	if err == nil {
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".txt") {
				prompts = append(prompts, file.Name())
			}
		}
	}

	sort.Strings(prompts)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"prompts": prompts,
		"current": systemPromptName,
	})
}

type UpdatePromptRequest struct {
	Source string `json:"source"` // "file" or "custom"
	File   string `json:"file"`   // filename if source=file
	Custom string `json:"custom"` // custom text if source=custom
}

func handleUpdatePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	var req UpdatePromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request"})
		return
	}

	var newPrompt string
	var newName string

	if req.Source == "file" {
		filePath := filepath.Join("prompts", req.File)
		data, err := os.ReadFile(filePath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to read prompt file"})
			return
		}
		newPrompt = string(data)
		newName = req.File
	} else if req.Source == "custom" {
		newPrompt = strings.TrimSpace(req.Custom)
		if newPrompt == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Custom prompt cannot be empty"})
			return
		}
		newName = "custom"
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid source"})
		return
	}

	systemPrompt = newPrompt
	systemPromptName = newName

	writeJSON(w, http.StatusOK, map[string]string{
		"success": "Prompt updated successfully",
		"name":    systemPromptName,
	})
}

func handleWebHelp(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"help": webHelpText})
}

func handleOllamaStatus(w http.ResponseWriter, _ *http.Request) {
	// Try to ping Ollama with a minimal request
	client := &http.Client{Timeout: 2 * time.Second}

	// Use the tags endpoint which is lightweight
	baseURL := strings.TrimSuffix(ollamaURL, "/api/chat")
	tagsURL := baseURL + "/api/tags"

	resp, err := client.Get(tagsURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ready": false,
			"error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	ready := resp.StatusCode == http.StatusOK
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ready": ready,
	})
}

type AutoStartRequest struct {
	SessionID string `json:"sessionId"`
	Goal      string `json:"goal"`
}

func handleAutonomousStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AutoStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	req.Goal = strings.TrimSpace(req.Goal)
	if req.Goal == "" {
		http.Error(w, "goal required", http.StatusBadRequest)
		return
	}

	sess := webStore.ensure(req.SessionID)

	// Save original prompt
	sess.OriginalPrompt = systemPrompt
	sess.OriginalPromptName = systemPromptName

	// Activate autonomous mode
	sess.AutonomousMode = true
	sess.AutonomousGoal = req.Goal

	log.Printf("[AUTONOMOUS] Mode activated for session %s: goal=%s", sess.ID, req.Goal)

	// Load autonomous system prompt
	promptPath := "prompts/autonomous_agent.txt"
	data, err := os.ReadFile(promptPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load autonomous prompt: %v", err), http.StatusInternalServerError)
		return
	}

	// Replace placeholders
	autonomousPrompt := string(data)
	autonomousPrompt = strings.ReplaceAll(autonomousPrompt, "{GOAL_DESCRIPTION}", req.Goal)
	autonomousPrompt = strings.ReplaceAll(autonomousPrompt, "{ITERATION_HISTORY}", "No previous attempts yet.")

	// Update system prompt
	systemPrompt = autonomousPrompt
	systemPromptName = "autonomous_agent.txt"

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessionId": sess.ID,
		"status":    "autonomous_mode_active",
		"message":   fmt.Sprintf("Autonomous mode activated. Goal: %s", req.Goal),
		"goal":      req.Goal,
	})
}

func handleAutonomousStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	sess := webStore.ensure(req.SessionID)

	if !sess.AutonomousMode {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"sessionId": sess.ID,
			"status":    "not_active",
			"message":   "Autonomous mode was not active",
		})
		return
	}

	// Restore original prompt
	if sess.OriginalPrompt != "" {
		systemPrompt = sess.OriginalPrompt
		systemPromptName = sess.OriginalPromptName
	} else {
		// Fallback to default
		systemPrompt = defaultSystemPrompt
		systemPromptName = "default"
	}

	// Deactivate autonomous mode
	sess.AutonomousMode = false
	sess.AutonomousGoal = ""

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessionId": sess.ID,
		"status":    "autonomous_mode_stopped",
		"message":   "Autonomous mode deactivated. System prompt restored.",
		"prompt":    systemPromptName,
	})
}

func buildIterationHistory(sess *webSession) string {
	if len(sess.Messages) == 0 {
		return "No previous attempts yet."
	}

	// Get last few messages to show what happened
	start := len(sess.Messages) - 6
	if start < 0 {
		start = 0
	}

	var history strings.Builder
	for i := start; i < len(sess.Messages); i++ {
		msg := sess.Messages[i]
		if msg.Role == "user" {
			history.WriteString(fmt.Sprintf("User: %s\n", msg.Content))
		} else if msg.Role == "assistant" {
			// Truncate long outputs
			content := msg.Content
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			history.WriteString(fmt.Sprintf("Assistant: %s\n", content))
		}
	}

	if history.Len() == 0 {
		return "No previous attempts yet."
	}
	return history.String()
}

func updateAutonomousPrompt(goal, history string) string {
	promptPath := "prompts/autonomous_agent.txt"
	data, err := os.ReadFile(promptPath)
	if err != nil {
		return defaultSystemPrompt
	}

	prompt := string(data)
	prompt = strings.ReplaceAll(prompt, "{GOAL_DESCRIPTION}", goal)
	prompt = strings.ReplaceAll(prompt, "{ITERATION_HISTORY}", history)

	return prompt
}

func handleWebCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req webCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		http.Error(w, "command required", http.StatusBadRequest)
		return
	}

	output := runCommand(req.Command)
	writeJSON(w, http.StatusOK, webCommandResponse{
		Command:   req.Command,
		Output:    output,
		Timestamp: time.Now().UTC(),
	})
}

func handleWebChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req webChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}

	sess := webStore.ensure(req.SessionID)

	log.Printf("[CHAT] Session %s, autonomous: %v", sess.ID, sess.AutonomousMode)

	lower := strings.ToLower(strings.TrimSpace(req.Message))
	switch lower {
	case "/auto-on":
		autoAnalyze = true
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: "Auto-analysis enabled (after commands).",
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
		return
	case "/auto-off":
		autoAnalyze = false
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: "Auto-analysis disabled.",
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
		return
	case "/last":
		if len(sess.Messages) == 0 {
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "answer",
				AssistantText: "No prompt/answer captured yet.",
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}
		lastUser := -1
		for i := len(sess.Messages) - 1; i >= 0; i-- {
			if sess.Messages[i].Role == "user" {
				lastUser = i
				break
			}
		}
		if lastUser == -1 {
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "answer",
				AssistantText: "No prompt/answer captured yet.",
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}
		nextAssistant := -1
		for i := lastUser + 1; i < len(sess.Messages); i++ {
			if sess.Messages[i].Role == "assistant" {
				nextAssistant = i
				break
			}
		}
		msg := "(No assistant reply yet after that prompt.)"
		if nextAssistant != -1 {
			msg = sess.Messages[nextAssistant].Content
		}
		resp := fmt.Sprintf("Last prompt:\n%s\n\nAnswer:\n%s", sess.Messages[lastUser].Content, msg)
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: resp,
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
		return
	case "/stats", "/s":
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: formatStatsPlain(sess.Stats),
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
		return
	case "/allowedcomm":
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: "Allowed commands:\n" + strings.Join(allowedCommandsList(), ", "),
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
		return
	default:
		// Check for /save with optional name
		if strings.HasPrefix(lower, "/save") {
			parts := strings.Fields(req.Message)
			var sessionName string
			if len(parts) > 1 {
				sessionName = parts[1]
			}
			session := Session{
				Timestamp:    time.Now().UTC().Format(time.RFC3339),
				Model:        modelName,
				Messages:     sess.Messages,
				Stats:        sess.Stats,
				CachedBlocks: nil,
			}
			path, err := saveSessionToFile(session, sessionName)
			text := "Session saved."
			if err != nil {
				text = fmt.Sprintf("Save failed: %v", err)
			} else if path != "" {
				text = fmt.Sprintf("Session saved to %s", path)
			}
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "answer",
				AssistantText: text,
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}
		// Check for /load <name>
		if strings.HasPrefix(lower, "/load") {
			parts := strings.Fields(req.Message)
			if len(parts) < 2 {
				writeJSON(w, http.StatusOK, webChatResponse{
					SessionID:     sess.ID,
					Action:        "answer",
					AssistantText: "Usage: /load <session-name>",
					Messages:      sess.Messages,
					Model:         modelName,
					Timestamp:     time.Now().UTC(),
					Stats:         sess.Stats,
				})
				return
			}
			sessionName := parts[1]
			loadedSession, err := loadSessionFromFile(sessionName)
			if err != nil {
				writeJSON(w, http.StatusOK, webChatResponse{
					SessionID:     sess.ID,
					Action:        "answer",
					AssistantText: fmt.Sprintf("Load failed: %v", err),
					Messages:      sess.Messages,
					Model:         modelName,
					Timestamp:     time.Now().UTC(),
					Stats:         sess.Stats,
				})
				return
			}
			// Restore session data
			sess.Messages = loadedSession.Messages
			sess.Stats = loadedSession.Stats
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "session_loaded",
				AssistantText: fmt.Sprintf("Session '%s' loaded successfully. Messages: %d user, %d assistant. You can continue from here.", sessionName, loadedSession.Stats.UserMessages, loadedSession.Stats.AssistantMessages),
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}
		// Check for /sessions or /list
		if lower == "/sessions" || lower == "/list" {
			sessions, err := listSessions()
			text := ""
			if err != nil {
				text = fmt.Sprintf("Failed to list sessions: %v", err)
			} else if len(sessions) == 0 {
				text = "No saved sessions found in /tmp/sessions"
			} else {
				text = fmt.Sprintf("Saved sessions (%d):\n", len(sessions))
				for _, name := range sessions {
					text += fmt.Sprintf("  %s\n", name)
				}
			}
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "answer",
				AssistantText: text,
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}
	}
	sess.Stats.recordUser(req.Message)
	sess.Messages = append(sess.Messages, ChatMessage{Role: "user", Content: req.Message})

	// Update autonomous prompt if in autonomous mode
	if sess.AutonomousMode {
		history := buildIterationHistory(sess)
		systemPrompt = updateAutonomousPrompt(sess.AutonomousGoal, history)
	}

	messages := make([]ChatMessage, 0, len(sess.Messages)+1)
	messages = append(messages, ChatMessage{Role: "system", Content: systemPrompt})
	messages = append(messages, sess.Messages...)

	// Force JSON mode only in autonomous mode
	forceJSON := sess.AutonomousMode
	raw, err := callOllamaWithLog("web-chat:"+sess.ID, messages, forceJSON)
	if err != nil {
		http.Error(w, fmt.Sprintf("ollama error: %v", err), http.StatusBadGateway)
		return
	}

	clean := strings.TrimSpace(raw)

	// Strip common prefixes that agent might mimic from conversation history
	if strings.HasPrefix(clean, "Command output:") {
		clean = strings.TrimPrefix(clean, "Command output:")
		clean = strings.TrimSpace(clean)
	}
	if strings.HasPrefix(clean, "✓ Result:") {
		clean = strings.TrimPrefix(clean, "✓ Result:")
		clean = strings.TrimSpace(clean)
	}

	// Strip markdown code blocks
	if strings.HasPrefix(clean, "```") {
		if i := strings.Index(clean, "\n"); i != -1 {
			clean = clean[i+1:]
		}
		clean = strings.TrimSuffix(clean, "```")
		clean = strings.TrimSpace(clean)
	}

	var tool ToolResponse
	if err := json.Unmarshal([]byte(clean), &tool); err != nil {
		// In autonomous mode, if JSON parsing fails, force agent to correct itself
		if sess.AutonomousMode {
			sess.RetryCount++
			truncLen := 100
			if len(clean) < truncLen {
				truncLen = len(clean)
			}
			log.Printf("[AUTONOMOUS] JSON parse failed (retry %d/3): %v. Response was: %s", sess.RetryCount, err, clean[:truncLen])

			// After 3 retries, stop autonomous mode
			if sess.RetryCount >= 3 {
				sess.AutonomousMode = false
				sess.RetryCount = 0
				systemPrompt = sess.OriginalPrompt
				systemPromptName = sess.OriginalPromptName
				log.Printf("[AUTONOMOUS] Max retries reached, stopping autonomous mode")

				sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: raw})
				errorText := "❌ Autonomous mode stopped: Agent failed to produce valid JSON after 3 retries. Please provide clearer instructions or try a different approach."
				sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: errorText})

				writeJSON(w, http.StatusOK, webChatResponse{
					SessionID:          sess.ID,
					Action:             "answer",
					AssistantText:      errorText,
					Raw:                raw,
					Messages:           sess.Messages,
					Model:              modelName,
					Timestamp:          time.Now().UTC(),
					Stats:              sess.Stats,
					AutonomousMode:     false,
					AutonomousContinue: false,
				})
				return
			}

			// Send error feedback to agent with their invalid response
			// Truncate the invalid response to 500 chars so agent can see what it wrote
			invalidResponsePreview := raw
			if len(invalidResponsePreview) > 500 {
				invalidResponsePreview = invalidResponsePreview[:500] + "\n... [truncated]"
			}

			errorMsg := fmt.Sprintf(`ERROR: Your response was not valid JSON (retry %d/3).

YOU WROTE THIS (which is WRONG):
---
%s
---

PROBLEMS WITH YOUR RESPONSE:
- It starts with "Command output:" - NEVER write this! This is only for SYSTEM messages showing results.
- You added markdown code blocks (backticks) - FORBIDDEN!
- Your response contains text BEFORE or AFTER the JSON object

ABSOLUTE RULE: Your ENTIRE response must be ONLY this pattern:
{"action":"run_command","command":"your_command_here"}

WRONG examples (what you did):
❌ Command output:\n{"action":...}
❌ `+"```json"+`\n{"action":...}\n`+"```"+`
❌ Here is the command: {"action":...}

CORRECT example (what you MUST do):
✅ {"action":"run_command","command":"nslookup michalswi.azurewebsites.net"}

Your response must be EXACTLY that - nothing before {, nothing after }.
Try again NOW with ONLY the JSON object.`, sess.RetryCount, invalidResponsePreview)

			sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: raw})
			sess.Messages = append(sess.Messages, ChatMessage{Role: "user", Content: errorMsg})

			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:          sess.ID,
				Action:             "error",
				AssistantText:      fmt.Sprintf("⚠️ Agent broke JSON format (retry %d/3). Forcing correction...", sess.RetryCount),
				Raw:                raw,
				Messages:           sess.Messages,
				Model:              modelName,
				Timestamp:          time.Now().UTC(),
				Stats:              sess.Stats,
				AutonomousMode:     sess.AutonomousMode,
				AutonomousContinue: true, // Force retry
			})
			return
		}

		// Non-autonomous mode: just show the raw output
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: raw})
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: raw,
			Raw:           raw,
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
		return
	}

	// Reset retry count on successful JSON parse
	if sess.AutonomousMode {
		sess.RetryCount = 0
	}

	switch tool.Action {
	case "run_command":
		sess.Stats.recordCommand(tool.Command)
		out := runCommand(tool.Command)

		// Store command output with different prefix to avoid agent mimicking it
		messageContent := "✓ Result:\n" + out
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: messageContent})

		// In autonomous mode: increment counter and generate summary
		if sess.AutonomousMode {
			sess.CommandCount++

			// Generate a quick summary for UI display (even for errors)
			summaryPrompt := `Provide a ONE sentence summary of this output. If it's an error, explain what failed. If it's data, explain what was found. Keep it under 100 characters.`

			summaryMessages := []ChatMessage{
				{Role: "system", Content: "You are a helpful assistant that summarizes command outputs concisely."},
				{Role: "assistant", Content: messageContent},
				{Role: "user", Content: summaryPrompt},
			}

			quickSummary, err := callOllamaWithLog("quick-summary:"+sess.ID, summaryMessages, false)
			if err != nil {
				// If summary generation fails, provide a basic one
				if strings.Contains(out, "Error") || strings.Contains(out, "error") {
					quickSummary = "⚠️ Command failed with error"
				} else {
					quickSummary = "✓ Command completed"
				}
			}
			quickSummary = strings.TrimSpace(quickSummary)
			if len(quickSummary) > 500 {
				quickSummary = quickSummary[:497] + "..."
			}

			// Add summary to conversation so model sees it
			summaryNote := "📝 " + quickSummary
			sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: summaryNote})

			// Every 3 commands, force detailed goal review. Otherwise, simple continue.
			var analysisPrompt string
			if sess.CommandCount%3 == 0 {
				// Detailed goal review every 3 commands
				history := buildIterationHistory(sess)

				partialSection := ""
				if sess.PartialFindings != "" {
					partialSection = fmt.Sprintf("\n✅ ALREADY FOUND (do NOT search again):\n%s\n", sess.PartialFindings)
				}

				analysisPrompt = fmt.Sprintf(`🎯 GOAL REVIEW (after %d commands):

Your goal: %s%s
Recent work:
%s

CRITICAL DECISION:
1. Review the last 6 commands and their outputs above
2. Check what you've ALREADY FOUND (marked ✅ above)
3. Do you have ALL information needed to complete the goal?
   - For "Find IP and owner": Need both IP address AND owner/organization name
   - For "Find GitHub links": Need actual repository URLs with descriptions
   - For "Make report": Need to compile all findings into structured format

4. If you completed a PART of the goal, update findings:
   {"action":"update_findings","text":"IP: X.X.X.X, Owner: CompanyName"}
   Then continue: {"action":"run_command","command":"<next task>"}

5. If you have ALL required information:
   {"action":"answer","text":"<comprehensive report with: IP, owner, all GitHub links with descriptions>"}

6. If information is INCOMPLETE:
   {"action":"run_command","command":"<specific command to get missing data>"}

Do NOT re-run commands for data you already found (✅ section). Focus ONLY on missing parts.`, sess.CommandCount, sess.AutonomousGoal, partialSection, history)
			} else {
				// Simple continuation - just keep working
				analysisPrompt = `Continue working toward the goal. What's your next command?

{"action":"run_command","command":"<next_command>"}`
			}

			sess.Messages = append(sess.Messages, ChatMessage{Role: "user", Content: analysisPrompt})

			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:          sess.ID,
				Action:             "run_command",
				AssistantText:      messageContent + "\n\n" + summaryNote,
				Command:            tool.Command,
				CommandOutput:      out,
				Messages:           sess.Messages,
				Model:              modelName,
				Timestamp:          time.Now().UTC(),
				Stats:              sess.Stats,
				AutonomousMode:     sess.AutonomousMode,
				AutonomousContinue: true, // Always continue to get analysis
				CommandCount:       sess.CommandCount,
			})
			return
		}

		// Non-autonomous mode: optional auto-analysis
		combined := messageContent
		if autoAnalyze {
			analysisPrompt := "Analyze the command output above and summarize key points. Do not request or run additional commands."
			analysisMessages := append(messages, ChatMessage{Role: "assistant", Content: messageContent})
			analysisMessages = append(analysisMessages, ChatMessage{Role: "user", Content: analysisPrompt})
			analysisRaw, err := callOllamaWithLog("web-chat-analysis:"+sess.ID, analysisMessages, false)
			if err == nil {
				analysisClean := strings.TrimSpace(analysisRaw)
				if strings.HasPrefix(analysisClean, "```") {
					if i := strings.Index(analysisClean, "\n"); i != -1 {
						analysisClean = analysisClean[i+1:]
					}
					analysisClean = strings.TrimSuffix(analysisClean, "```")
					analysisClean = strings.TrimSpace(analysisClean)
				}

				var analysis ToolResponse
				if err := json.Unmarshal([]byte(analysisClean), &analysis); err == nil && analysis.Action == "answer" {
					analysisText := analysis.Text
					sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: analysisText})
					sess.Stats.recordAssistant(analysisText)
					combined = combined + "\n\nAnalysis:\n" + analysisText
				} else {
					sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: analysisRaw})
					combined = combined + "\n\nAnalysis (raw):\n" + analysisRaw
				}
			}
		}

		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "run_command",
			AssistantText: combined,
			Command:       tool.Command,
			CommandOutput: out,
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})

	case "run_command_bg":
		sess.Stats.recordCommand(tool.Command)
		job, err := executeBackgroundCommand(sess.ID, tool.Command)
		if err != nil {
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "error",
				AssistantText: fmt.Sprintf("Failed to start background job: %v", err),
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}

		messageContent := fmt.Sprintf("Started background job: %s\nCommand: %s\nStatus: %s", job.ID, job.Command, job.Status)
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: messageContent})

		autoContinue := sess.AutonomousMode

		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:          sess.ID,
			Action:             "run_command_bg",
			AssistantText:      messageContent,
			Command:            tool.Command,
			JobID:              job.ID,
			JobStatus:          job.Status,
			Messages:           sess.Messages,
			Model:              modelName,
			Timestamp:          time.Now().UTC(),
			Stats:              sess.Stats,
			AutonomousMode:     sess.AutonomousMode,
			AutonomousContinue: autoContinue,
		})

	case "check_job":
		job, ok := jobStore.get(tool.JobID)
		if !ok {
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "error",
				AssistantText: fmt.Sprintf("Job not found: %s", tool.JobID),
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}

		// Show current job output (may be partial if job still running)
		partialOutput := job.Output
		if len(partialOutput) > 1000 {
			partialOutput = partialOutput[:1000] + "\n... (truncated, use get_job to see full output)"
		}

		messageContent := fmt.Sprintf("Job %s status: %s\nCommand: %s\nStarted: %s\nPartial output:\n%s",
			job.ID, job.Status, job.Command, job.StartTime.Format(time.RFC3339), partialOutput)

		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: messageContent})

		autoContinue := sess.AutonomousMode

		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:          sess.ID,
			Action:             "check_job",
			AssistantText:      messageContent,
			JobID:              job.ID,
			JobStatus:          job.Status,
			CommandOutput:      partialOutput,
			Messages:           sess.Messages,
			Model:              modelName,
			Timestamp:          time.Now().UTC(),
			Stats:              sess.Stats,
			AutonomousMode:     sess.AutonomousMode,
			AutonomousContinue: autoContinue,
		})

	case "get_job":
		job, ok := jobStore.get(tool.JobID)
		if !ok {
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "error",
				AssistantText: fmt.Sprintf("Job not found: %s", tool.JobID),
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}

		duration := ""
		if !job.EndTime.IsZero() {
			duration = job.EndTime.Sub(job.StartTime).String()
		}

		// Store full job output in messages (agent needs complete context)
		messageContent := fmt.Sprintf("Job %s final results:\nStatus: %s\nCommand: %s\nDuration: %s\n\nFull output:\n%s",
			job.ID, job.Status, job.Command, duration, job.Output)
		if job.Error != "" {
			messageContent += fmt.Sprintf("\n\nError:\n%s", job.Error)
		}
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: messageContent})

		autoContinue := sess.AutonomousMode

		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:          sess.ID,
			Action:             "get_job",
			AssistantText:      messageContent,
			JobID:              job.ID,
			JobStatus:          job.Status,
			CommandOutput:      job.Output,
			Messages:           sess.Messages,
			Model:              modelName,
			Timestamp:          time.Now().UTC(),
			Stats:              sess.Stats,
			AutonomousMode:     sess.AutonomousMode,
			AutonomousContinue: autoContinue,
		})

	case "cancel_job":
		job, ok := jobStore.get(tool.JobID)
		if !ok {
			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "error",
				AssistantText: fmt.Sprintf("Job not found: %s", tool.JobID),
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
			return
		}

		if job.Status == "running" && job.Cmd != nil && job.Cmd.Process != nil {
			job.Cmd.Process.Kill()
			job.Status = "cancelled"
			job.EndTime = time.Now()
			jobStore.update(job)
			content := fmt.Sprintf("Job %s cancelled", job.ID)
			sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: content})

			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:          sess.ID,
				Action:             "cancel_job",
				AssistantText:      content,
				JobID:              job.ID,
				JobStatus:          job.Status,
				Messages:           sess.Messages,
				Model:              modelName,
				Timestamp:          time.Now().UTC(),
				Stats:              sess.Stats,
				AutonomousMode:     sess.AutonomousMode,
				AutonomousContinue: sess.AutonomousMode,
			})
		} else {
			content := fmt.Sprintf("Job %s is not running (status: %s)", job.ID, job.Status)
			sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: content})

			writeJSON(w, http.StatusOK, webChatResponse{
				SessionID:     sess.ID,
				Action:        "answer",
				AssistantText: content,
				Messages:      sess.Messages,
				Model:         modelName,
				Timestamp:     time.Now().UTC(),
				Stats:         sess.Stats,
			})
		}

	case "list_jobs":
		jobs := jobStore.listBySession(sess.ID)
		content := fmt.Sprintf("Active jobs for this session (%d):\n", len(jobs))
		for _, job := range jobs {
			status := job.Status
			if job.Status == "running" {
				duration := time.Since(job.StartTime).Round(time.Second)
				status = fmt.Sprintf("running (%s)", duration)
			}
			content += fmt.Sprintf("- %s: %s [%s]\n", job.ID, job.Command, status)
		}
		if len(jobs) == 0 {
			content = "No background jobs in this session"
		}
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: content})

		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:          sess.ID,
			Action:             "list_jobs",
			AssistantText:      content,
			Jobs:               jobs,
			Messages:           sess.Messages,
			Model:              modelName,
			Timestamp:          time.Now().UTC(),
			Stats:              sess.Stats,
			AutonomousMode:     sess.AutonomousMode,
			AutonomousContinue: sess.AutonomousMode,
		})

	case "update_findings":
		// Store partial findings to avoid re-running commands
		if tool.Text != "" {
			if sess.PartialFindings == "" {
				sess.PartialFindings = tool.Text
			} else {
				sess.PartialFindings += "\n" + tool.Text
			}
			log.Printf("[AUTONOMOUS] Updated findings for session %s: %s", sess.ID, tool.Text)
		}

		// Continue autonomous mode
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:          sess.ID,
			Action:             "update_findings",
			AssistantText:      "✅ Saved: " + tool.Text,
			Messages:           sess.Messages,
			Model:              modelName,
			Timestamp:          time.Now().UTC(),
			Stats:              sess.Stats,
			AutonomousMode:     sess.AutonomousMode,
			AutonomousContinue: true,
			CommandCount:       sess.CommandCount,
		})

	case "answer":
		sess.Stats.recordAssistant(tool.Text)
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: tool.Text})

		// In autonomous mode, "answer" action means goal complete - stop autonomous mode
		if sess.AutonomousMode {
			sess.AutonomousMode = false
			systemPrompt = sess.OriginalPrompt
			systemPromptName = sess.OriginalPromptName
			sess.PartialFindings = "" // Clear findings when done
			log.Printf("[AUTONOMOUS] Goal completed with final summary, stopping autonomous mode")
		}

		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "answer",
			AssistantText: tool.Text,
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
	default:
		sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: raw})
		writeJSON(w, http.StatusOK, webChatResponse{
			SessionID:     sess.ID,
			Action:        "unknown",
			AssistantText: raw,
			Raw:           raw,
			Messages:      sess.Messages,
			Model:         modelName,
			Timestamp:     time.Now().UTC(),
			Stats:         sess.Stats,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func printStats(s *Stats) {
	fmt.Println(accent("Session stats:"))
	fmt.Printf("  User messages:      %s\n", success(fmt.Sprintf("%d", s.UserMessages)))
	fmt.Printf("  Assistant messages: %s\n", success(fmt.Sprintf("%d", s.AssistantMessages)))
	fmt.Printf("  Commands run:       %s\n", success(fmt.Sprintf("%d", s.CommandsRun)))
	fmt.Printf("  User chars total:   %s\n", success(fmt.Sprintf("%d", s.TotalUserChars)))
	fmt.Printf("  Assistant chars:    %s\n", success(fmt.Sprintf("%d", s.TotalAssistantChars)))
	if s.LastCommand != "" {
		fmt.Printf("  Last command:       %s\n", info(s.LastCommand))
	}
}

func formatStatsPlain(s Stats) string {
	var b strings.Builder
	b.WriteString("Session stats:\n")
	fmt.Fprintf(&b, "- User messages: %d\n", s.UserMessages)
	fmt.Fprintf(&b, "- Assistant messages: %d\n", s.AssistantMessages)
	fmt.Fprintf(&b, "- Commands run: %d\n", s.CommandsRun)
	fmt.Fprintf(&b, "- User chars total: %d\n", s.TotalUserChars)
	fmt.Fprintf(&b, "- Assistant chars: %d\n", s.TotalAssistantChars)
	if s.LastCommand != "" {
		fmt.Fprintf(&b, "- Last command: %s\n", s.LastCommand)
	} else {
		b.WriteString("- Last command: (none)\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func printLastQA() {
	lastUser := -1
	for i := len(sessionMessages) - 1; i >= 0; i-- {
		if sessionMessages[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser == -1 {
		fmt.Println(info("No prompt/answer captured yet."))
		return
	}

	nextAssistant := -1
	for i := lastUser + 1; i < len(sessionMessages); i++ {
		if sessionMessages[i].Role == "assistant" {
			nextAssistant = i
			break
		}
	}

	fmt.Println(accent("Last prompt + answer:"))
	fmt.Println(userLabel(), sessionMessages[lastUser].Content)
	if nextAssistant == -1 {
		fmt.Println(info("(No assistant reply yet after that prompt.)"))
		return
	}
	fmt.Println(assistantLabel(), sessionMessages[nextAssistant].Content)
}

func printSeparator() {
	fmt.Println(separatorLine())
}

func formatUptime() string {
	if startTime.IsZero() {
		return "0s"
	}
	d := time.Since(startTime).Round(time.Second)
	if d < 0 {
		return "0s"
	}
	return d.String()
}

func printCachedBlocks() {
	if len(cachedBlocks) == 0 {
		fmt.Println(info("No cached blocks."))
		return
	}
	fmt.Println(accent("Cached blocks:"))
	for i, b := range cachedBlocks {
		preview := b
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		fmt.Printf("  #%d (%d chars): %s\n", i+1, len(b), strings.ReplaceAll(preview, "\n", " "))
	}
}

func buildSession(model string, stats *Stats) Session {
	return Session{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Model:        model,
		Messages:     sessionMessages,
		Stats:        *stats,
		CachedBlocks: cachedBlocks,
	}
}

// saveSessionToFile saves the provided Session as a named JSON file in /tmp/sessions.
func saveSessionToFile(session Session, name string) (string, error) {
	sessionsDir := "/tmp/sessions"
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create sessions directory: %w", err)
	}

	var filename string
	if name == "" {
		// Auto-generate name with timestamp
		timestamp := time.Now().UTC().Format("20060102_1504")
		filename = filepath.Join(sessionsDir, fmt.Sprintf("owrap_%s.json", timestamp))
	} else {
		// Use provided name
		if !strings.HasSuffix(name, ".json") {
			name = name + ".json"
		}
		filename = filepath.Join(sessionsDir, name)
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal session: %w", err)
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write session to %s: %w", filename, err)
	}
	return filename, nil
}

// loadSessionFromFile loads a session from /tmp/sessions by name.
func loadSessionFromFile(name string) (*Session, error) {
	sessionsDir := "/tmp/sessions"
	if !strings.HasSuffix(name, ".json") {
		name = name + ".json"
	}
	filename := filepath.Join(sessionsDir, name)

	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read session from %s: %w", filename, err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &session, nil
}

// listSessions returns a list of saved session files in /tmp/sessions.
func listSessions() ([]string, error) {
	sessionsDir := "/tmp/sessions"
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read sessions directory: %w", err)
	}

	var sessions []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			sessions = append(sessions, entry.Name())
		}
	}
	sort.Strings(sessions)
	return sessions, nil
}

func saveWebSession(sess *webSession) (string, error) {
	timestamp := time.Now().UTC().Format("20060102_1504")
	filename := filepath.Join("/tmp", fmt.Sprintf("owrap_web_%s.json", timestamp))
	payload := Session{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Model:        modelName,
		Messages:     sess.Messages,
		Stats:        sess.Stats,
		CachedBlocks: nil,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal session: %w", err)
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write session to %s: %w", filename, err)
	}
	return filename, nil
}

// readUserInput captures a full user paste/entry, including multiple lines already
// present in the input buffer, so multi-line paste is treated as one message.
func readUserInput(r *bufio.Reader) (string, error) {
	var b bytes.Buffer
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	b.WriteString(line)
	for r.Buffered() > 0 {
		more, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		b.WriteString(more)
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return b.String(), nil
}

// readUserInputWithDelim reads until a line trimmed of CR/LF equals the delimiter.
func readUserInputWithDelim(r *bufio.Reader, delim string) (string, error) {
	var b bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == delim {
			break
		}
		b.WriteString(line)
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return b.String(), nil
}

func main() {
	webUI := flag.Bool("web", false, "serve the web UI instead of the CLI")
	flag.Parse()

	if *webUI {
		bind := webBindDefault
		fmt.Printf("Starting web UI on %s (model=%s, prompt=%s, chars=%d)\n", bind, modelName, systemPromptName, len(systemPrompt))
		if err := startWebUI(bind); err != nil {
			fmt.Println(warn("Web UI error:"), err)
			os.Exit(1)
		}
		return
	}

	ShowBanner()

	reader := bufio.NewReader(os.Stdin)
	stats := &Stats{}
	startTime = time.Now()
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
	}
	sessionMessages = append(sessionMessages, messages...)

	fmt.Println(info("Type '/q' to quit."))
	fmt.Println(info("Type '/h' for help/shortcuts."))

	for {
		printSeparator()
		fmt.Print(userLabel())
		userInput, err := readUserInput(reader)
		if err != nil {
			fmt.Println(warn("Read error:"), err)
			return
		}
		userInput = strings.TrimRight(userInput, "\r\n")
		if strings.TrimSpace(userInput) == "" {
			continue
		}

		if strings.HasPrefix(userInput, "/") {
			lower := strings.ToLower(strings.TrimSpace(userInput))
			if strings.HasPrefix(lower, "/p") || strings.HasPrefix(lower, "/paste") {
				delim := "EOF"
				parts := strings.Fields(userInput)
				if len(parts) > 1 {
					delim = parts[1]
				}
				fmt.Println(info(fmt.Sprintf("Paste mode: end with a line containing only '%s'.", delim)))
				block, err := readUserInputWithDelim(reader, delim)
				if err != nil {
					fmt.Println(warn("Read error (paste mode):"), err)
					continue
				}
				block = strings.TrimRight(block, "\r\n")
				if strings.TrimSpace(block) == "" {
					continue
				}
				fmt.Print(info("Add a question/instruction (optional, Enter to skip): "))
				follow, err := reader.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					fmt.Println(warn("Read error (question):"), err)
					continue
				}
				follow = strings.TrimRight(follow, "\r\n")
				if strings.TrimSpace(follow) == "" {
					cachedBlocks = append(cachedBlocks, block)
					fmt.Println(info("Pasted block cached (not sent)."))
					continue
				}
				userInput = block + "\n\n" + follow
			} else if strings.HasPrefix(lower, "/cache") {
				printCachedBlocks()
				continue
			} else if strings.HasPrefix(lower, "/use") {
				parts := strings.Fields(userInput)
				if len(parts) < 2 {
					fmt.Println(warn("Usage: /use <index> [question]"))
					printCachedBlocks()
					continue
				}
				idx, err := strconv.Atoi(parts[1])
				if err != nil || idx < 1 || idx > len(cachedBlocks) {
					fmt.Println(warn("Invalid index for /use"))
					printCachedBlocks()
					continue
				}
				block := cachedBlocks[idx-1]
				question := strings.Join(parts[2:], " ")
				if strings.TrimSpace(question) != "" {
					userInput = block + "\n\n" + question
				} else {
					userInput = block
				}
			} else if strings.HasPrefix(lower, "/execfile") || strings.HasPrefix(lower, "/xf") {
				parts := strings.Fields(userInput)
				if len(parts) < 2 {
					fmt.Println(warn("Usage: /execfile <path>"))
					continue
				}
				path := parts[1]
				output := execCommandsFromFile(path, stats)
				fmt.Println(output)
				if strings.TrimSpace(output) != "" {
					sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: output})
				}
				continue
			} else {
				beforeLoadLen := len(sessionMessages)
				handled := handleSlashCommand(userInput, stats)
				if handled {
					// Check if session was loaded (sessionMessages changed significantly)
					if len(sessionMessages) != beforeLoadLen && len(sessionMessages) > 0 {
						// Rebuild messages array from loaded session
						messages = []ChatMessage{{Role: "system", Content: systemPrompt}}
						messages = append(messages, sessionMessages...)
						fmt.Println(info("Conversation context restored - you can continue from here."))
					}
					continue
				}
				// Fall through if unknown slash command; let model handle text
			}
		}
		if strings.EqualFold(userInput, "exit") || strings.EqualFold(userInput, "quit") {
			return
		}

		messages = append(messages, ChatMessage{
			Role:    "user",
			Content: userInput,
		})
		sessionMessages = append(sessionMessages, ChatMessage{Role: "user", Content: userInput})
		stats.recordUser(userInput)

		raw, err := callOllama(messages, false)
		if err != nil {
			fmt.Println(warn("Ollama error:"), err)
			continue
		}

		clean := strings.TrimSpace(raw)
		if strings.HasPrefix(clean, "```") {
			if i := strings.Index(clean, "\n"); i != -1 {
				clean = clean[i+1:]
			}
			clean = strings.TrimSuffix(clean, "```")
			clean = strings.TrimSpace(clean)
		}

		var tool ToolResponse
		if err := json.Unmarshal([]byte(clean), &tool); err != nil {
			// Model didn’t stick to JSON; just print raw
			fmt.Println(assistantLabel(), raw)
			messages = append(messages, ChatMessage{Role: "assistant", Content: raw})
			sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: raw})
			continue
		}

		switch tool.Action {
		case "run_command":
			stats.recordCommand(tool.Command)
			fmt.Printf("%s %s\n", runningLabel(), tool.Command)
			out := runCommand(tool.Command)
			fmt.Println(outputLabel())
			fmt.Println(out)

			// Feed result back so model can explain/continue
			messages = append(messages, ChatMessage{
				Role:    "assistant",
				Content: "Command output:\n" + out,
			})
			sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: "Command output:\n" + out})

			if autoAnalyze {
				// Ask for analysis without running more commands
				analysisPrompt := "Analyze the command output above and summarize key points. Do not request or run additional commands."
				analysisMessages := append(messages, ChatMessage{Role: "user", Content: analysisPrompt})
				analysisRaw, err := callOllama(analysisMessages, false)
				if err != nil {
					fmt.Println(warn("Ollama error during analysis:"), err)
					continue
				}

				analysisClean := strings.TrimSpace(analysisRaw)
				if strings.HasPrefix(analysisClean, "```") {
					if i := strings.Index(analysisClean, "\n"); i != -1 {
						analysisClean = analysisClean[i+1:]
					}
					analysisClean = strings.TrimSuffix(analysisClean, "```")
					analysisClean = strings.TrimSpace(analysisClean)
				}

				var analysis ToolResponse
				if err := json.Unmarshal([]byte(analysisClean), &analysis); err != nil || analysis.Action != "answer" {
					// Fall back to printing raw analysis
					fmt.Println(analysisLabel(), analysisRaw)
					messages = append(messages, ChatMessage{Role: "assistant", Content: analysisRaw})
					sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: analysisRaw})
				} else {
					fmt.Println(analysisLabel(), analysis.Text)
					stats.recordAssistant(analysis.Text)
					messages = append(messages, ChatMessage{Role: "assistant", Content: analysis.Text})
					sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: analysis.Text})
				}
			}

		case "answer":
			fmt.Println(assistantLabel(), tool.Text)
			stats.recordAssistant(tool.Text)
			messages = append(messages, ChatMessage{
				Role:    "assistant",
				Content: tool.Text,
			})
			sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: tool.Text})

		default:
			fmt.Println("Assistant (unknown action):", raw)
			messages = append(messages, ChatMessage{Role: "assistant", Content: raw})
			sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: raw})
		}
	}
}

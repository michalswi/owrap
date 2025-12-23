package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/michalswi/color"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

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
}

type ChatResponse struct {
	Message ChatMessage `json:"message"`
}

type ToolResponse struct {
	Action  string `json:"action"`            // "run_command" or "answer"
	Command string `json:"command,omitempty"` // when action == run_command
	Text    string `json:"text,omitempty"`    // when action == answer
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

func callOllama(messages []ChatMessage) (string, error) {
	reqBody := ChatRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   false,
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

func runCommand(cmdStr string) string {
	sanitized := sanitizeCommand(cmdStr)
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

// saveSessionToFile saves the provided Session as a timestamped JSON file in /tmp.
func saveSessionToFile(session Session) error {
	timestamp := time.Now().UTC().Format("20060102_1504")
	filename := filepath.Join("/tmp", fmt.Sprintf("owrap_%s.json", timestamp))
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("failed to write session to %s: %w", filename, err)
	}
	return nil
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
				handled := handleSlashCommand(userInput, stats)
				if handled {
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

		raw, err := callOllama(messages)
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
				analysisRaw, err := callOllama(analysisMessages)
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
					continue
				}

				fmt.Println(analysisLabel(), analysis.Text)
				stats.recordAssistant(analysis.Text)
				messages = append(messages, ChatMessage{Role: "assistant", Content: analysis.Text})
				sessionMessages = append(sessionMessages, ChatMessage{Role: "assistant", Content: analysis.Text})
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
		}
	}
}

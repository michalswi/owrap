package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// AutoStartRequest represents the JSON body for starting autonomous mode
type AutoStartRequest struct {
	SessionID string `json:"sessionId"`
	Goal      string `json:"goal"`
}

// handleAutonomousStart activates autonomous mode for a session
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

// handleAutonomousStop deactivates autonomous mode for a session
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

// buildIterationHistory generates a summary of recent conversation for autonomous mode
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

// getRecentCommands returns the last 3 executed commands to prevent duplicates
func getRecentCommands(sess *webSession) []string {
	return sess.LastCommands
}

// updateAutonomousPrompt updates the autonomous mode prompt with current goal and history
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

// handleAutonomousCommand processes commands in autonomous mode with special logic
func handleAutonomousCommand(sess *webSession, tool ToolResponse, messageContent string, out string, messages []ChatMessage) webChatResponse {
	// Increment command count
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
		// Simple continuation - include recent commands to prevent duplicates
		recentCmds := getRecentCommands(sess)
		cmdHistory := ""
		if len(recentCmds) > 0 {
			cmdHistory = "\n\n⚠️ Commands you JUST executed:\n"
			for _, cmd := range recentCmds {
				cmdHistory += "- " + cmd + "\n"
			}
			cmdHistory += "\nDo NOT repeat these commands. Move to the NEXT step of the goal.\n"
		}

		analysisPrompt = fmt.Sprintf(`Continue working toward the goal.%s
What's your NEXT command (different from above)?

{"action":"run_command","command":"<next_command>"}`, cmdHistory)
	}

	sess.Messages = append(sess.Messages, ChatMessage{Role: "user", Content: analysisPrompt})

	return webChatResponse{
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
	}
}

// handleAutonomousJSONError handles JSON parsing errors in autonomous mode
func handleAutonomousJSONError(sess *webSession, raw string, clean string, err error) webChatResponse {
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

		return webChatResponse{
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
		}
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

	return webChatResponse{
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
	}
}

// handleUpdateFindings processes the update_findings action in autonomous mode
func handleUpdateFindings(sess *webSession, tool ToolResponse) webChatResponse {
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
	return webChatResponse{
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
	}
}

// handleAutonomousAnswer processes the answer action and stops autonomous mode
func handleAutonomousAnswer(sess *webSession, tool ToolResponse) webChatResponse {
	sess.Stats.recordAssistant(tool.Text)
	sess.Messages = append(sess.Messages, ChatMessage{Role: "assistant", Content: tool.Text})

	// In autonomous mode, "answer" action means goal complete - stop autonomous mode
	wasAutonomous := sess.AutonomousMode
	if sess.AutonomousMode {
		sess.AutonomousMode = false
		systemPrompt = sess.OriginalPrompt
		systemPromptName = sess.OriginalPromptName
		sess.PartialFindings = "" // Clear findings when done
		sess.LastCommands = nil   // Clear command history
		log.Printf("[AUTONOMOUS] Goal completed with final summary, stopping autonomous mode")
	}

	response := webChatResponse{
		SessionID:          sess.ID,
		Action:             "answer",
		AssistantText:      tool.Text,
		Messages:           sess.Messages,
		Model:              modelName,
		Timestamp:          time.Now().UTC(),
		Stats:              sess.Stats,
		AutonomousMode:     false, // Explicitly set to false
		AutonomousContinue: false, // Stop the loop
	}

	// If it was autonomous, log auto-stop
	if wasAutonomous {
		log.Printf("[AUTONOMOUS] Triggering auto-stop for session %s", sess.ID)
	}

	return response
}

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// handleSlashCommand processes local slash shortcuts like /help.
func handleSlashCommand(input string, stats *Stats) bool {
	parts := strings.Fields(strings.TrimSpace(input))
	if len(parts) == 0 {
		return false
	}

	command := strings.ToLower(parts[0])

	switch command {
	case "/h":
		printHelp()
		return true
	case "/q":
		fmt.Println(success("Bye!"))
		os.Exit(0)
		return true
	case "/dir":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Println(warn("cwd unavailable:"), err)
		} else {
			fmt.Println(info(cwd))
		}
		return true
	case "/m":
		fmt.Println(info(fmt.Sprintf("Model: %s", modelName)))
		return true
	case "/up":
		fmt.Println(info(fmt.Sprintf("Uptime: %s", formatUptime())))
		return true
	case "/s", "/stats":
		printStats(stats)
		return true
	case "/last":
		printLastQA()
		return true
	case "/sysprompt":
		printSystemPrompt()
		return true
	case "/save":
		fmt.Println(info("Saving session..."))
		session := buildSession(modelName, stats)
		var sessionName string
		if len(parts) > 1 {
			sessionName = parts[1]
		}
		path, err := saveSessionToFile(session, sessionName)
		if err != nil {
			fmt.Println(warn("Save failed:"), err)
		} else {
			fmt.Println(success(fmt.Sprintf("Session saved to: %s", path)))
		}
		return true
	case "/load":
		if len(parts) < 2 {
			fmt.Println(warn("Usage: /load <session-name>"))
			return true
		}
		sessionName := parts[1]
		if err := loadSession(sessionName, stats); err != nil {
			fmt.Println(warn("Load failed:"), err)
		} else {
			fmt.Println(success(fmt.Sprintf("Session '%s' loaded", sessionName)))
			fmt.Println(info("Note: Restart conversation with loaded context by rebuilding messages array"))
		}
		return true
	case "/sessions", "/list":
		sessions, err := listSessions()
		if err != nil {
			fmt.Println(warn("Failed to list sessions:"), err)
			return true
		}
		if len(sessions) == 0 {
			fmt.Println(info("No saved sessions found in ~/.owrap/sessions"))
		} else {
			fmt.Println(accent(fmt.Sprintf("Saved sessions (%d):", len(sessions))))
			for _, name := range sessions {
				fmt.Printf("  %s\n", info(name))
			}
		}
		return true
	case "/auto-on":
		autoAnalyze = true
		fmt.Println(success("Auto-analysis enabled (after commands)."))
		return true
	case "/auto-off":
		autoAnalyze = false
		fmt.Println(info("Auto-analysis disabled."))
		return true
	case "/editsysprompt":
		handleEditSystemPrompt()
		return true
	case "/myprompts":
		printMyPrompts()
		return true
	default:
		fmt.Printf("%s %s\n", warn("Unknown command:"), input)
		return true
	}
}

func printHelp() {
	fmt.Println(accent("Available commands:"))
	fmt.Println("  /h             Show this help")
	fmt.Println("  /q             Exit the program")
	fmt.Println("  /dir           Show current working directory")
	fmt.Println("  /m             Show Ollama LLM model in use")
	fmt.Println("  /up            Show app uptime")
	fmt.Println("  /s, /stats     Show counts, chars, context estimate, timing, and last command")
	fmt.Println("  /last          Show last prompt + model answer")
	fmt.Println("  /myprompts     Show all your prompts from current session")
	fmt.Println("  /sysprompt     Show current system prompt")
	fmt.Println("  /editsysprompt Edit system prompt (select from files or write custom)")
	fmt.Println("  /save [NAME]   Save current session to ~/.owrap/sessions (auto-named if NAME omitted)")
	fmt.Println("  /load NAME     Load a saved session by name")
	fmt.Println("  /sessions      List all saved sessions in ~/.owrap/sessions")
	fmt.Println("  /p [DELIM]     Paste multi-line input; finish with a line containing only DELIM (default EOF)")
	fmt.Println("  /cache         List cached (not sent) blocks")
	fmt.Println("  /use N         Send cached block #N (1-based) with optional question")
	fmt.Println("  /auto-on       LLM auto-analyzes command output after execution")
	fmt.Println("  /auto-off      [default] No LLM auto-analysis after command execution")
	fmt.Println("  /execfile P    Execute each non-empty line in file P (no analysis)")
	fmt.Println(accent("Model-run allowed commands:"))
	fmt.Printf("  [%s]\n", strings.Join(allowedCommandsList(), ", "))
}

func printSystemPrompt() {
	fmt.Println(accent("Current system prompt:"))
	fmt.Println(systemPrompt)
}

// loadSession loads a saved session, displays it, and restores it into active conversation
func loadSession(name string, stats *Stats) error {
	session, err := loadSessionFromFile(name)
	if err != nil {
		return err
	}

	// Display session header
	fmt.Println(separatorLine())
	fmt.Println(accent(fmt.Sprintf("Loaded session: %s", name)))
	fmt.Println(info(fmt.Sprintf("Model: %s | Saved: %s", session.Model, session.Timestamp)))
	fmt.Println(separatorLine())

	// Display all messages from the session
	for _, msg := range session.Messages {
		if msg.Role == "system" {
			// Skip system messages in display
			continue
		}

		if msg.Role == "user" {
			fmt.Println(separatorLine())
			fmt.Printf("%s%s\n", userLabel(), msg.Content)
		} else if msg.Role == "assistant" {
			fmt.Printf("%s %s\n", assistantLabel(), msg.Content)
		}
	}

	fmt.Println(separatorLine())
	fmt.Println(success("Session loaded and conversation restored"))
	fmt.Println(info(fmt.Sprintf("Messages: %d user, %d assistant", session.Stats.UserMessages, session.Stats.AssistantMessages)))
	fmt.Println(info("You can now continue the conversation from where it left off."))

	// Restore stats
	*stats = session.Stats
	stats.updateContext(systemPrompt, session.Messages)

	// Restore session messages for saving
	sessionMessages = session.Messages

	// Restore cached blocks if any
	if len(session.CachedBlocks) > 0 {
		cachedBlocks = session.CachedBlocks
		fmt.Println(info(fmt.Sprintf("Restored %d cached blocks", len(cachedBlocks))))
	}

	return nil
}

func handleEditSystemPrompt() {
	fmt.Println(accent("=== Edit System Prompt ==="))
	fmt.Println(info(fmt.Sprintf("Current prompt: %s", systemPromptName)))
	fmt.Println()

	// List available prompt files
	promptsDir := "prompts"
	files, err := os.ReadDir(promptsDir)
	var availablePrompts []string
	if err == nil {
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".txt") {
				availablePrompts = append(availablePrompts, file.Name())
			}
		}
	}

	if len(availablePrompts) > 0 {
		fmt.Println(accent("Available prompts:"))
		for i, p := range availablePrompts {
			fmt.Printf("  %d. %s\n", i+1, info(p))
		}
		fmt.Println()
	}

	fmt.Println(accent("Options:"))
	fmt.Println("  1-" + fmt.Sprint(len(availablePrompts)) + ": Select a prompt file by number")
	fmt.Println("  custom: Write your own custom prompt")
	fmt.Println("  cancel: Cancel and keep current prompt")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	fmt.Print(accent("Choose option: "))
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	if strings.ToLower(choice) == "cancel" {
		fmt.Println(info("Cancelled - keeping current prompt"))
		return
	}

	if strings.ToLower(choice) == "custom" {
		fmt.Println(info("Enter your custom system prompt (type EOF on a new line to finish):"))
		customPrompt, err := readUserInputWithDelim(reader, "EOF")
		if err != nil {
			fmt.Println(warn("Error reading custom prompt:"), err)
			return
		}
		customPrompt = strings.TrimSpace(customPrompt)
		if customPrompt == "" {
			fmt.Println(warn("Custom prompt cannot be empty"))
			return
		}
		systemPrompt = customPrompt
		systemPromptName = "custom"
		fmt.Println(success(fmt.Sprintf("✓ System prompt updated to: %s", systemPromptName)))
		return
	}

	// Try to parse as number
	if idx, err := strconv.Atoi(choice); err == nil && idx >= 1 && idx <= len(availablePrompts) {
		selectedFile := availablePrompts[idx-1]
		filePath := filepath.Join(promptsDir, selectedFile)
		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Println(warn("Failed to read prompt file:"), err)
			return
		}
		systemPrompt = string(data)
		systemPromptName = selectedFile
		fmt.Println(success(fmt.Sprintf("✓ System prompt updated to: %s", systemPromptName)))
		return
	}

	fmt.Println(warn("Invalid choice"))
}

func printMyPrompts() {
	// Extract all user messages from sessionMessages
	userPrompts := []string{}
	for _, msg := range sessionMessages {
		if msg.Role == "user" {
			userPrompts = append(userPrompts, msg.Content)
		}
	}

	if len(userPrompts) == 0 {
		fmt.Println(info("No prompts in current session yet."))
		return
	}

	fmt.Println(separatorLine())
	fmt.Println(accent(fmt.Sprintf("My Prompts (Total: %d)", len(userPrompts))))
	fmt.Println(separatorLine())

	for i, prompt := range userPrompts {
		fmt.Printf("\n%s\n", info(fmt.Sprintf("#%d:", i+1)))
		fmt.Println(prompt)
		if i < len(userPrompts)-1 {
			fmt.Println("")
		}
	}

	fmt.Println("")
	fmt.Println(separatorLine())
	fmt.Println(info("💡 These prompts are preserved when you save/load sessions."))
}

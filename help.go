package main

import (
	"fmt"
	"os"
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
			fmt.Println(info("No saved sessions found in /tmp/sessions"))
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
	default:
		fmt.Printf("%s %s\n", warn("Unknown command:"), input)
		return true
	}
}

func printHelp() {
	fmt.Println(accent("Available commands:"))
	fmt.Println("  /h            Show this help")
	fmt.Println("  /q            Exit the program")
	fmt.Println("  /dir          Show current working directory")
	fmt.Println("  /m            Show Ollama LLM model in use")
	fmt.Println("  /up           Show app uptime")
	fmt.Println("  /s, /stats    Show session stats (counts, chars, last command)")
	fmt.Println("  /last         Show last prompt + model answer")
	fmt.Println("  /sysprompt    Show current system prompt")
	fmt.Println("  /save [NAME]  Save current session to /tmp/sessions (auto-named if NAME omitted)")
	fmt.Println("  /load NAME    Load a saved session by name")
	fmt.Println("  /sessions     List all saved sessions in /tmp/sessions")
	fmt.Println("  /p [DELIM]    Paste multi-line input; finish with a line containing only DELIM (default EOF)")
	fmt.Println("  /cache        List cached (not sent) blocks")
	fmt.Println("  /use N        Send cached block #N (1-based) with optional question")
	fmt.Println("  /auto-on      Enable automatic analysis after commands")
	fmt.Println("  /auto-off     [default] Disable automatic analysis after commands")
	fmt.Println("  /execfile P   Execute each non-empty line in file P (no analysis)")
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

	// Restore session messages for saving
	sessionMessages = session.Messages

	// Restore cached blocks if any
	if len(session.CachedBlocks) > 0 {
		cachedBlocks = session.CachedBlocks
		fmt.Println(info(fmt.Sprintf("Restored %d cached blocks", len(cachedBlocks))))
	}

	return nil
}

package main

import (
	"fmt"
	"os"
	"strings"
)

// handleSlashCommand processes local slash shortcuts like /help.
func handleSlashCommand(input string, stats *Stats) bool {
	switch strings.ToLower(strings.TrimSpace(input)) {
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
	case "/save":
		fmt.Println(info("Saving session..."))
		session := buildSession(modelName, stats)
		if err := saveSessionToFile(session); err != nil {
			fmt.Println(warn("Save failed:"), err)
		} else {
			fmt.Println(success("Session saved."))
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
	fmt.Println("  /save         Save current session to /tmp as JSON (includes cached blocks)")
	fmt.Println("  /p [DELIM]    Paste multi-line input; finish with a line containing only DELIM (default EOF)")
	fmt.Println("  /cache        List cached (not sent) blocks")
	fmt.Println("  /use N        Send cached block #N (1-based) with optional question")
	fmt.Println("  /auto-on      Enable automatic analysis after commands")
	fmt.Println("  /auto-off     [default] Disable automatic analysis after commands")
	fmt.Println("  /execfile P   Execute each non-empty line in file P (no analysis)")
	fmt.Println(accent("Model-run allowed commands:"))
	fmt.Printf("  [%s]\n", strings.Join(allowedCommandsList(), ", "))
}

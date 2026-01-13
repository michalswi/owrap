# Autonomous Loop Mode - Implementation Guide

## Overview

Autonomous mode enables owrap to work for extended periods without human intervention. You define a goal and success criteria, then the agent iterates automatically until it succeeds or hits the iteration limit.

## How It Works

### Simple Flow

```
1. You define goal + success criteria via /autostart button
2. System prompt automatically switches to autonomous_agent.txt
3. Agent iterates, learning from failures
4. When done (or you stop it), /autostop restores default prompt
```

### Key Concept

**Traditional AI workflow:** Human → AI suggests → Human approves → Execute → Repeat

**Autonomous workflow:** Human defines goal once → AI iterates automatically → Delivers result

## Using Autonomous Mode

### Starting Autonomous Mode

1. Click **🤖 /autostart** button in WebUI
2. Fill in the form:
   - **Goal**: What you want to achieve (e.g., "find all devices in local network 192.168.1.0/24")
   - **Success Criteria**: How to know when done (e.g., "Found at least 3 devices with IP addresses and MAC addresses")
   - **Max Iterations**: Safety limit (1-50, recommended: 10)
3. Click "🚀 Start Autonomous Loop"

### What Happens

- System prompt switches to `autonomous_agent.txt`
- Agent receives goal + success criteria
- Iteration counter starts at 0
- Agent begins autonomous execution

### During Autonomous Mode

- Agent will automatically run commands
- Each iteration learns from previous failures
- You can still send messages to guide the agent
- Iteration history is tracked

### Stopping Autonomous Mode

- Click **⏹️ /autostop** button
- System prompt restores to default (or your previous custom prompt)
- Autonomous mode deactivated

## Example Scenarios

### 1. Network Discovery

**Goal:** Find all devices in local network 192.168.1.0/24

**Success:** Found at least 5 devices with IP addresses and MAC addresses

**Agent might do:**
- Iteration 1: Try `nmap -sn 192.168.1.0/24` → Permission denied
- Iteration 2: Try `arp -a` → Only shows 2 cached entries
- Iteration 3: Try ping sweep + ARP:
  ```bash
  for i in {1..254}; do ping -c 1 -W 1 192.168.1.$i >/dev/null 2>&1 & done
  sleep 5
  arp -a | grep 192.168.1
  ```
- Result: ✓ Found 8 devices → Success!

### 2. Code Generation

**Goal:** Write a simple Python HTTP server that serves files from current directory

**Success:** Generated working Python code that can be executed

**Agent might do:**
- Iteration 1: Generate basic HTTP server code
- Iteration 2: Test it mentally, add error handling
- Iteration 3: Add directory listing feature
- Result: ✓ Complete working code → Success!

### 3. Log Analysis

**Goal:** Analyze web server logs and find all 404 errors

**Success:** List of all unique 404 URLs with count

**Agent might do:**
- Iteration 1: Try to find log file location
- Iteration 2: Use grep to find 404 entries
- Iteration 3: Extract URLs and count occurrences
- Result: ✓ Summary report generated → Success!

## Autonomous System Prompt

The `prompts/autonomous_agent.txt` prompt is automatically loaded when you start autonomous mode. It contains:

- Goal and success criteria (filled from your input)
- Current iteration number
- History of previous attempts
- Instructions to learn from failures
- Rules to avoid repeating failed commands

## Technical Details

### Backend (main.go)

- `webSession` struct extended with autonomous fields:
  - `AutonomousMode`: bool flag
  - `AutonomousGoal`: goal description
  - `AutonomousSuccess`: success criteria
  - `AutonomousMaxIter`: max iterations
  - `AutonomousIter`: current iteration
  - `OriginalPrompt`: saved original prompt for restoration

- New endpoints:
  - `/api/autonomous/start` - Activates autonomous mode
  - `/api/autonomous/stop` - Deactivates and restores prompt

### Frontend (index.html)

- New buttons: `/autostart` and `/autostop`
- Modal dialog for goal definition
- Automatic prompt switching
- Visual feedback when autonomous mode is active

### Prompt Template

Located at `prompts/autonomous_agent.txt`, uses placeholders:
- `{GOAL_DESCRIPTION}` - Replaced with user's goal
- `{SUCCESS_CRITERIA}` - Replaced with success criteria
- `{ITERATION}` - Current iteration number
- `{MAX_ITERATIONS}` - Maximum allowed iterations
- `{ITERATION_HISTORY}` - What happened in previous attempts

## Safety Features

1. **Iteration Limit**: Hard cap at 50 iterations (configurable 1-50)
2. **Command Allowlist**: Only pre-approved commands can run
3. **Prompt Restoration**: Original prompt always saved and restored
4. **Manual Override**: You can stop anytime with /autostop
5. **No Automatic Looping**: Agent doesn't loop infinitely, requires user messages to continue

## Best Practices

### Good Goals

✅ **Specific**: "Find all devices in 192.168.1.0/24"  
✅ **Measurable**: "Generate Python code that runs without errors"  
✅ **Achievable**: "List all .txt files in current directory"

### Bad Goals

❌ **Too vague**: "Do something useful"  
❌ **Impossible**: "Hack into NASA"  
❌ **Unmeasurable**: "Make things better"

### Good Success Criteria

✅ **Concrete**: "Found at least 3 devices"  
✅ **Verifiable**: "Code executes without syntax errors"  
✅ **Clear**: "Produced list with 10+ items"

### Bad Success Criteria

❌ **Vague**: "Looks good"  
❌ **Subjective**: "Best solution"  
❌ **Unverifiable**: "Probably works"

## Limitations

- Agent cannot actually verify success automatically (yet)
- No automatic goal completion detection
- Iteration history not yet implemented (future enhancement)
- Single session only (not multi-session aware)
- No pause/resume functionality

## Future Enhancements

Ideas for extending autonomous mode:

1. **Automatic Success Detection**: Parse outputs and check against criteria
2. **Iteration History Tracking**: Feed previous attempts back to LLM
3. **Learning Database**: Store successful strategies for similar goals
4. **Multi-Agent Coordination**: Multiple agents working on sub-goals
5. **Approval Checkpoints**: Optional human approval after N iterations
6. **Goal Templates**: Pre-defined goals for common tasks
7. **Result Export**: Save autonomous session results separately
8. **Progress Visualization**: Graph showing iteration progress

## Troubleshooting

**Q: Autonomous mode not starting?**  
A: Check that `prompts/autonomous_agent.txt` exists and is readable.

**Q: Prompt not restoring after /autostop?**  
A: Original prompt is saved in session. If it fails, default prompt is used as fallback.

**Q: Agent keeps repeating same failed command?**  
A: The autonomous prompt instructs against this. Try being more specific in success criteria.

**Q: How do I know what iteration I'm on?**  
A: Currently not displayed in UI (future enhancement). Check backend logs or session state.

## Example Session

```
[User clicks /autostart]

Goal: Find all devices in local network 192.168.1.0/24
Success: Found at least 3 devices with IP and MAC addresses
Max Iterations: 10

[Autonomous mode activated]

You: start scanning
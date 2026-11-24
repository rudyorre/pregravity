package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"pregravity/internal/assistant"
	"pregravity/internal/config"
	"pregravity/internal/cron"
	"pregravity/internal/gemini"
	"pregravity/internal/gtasks"
	"pregravity/internal/mcp"
	"pregravity/internal/store"
	"pregravity/internal/transport"
	"pregravity/skills"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	var configPath string
	args := os.Args[1:]
	// Support optional global -c / --config flag before subcommand.
	for len(args) >= 2 && (args[0] == "-c" || args[0] == "--config") {
		configPath = args[1]
		args = args[2:]
	}
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	cmd := args[0]
	subArgs := args[1:]

	var err error
	switch cmd {
	case "init":
		err = cmdInit(configPath)
	case "daemon":
		err = cmdDaemon(configPath)
	case "chat":
		err = cmdChat(configPath, subArgs)
	case "tasks":
		err = cmdTasks(configPath, subArgs)
	case "handoffs":
		err = cmdHandoffs(configPath, subArgs)
	case "quota", "usage":
		err = cmdQuota(configPath, subArgs)
	case "mcp":
		err = cmdMCP(configPath, subArgs)
	case "gtasks":
		err = cmdGTasks(configPath, subArgs)
	case "install-skill":
		err = cmdInstallSkill()
	case "doctor":
		err = cmdDoctor(configPath)
	case "version", "-v", "--version":
		fmt.Printf("pregravity %s\n", version)
		return
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`pregravity — minimal, hardened personal accountability & Antigravity handoff assistant

Usage:
  pregravity [--config PATH] <command> [args]

Commands:
  init            Bootstrap config, 0600 env template, data dir, and /pickup skill
  daemon          Run the scheduled check-ins + enabled transports (Telegram / HTTP / ntfy)
  chat            Interactive terminal chat (or one-shot with -p "...")
  tasks           Manage local tasks directly (list | add | done | snooze)
  handoffs        Manage Antigravity handoffs directly (list | create | show | done)
  quota           Show Gemini API usage/freebie quota & Antigravity sub quota (or set-limit <N>)
  gtasks          Read-only Google Tasks setup & CLI (auth | list | enable)
  mcp             Run built-in read-only MCP stdio servers (e.g. 'pregravity mcp gtasks')
  install-skill   Install the Antigravity /pickup skill to ~/.gemini/config/skills/pickup/
  doctor          Verify config, secret file permissions, data dir, and agy remote-control
  version         Print version`)
}

func cmdInit(configPath string) error {
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	cfg := config.Default()
	if inst := detectAgyInstanceName(); inst != "" {
		cfg.Antigravity.InstanceName = inst
	}

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if err := config.Save(configPath, cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Printf("Created config: %s\n", configPath)
	} else {
		fmt.Printf("Config already exists (kept): %s\n", configPath)
		loaded, err := config.Load(configPath)
		if err == nil {
			cfg = loaded
		}
	}

	envPath := filepath.Join(filepath.Dir(configPath), "env")
	if _, err := os.Stat(envPath); errors.Is(err, os.ErrNotExist) {
		template := strings.Join([]string{
			"# Pregravity secrets (chmod 0600 — never committed to git)",
			"# Required: Gemini API key from https://aistudio.google.com/apikey",
			"GEMINI_API_KEY=",
			"",
			"# Optional: Telegram bot token (from @BotFather) when telegram.enabled=true",
			"# PREGRAVITY_TELEGRAM_BOT_TOKEN=",
			"",
			"# Optional: ntfy bearer token if using a private topic",
			"# PREGRAVITY_NTFY_TOKEN=",
			"",
			"# Optional: bearer token if http.enabled=true (loopback 127.0.0.1 only)",
			"# PREGRAVITY_HTTP_TOKEN=",
			"",
		}, "\n")
		if err := os.WriteFile(envPath, []byte(template), 0o600); err != nil {
			return fmt.Errorf("create env file: %w", err)
		}
		fmt.Printf("Created env template (0600): %s\n", envPath)
	} else {
		fmt.Printf("Env file already exists (kept): %s\n", envPath)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	fmt.Printf("Initialized data dir: %s\n", st.RootDir())

	if err := cmdInstallSkill(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install /pickup skill: %v\n", err)
	}

	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Put your Gemini API key into %s\n", envPath)
	fmt.Printf("  2. (Optional) Enable Telegram or ntfy in %s\n", configPath)
	fmt.Println("  3. Run: pregravity doctor")
	fmt.Println("  4. Test: pregravity chat")
	return nil
}

func cmdInstallSkill() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".gemini", "config", "skills", "pickup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	dest := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(dest, skills.PickupSkillMD, 0o644); err != nil {
		return fmt.Errorf("write skill: %w", err)
	}
	fmt.Printf("Installed Antigravity /pickup skill: %s\n", dest)
	return nil
}

func cmdDaemon(configPath string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.GeminiAPIKey == "" {
		return errors.New("GEMINI_API_KEY is not set; add it to ~/.config/pregravity/env before starting the daemon")
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	client := gemini.NewClient(cfg.GeminiAPIKey, cfg.Model, cfg.APIBaseURL, time.Duration(cfg.RequestTimeout)*time.Second)
	asst := assistant.New(cfg, st, client, logger)

	loc, err := cfg.Location()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mcpMgr := mcp.NewManager(ctx, cfg.MCPServers, logger)
	defer mcpMgr.Close()
	asst.SetMCPManager(mcpMgr)

	var notifiers []transport.Notifier
	var wg sync.WaitGroup

	if cfg.Notify.NtfyTopic != "" {
		notifiers = append(notifiers, &transport.NtfyNotifier{
			ServerURL: cfg.Notify.NtfyURL,
			Topic:     cfg.Notify.NtfyTopic,
			Token:     cfg.NtfyToken,
		})
	}
	if cfg.Notify.WebhookURL != "" {
		notifiers = append(notifiers, &transport.WebhookNotifier{
			URL: cfg.Notify.WebhookURL,
		})
	}

	if cfg.Telegram.Enabled {
		tg, err := transport.NewTelegramBot(cfg, st, asst, logger)
		if err != nil {
			return fmt.Errorf("init telegram: %w", err)
		}
		notifiers = append(notifiers, tg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			tg.Run(ctx)
		}()
	}

	if cfg.HTTP.Enabled {
		srv, err := transport.NewHTTPServer(cfg.HTTP.Listen, cfg.HTTPToken, cfg, st, asst)
		if err != nil {
			return fmt.Errorf("init http api: %w", err)
		}
		logger.Info("loopback http api listening", "addr", cfg.HTTP.Listen)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil {
				logger.Error("http server error", "err", err)
			}
		}()
	}

	notifier := &transport.MultiNotifier{Targets: notifiers}
	sched := cron.NewScheduler(loc, logger)
	enabledJobs := 0
	for _, s := range cfg.Schedules {
		if !s.Enabled {
			continue
		}
		sc := s
		err := sched.Add(sc.Name, sc.Cron, sc.Prompt, func(jobCtx context.Context, _ time.Time) {
			reply, err := asst.RunScheduledCheckin(jobCtx, sc.Name, sc.Prompt)
			if err != nil {
				logger.Error("scheduled check-in failed", "schedule", sc.Name, "err", err)
				return
			}
			_, _ = st.AppendJournal(fmt.Sprintf("[%s] %s", sc.Name, firstLine(reply.Text, 180)))
			if len(notifiers) == 0 {
				logger.Info("scheduled check-in completed (no notifier configured)", "schedule", sc.Name, "reply", reply.Text)
				return
			}
			if err := notifier.Notify(jobCtx, "Pregravity: "+sc.Name, reply.Text); err != nil {
				logger.Error("notify failed", "schedule", sc.Name, "err", err)
			}
		})
		if err != nil {
			return err
		}
		enabledJobs++
	}

	logger.Info("pregravity daemon started",
		"model", cfg.Model,
		"data_dir", st.RootDir(),
		"schedules", enabledJobs,
		"telegram", cfg.Telegram.Enabled,
		"http", cfg.HTTP.Enabled,
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.Run(ctx)
	}()

	// Zero-token due-task and one-shot reminder watcher (checks local store every 30s;
	// sends direct notifications only when an item enters its lead/due window).
	lead := time.Duration(cfg.DueLeadMinutes) * time.Minute
	wg.Add(1)
	go func() {
		defer wg.Done()
		checkDue := func() {
			due, err := st.ClaimDueNotifications(time.Now(), loc, lead)
			if err != nil {
				logger.Error("due check failed", "err", err)
				return
			}
			for _, d := range due {
				msg := formatDueNotification(d)
				logger.Info("firing due notification", "kind", d.Kind, "id", d.ID)
				_, _ = st.AppendJournal(fmt.Sprintf("[ping:%s] %s", d.ID, msg))
				// Record into allowed Telegram session(s) so replying directly has context.
				for _, chatID := range cfg.Telegram.AllowedChatIDs {
					_ = st.AppendSession(fmt.Sprintf("tg-%d", chatID), store.Message{
						Role:      "model",
						Text:      msg,
						Timestamp: time.Now().UTC(),
					})
				}
				if len(notifiers) > 0 {
					if err := notifier.Notify(ctx, "", msg); err != nil {
						logger.Error("due notify failed", "id", d.ID, "err", err)
					}
				}
			}
		}
		checkDue()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				checkDue()
			}
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down pregravity daemon...")
	wg.Wait()
	return nil
}

func cmdChat(configPath string, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	sessionID := fs.String("session", "cli-default", "session ID for conversation history")
	prompt := fs.String("p", "", "single-shot prompt (non-interactive)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	client := gemini.NewClient(cfg.GeminiAPIKey, cfg.Model, cfg.APIBaseURL, time.Duration(cfg.RequestTimeout)*time.Second)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	asst := assistant.New(cfg, st, client, logger)

	ctx := context.Background()
	mcpMgr := mcp.NewManager(ctx, cfg.MCPServers, logger)
	defer mcpMgr.Close()
	asst.SetMCPManager(mcpMgr)
	if strings.TrimSpace(*prompt) != "" {
		reply, err := asst.HandleMessage(ctx, *sessionID, *prompt)
		if err != nil {
			return err
		}
		fmt.Println(reply.Text)
		return nil
	}

	fmt.Printf("Pregravity (%s) — session: %s (type /quit to exit)\n", cfg.Model, *sessionID)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/quit" || line == "/exit" {
			break
		}
		reply, err := asst.HandleMessage(ctx, *sessionID, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		fmt.Printf("\npregravity> %s\n", reply.Text)
		if len(reply.ToolCalls) > 0 {
			fmt.Printf("  [tools: %s | tokens: %d]\n", strings.Join(reply.ToolCalls, ", "), reply.Tokens)
		}
	}
	return scanner.Err()
}

func cmdTasks(configPath string, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}

	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "list", "ls":
		status := "open"
		if len(args) > 0 {
			status = args[0]
		}
		tasks, err := st.ListTasks(status)
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			fmt.Printf("No %s tasks.\n", status)
			return nil
		}
		for _, t := range tasks {
			due := ""
			if t.Due != "" {
				due = " (due: " + t.Due + ")"
			}
			fmt.Printf("%-10s [%-6s] %-7s %s%s\n", t.ID, t.Priority, t.Status, t.Title, due)
		}
		return nil

	case "add":
		fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
		priority := fs.String("priority", "medium", "low, medium, or high")
		due := fs.String("due", "", "optional due date/time")
		notes := fs.String("notes", "", "optional notes")
		if err := fs.Parse(args); err != nil {
			return err
		}
		title := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if title == "" {
			return errors.New("usage: pregravity tasks add [--priority high] [--due tomorrow] <title>")
		}
		t, err := st.AddTask(title, *notes, *priority, *due)
		if err != nil {
			return err
		}
		fmt.Printf("Added task %s: %s\n", t.ID, t.Title)
		return nil

	case "done":
		if len(args) < 1 {
			return errors.New("usage: pregravity tasks done <task-id>")
		}
		t, err := st.UpdateTask(args[0], "done", "", "", "")
		if err != nil {
			return err
		}
		fmt.Printf("Completed %s: %s\n", t.ID, t.Title)
		return nil

	case "snooze":
		if len(args) < 1 {
			return errors.New("usage: pregravity tasks snooze <task-id> [new-due]")
		}
		due := ""
		if len(args) > 1 {
			due = strings.Join(args[1:], " ")
		}
		t, err := st.UpdateTask(args[0], "snoozed", due, "", "")
		if err != nil {
			return err
		}
		fmt.Printf("Snoozed %s: %s\n", t.ID, t.Title)
		return nil

	default:
		return fmt.Errorf("unknown tasks subcommand %q (expected: list, add, done, snooze)", sub)
	}
}

func cmdHandoffs(configPath string, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}

	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "list", "ls":
		status := "open"
		if len(args) > 0 {
			status = args[0]
		}
		hs, err := st.ListHandoffs(status)
		if err != nil {
			return err
		}
		if len(hs) == 0 {
			fmt.Printf("No %s handoffs.\n", status)
			return nil
		}
		for _, h := range hs {
			fmt.Printf("%-34s [%-4s] %s\n", h.ID, h.Status, h.Title)
		}
		return nil

	case "create":
		fs := flag.NewFlagSet("handoffs create", flag.ContinueOnError)
		goal := fs.String("goal", "", "goal for Antigravity")
		contextStr := fs.String("context", "", "optional context")
		constraints := fs.String("constraints", "", "optional constraints")
		if err := fs.Parse(args); err != nil {
			return err
		}
		title := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if title == "" {
			return errors.New("usage: pregravity handoffs create --goal \"...\" <title>")
		}
		g := strings.TrimSpace(*goal)
		if g == "" {
			g = title
		}
		h, err := st.CreateHandoff(title, g, *contextStr, *constraints, nil)
		if err != nil {
			return err
		}
		fmt.Printf("Created handoff: %s\n", h.ID)
		fmt.Printf("Briefing file:   %s\n", h.MarkdownPath)
		fmt.Printf("Antigravity UI:  %s (instance: %s)\n", cfg.Antigravity.DashboardURL, cfg.Antigravity.InstanceName)
		fmt.Printf("Pickup command:  %s %s\n", cfg.Antigravity.PickupSkill, h.ID)
		return nil

	case "show":
		if len(args) < 1 {
			return errors.New("usage: pregravity handoffs show <handoff-id>")
		}
		h, err := st.GetHandoff(args[0])
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(h.MarkdownPath)
		if err != nil {
			return err
		}
		fmt.Print(string(raw))
		if h.Status == "done" && h.Outcome != "" {
			fmt.Printf("\n--- Outcome (%s) ---\n%s\n", h.DoneNotePath, h.Outcome)
		}
		return nil

	case "done":
		if len(args) < 2 {
			return errors.New("usage: pregravity handoffs done <handoff-id> <summary>")
		}
		h, err := st.CompleteHandoff(args[0], strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		fmt.Printf("Marked handoff %s done (%s)\n", h.ID, h.DoneNotePath)
		return nil

	default:
		return fmt.Errorf("unknown handoffs subcommand %q (expected: list, create, show, done)", sub)
	}
}

func cmdQuota(configPath string, args []string) error {
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	if len(args) > 0 && args[0] == "set-limit" {
		if len(args) < 2 {
			return errors.New("usage: pregravity quota set-limit <daily-request-limit> [model]")
		}
		n, err := strconv.Atoi(strings.TrimSpace(args[1]))
		if err != nil || n < 0 {
			return fmt.Errorf("invalid daily request limit %q (expected non-negative integer)", args[1])
		}
		targetModel := cfg.Model
		if len(args) >= 3 && strings.TrimSpace(args[2]) != "" {
			targetModel = strings.TrimSpace(args[2])
		}
		if cfg.Quota.ModelDailyLimits == nil {
			cfg.Quota.ModelDailyLimits = map[string]int{}
		}
		cfg.Quota.ModelDailyLimits[targetModel] = n
		if err := config.Save(configPath, cfg); err != nil {
			return err
		}
		fmt.Printf("Saved daily request limit for %s: %d/day (%s)\n", targetModel, n, configPath)
		return nil
	}

	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	apiOnly := fs.Bool("api-only", false, "only show Gemini API usage (skip agy subscription check)")
	subOnly := fs.Bool("sub-only", false, "only show Antigravity subscription quota")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if !*subOnly {
		st, err := store.Open(cfg.DataDir)
		if err != nil {
			return err
		}
		summary, err := st.GetUsageSummary(time.Now(), cfg.Model, cfg.Quota.LimitForModel(cfg.Model))
		if err != nil {
			return err
		}
		fmt.Println(store.FormatQuotaReport(summary))
	}

	if !*apiOnly {
		if !*subOnly {
			fmt.Println()
		}
		fmt.Println("Antigravity Subscription Quota (via `agy -p /usage`)")
		fmt.Println("----------------------------------------------------")
		out, err := fetchAgySubQuota()
		if err != nil {
			fmt.Printf("Unavailable (%v)\n", err)
		} else {
			fmt.Println(strings.TrimSpace(out))
		}
	}
	return nil
}

func fetchAgySubQuota() (string, error) {
	agyBin, err := exec.LookPath("agy")
	if err != nil {
		home, _ := os.UserHomeDir()
		candidate := filepath.Join(home, ".local", "bin", "agy")
		if _, statErr := os.Stat(candidate); statErr == nil {
			agyBin = candidate
		} else {
			return "", errors.New("agy binary not found")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, agyBin, "-p", "/usage", "--print-timeout", "8s").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func cmdDoctor(configPath string) error {
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	fmt.Println("Pregravity Doctor")
	fmt.Println("-----------------")

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Printf("[FAIL] Config (%s): %v\n", configPath, err)
		return err
	}
	fmt.Printf("[OK]   Config loaded (%s) | model=%s | tz=%s\n", configPath, cfg.Model, cfg.Timezone)

	envPath := filepath.Join(filepath.Dir(configPath), "env")
	if fi, err := os.Stat(envPath); err == nil {
		mode := fi.Mode().Perm()
		if mode&0o077 != 0 {
			fmt.Printf("[WARN] Env file %s has loose permissions %04o (recommend: chmod 0600 %s)\n", envPath, mode, envPath)
		} else {
			fmt.Printf("[OK]   Env file permissions (%s: %04o)\n", envPath, mode)
		}
	} else {
		fmt.Printf("[INFO] No env file at %s (using process environment)\n", envPath)
	}

	if cfg.GeminiAPIKey != "" {
		fmt.Println("[OK]   GEMINI_API_KEY is set")
	} else {
		fmt.Println("[WAIT] GEMINI_API_KEY is NOT set yet (add it to ~/.config/pregravity/env)")
	}

	if _, err := store.Open(cfg.DataDir); err != nil {
		fmt.Printf("[FAIL] Data dir %s: %v\n", cfg.DataDir, err)
	} else {
		fmt.Printf("[OK]   Data dir ready (%s)\n", cfg.DataDir)
	}

	home, _ := os.UserHomeDir()
	skillPath := filepath.Join(home, ".gemini", "config", "skills", "pickup", "SKILL.md")
	if _, err := os.Stat(skillPath); err == nil {
		fmt.Printf("[OK]   Antigravity /pickup skill installed (%s)\n", skillPath)
	} else {
		fmt.Printf("[WARN] Antigravity /pickup skill not installed (run: pregravity install-skill)\n")
	}

	if inst := detectAgyInstanceName(); inst != "" {
		fmt.Printf("[OK]   agy remote-control active (instance: %s)\n", inst)
	} else {
		fmt.Println("[INFO] agy remote-control status unavailable (optional: run 'agy remote-control start')")
	}

	if cfg.Telegram.Enabled {
		if cfg.TelegramBotToken == "" {
			fmt.Println("[WAIT] Telegram enabled, but PREGRAVITY_TELEGRAM_BOT_TOKEN is empty")
		} else {
			fmt.Printf("[OK]   Telegram enabled (allowed_chat_ids=%v)\n", cfg.Telegram.AllowedChatIDs)
		}
	} else {
		fmt.Println("[INFO] Telegram transport disabled")
	}

	enabledMCP := 0
	for _, s := range cfg.MCPServers {
		if s.Enabled {
			enabledMCP++
		}
	}
	fmt.Printf("[INFO] Read-only MCP servers enabled: %d\n", enabledMCP)

	gtCreds := filepath.Join(filepath.Dir(configPath), "gtasks_credentials.json")
	gtToken := filepath.Join(cfg.DataDir, "gtasks_token.json")
	if _, err := os.Stat(gtToken); err == nil {
		fmt.Printf("[OK]   Google Tasks read-only token present (%s)\n", gtToken)
	} else if _, err := os.Stat(gtCreds); err == nil {
		fmt.Printf("[WAIT] Google Tasks credentials found (%s) — run: pregravity gtasks auth\n", gtCreds)
	} else {
		fmt.Printf("[INFO] Google Tasks not configured (optional: place OAuth Desktop JSON at %s)\n", gtCreds)
	}

	return nil
}

func cmdMCP(configPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pregravity mcp <gtasks>")
	}
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "gtasks":
		credsPath := filepath.Join(filepath.Dir(configPath), "gtasks_credentials.json")
		tokenPath := filepath.Join(cfg.DataDir, "gtasks_token.json")
		cli := gtasks.NewClient(credsPath, tokenPath, "")
		return gtasks.ServeStdioMCP(context.Background(), cli, os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown built-in mcp server %q (supported: gtasks)", args[0])
	}
}

func cmdGTasks(configPath string, args []string) error {
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	credsPath := filepath.Join(filepath.Dir(configPath), "gtasks_credentials.json")
	tokenPath := filepath.Join(cfg.DataDir, "gtasks_token.json")

	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}

	switch sub {
	case "auth":
		creds, err := gtasks.LoadCredentials(credsPath)
		if err != nil {
			return fmt.Errorf("%w\nDownload a Desktop OAuth Client JSON from Google Cloud Console (enable Google Tasks API) and save it to: %s", err, credsPath)
		}
		state := gtasks.RandomState()
		redirectURI := "http://127.0.0.1:8789/callback"
		authURL := gtasks.AuthURL(creds, redirectURI, state)

		fmt.Println("Google Tasks Read-Only OAuth Setup (scope: tasks.readonly)")
		fmt.Println("----------------------------------------------------------")
		fmt.Println("1. Open this URL in your browser and approve read-only access:")
		fmt.Println()
		fmt.Println(authURL)
		fmt.Println()

		codeCh := make(chan string, 1)
		ln, listenErr := net.Listen("tcp", "127.0.0.1:8789")
		if listenErr == nil {
			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("state") != state {
						http.Error(w, "invalid state", http.StatusBadRequest)
						return
					}
					if code := r.URL.Query().Get("code"); code != "" {
						_, _ = fmt.Fprintln(w, "Pregravity Google Tasks read-only auth complete! You can close this tab.")
						select {
						case codeCh <- code:
						default:
						}
					}
				}),
			}
			go func() { _ = srv.Serve(ln) }()
			defer srv.Close()
		}

		fmt.Print("2. If on a remote devbox without local browser forwarding, paste the full redirected URL (or `code` param) here and press Enter: ")
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			if sc.Scan() {
				raw := strings.TrimSpace(sc.Text())
				if raw == "" {
					return
				}
				if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
					if u, err := url.Parse(raw); err == nil {
						if c := u.Query().Get("code"); c != "" {
							raw = c
						}
					}
				}
				select {
				case codeCh <- raw:
				default:
				}
			}
		}()

		code := <-codeCh
		tok, err := gtasks.ExchangeCode(context.Background(), creds, code, redirectURI, nil)
		if err != nil {
			return err
		}
		if err := gtasks.SaveToken(tokenPath, tok); err != nil {
			return err
		}
		fmt.Printf("\nSaved read-only Google Tasks token (0600): %s\n", tokenPath)
		fmt.Println("Next: run `pregravity gtasks enable` to wire the read-only gtasks MCP server into Gravy.")
		return nil

	case "enable":
		exePath, err := os.Executable()
		if err != nil {
			home, _ := os.UserHomeDir()
			exePath = filepath.Join(home, ".local", "bin", "pregravity")
		}
		found := false
		for i := range cfg.MCPServers {
			if cfg.MCPServers[i].Name == "gtasks" {
				cfg.MCPServers[i].Enabled = true
				cfg.MCPServers[i].Transport = "stdio"
				cfg.MCPServers[i].Command = exePath
				cfg.MCPServers[i].Args = []string{"mcp", "gtasks"}
				found = true
				break
			}
		}
		if !found {
			ro := true
			cfg.MCPServers = append(cfg.MCPServers, mcp.ServerConfig{
				Name:      "gtasks",
				Enabled:   true,
				Transport: "stdio",
				Command:   exePath,
				Args:      []string{"mcp", "gtasks"},
				ReadOnly:  &ro,
			})
		}
		if err := config.Save(configPath, cfg); err != nil {
			return err
		}
		fmt.Printf("Enabled built-in read-only gtasks MCP server in %s\n", configPath)
		fmt.Println("Restart daemon to activate: systemctl --user restart pregravity.service")
		return nil

	case "list":
		cli := gtasks.NewClient(credsPath, tokenPath, "")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		lists, err := cli.ListTaskLists(ctx)
		if err != nil {
			return err
		}
		for _, l := range lists {
			fmt.Printf("== %s (%s) ==\n", l.Title, l.ID)
			items, err := cli.ListTasks(ctx, l.ID, false)
			if err != nil {
				fmt.Printf("  error: %v\n", err)
				continue
			}
			if len(items) == 0 {
				fmt.Println("  (no open tasks)")
				continue
			}
			for _, it := range items {
				due := ""
				if it.Due != "" {
					due = " (due " + it.Due + ")"
				}
				fmt.Printf("  - %s%s\n", it.Title, due)
			}
		}
		return nil

	default:
		return fmt.Errorf("unknown gtasks subcommand %q (expected: auth, enable, list)", sub)
	}
}

func detectAgyInstanceName() string {
	agyBin, err := exec.LookPath("agy")
	if err != nil {
		home, _ := os.UserHomeDir()
		candidate := filepath.Join(home, ".local", "bin", "agy")
		if _, statErr := os.Stat(candidate); statErr == nil {
			agyBin = candidate
		} else {
			return ""
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, agyBin, "remote-control", "status").CombinedOutput()
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`Instance name:\s*([a-zA-Z0-9._-]+)`)
	if m := re.FindSubmatch(out); len(m) == 2 {
		return string(m[1])
	}
	return ""
}

func formatDueNotification(d store.DueNotification) string {
	switch d.Kind {
	case "task_lead":
		return fmt.Sprintf("Heads up (%dm): %s [%s] — due %s", d.LeadMin, d.Title, d.ID, d.DueText)
	case "task_due":
		return fmt.Sprintf("Due now: %s [%s] (%s) — reply \"done\" when finished or tell me if you're pushing it.", d.Title, d.ID, d.DueText)
	default:
		return fmt.Sprintf("Reminder: %s [%s] (%s)", d.Title, d.ID, d.DueText)
	}
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

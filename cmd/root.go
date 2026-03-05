package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	"github.com/PlatoX-Type/monet-bot/agent"
	"github.com/PlatoX-Type/monet-bot/bus"
	"github.com/PlatoX-Type/monet-bot/channels"
	"github.com/PlatoX-Type/monet-bot/config"
	cronpkg "github.com/PlatoX-Type/monet-bot/cron"
	"github.com/PlatoX-Type/monet-bot/dashboard"
	"github.com/PlatoX-Type/monet-bot/heartbeat"
	"github.com/PlatoX-Type/monet-bot/hooks"
	"github.com/PlatoX-Type/monet-bot/providers"
	"github.com/PlatoX-Type/monet-bot/repos"
)

var (
	configPath  string
	channelName string
)

var rootCmd = &cobra.Command{
	Use:   "monet",
	Short: "CCMonet Bot — internal team agent",
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the bot",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.Load(configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		// Set API key env vars
		if cfg.LLM.APIKey != "" {
			switch {
			case contains(cfg.LLM.Provider, "openrouter"):
				os.Setenv("OPENROUTER_API_KEY", cfg.LLM.APIKey)
			case contains(cfg.LLM.Provider, "anthropic"):
				os.Setenv("ANTHROPIC_API_KEY", cfg.LLM.APIKey)
			case contains(cfg.LLM.Provider, "openai"):
				os.Setenv("OPENAI_API_KEY", cfg.LLM.APIKey)
			case contains(cfg.LLM.Provider, "gemini"):
				os.Setenv("GEMINI_API_KEY", cfg.LLM.APIKey)
			}
		}

		log.Printf("[mode] %s", cfg.Mode)

		// Initialize repos
		repos.InitAll(cfg)

		// Create message bus
		mb := bus.New()

		// Create LLM provider (base URL override from config takes precedence)
		provider := providers.NewWithBaseURL(cfg.LLM.APIKey, cfg.LLM.Model, cfg.LLM.Provider, cfg.LLM.BaseURL)

		// Create cron service (always create it so the cron tool works)
		cronSvc := cronpkg.New(mb, cfg.Workspace, cfg.Cron)

		// Create agent loop
		loop := agent.NewLoop(cfg, provider, mb, cronSvc)

		// Set up hooks/emitter and dashboard
		var emitter *hooks.Emitter
		if cfg.Dashboard.Enabled {
			emitter = hooks.NewEmitter()
		}
		loop.Emitter = emitter
		loop.SubagentManager.Emitter = emitter

		// Set up channels
		mgr := channels.NewManager(mb)

		// Register dashboard as both Channel and Hook
		if cfg.Dashboard.Enabled {
			dash := dashboard.New(mb, cfg.Dashboard.Port)
			mgr.Register(dash)
			emitter.Register(dash)
			log.Printf("[dashboard] enabled on port %d", cfg.Dashboard.Port)
		}

		switch channelName {
		case "cli":
			mgr.Register(channels.NewCLI(mb))
		case "lark":
			larkCfg := findChannel(cfg, "lark")
			if larkCfg == nil {
				log.Fatal("No Lark channel configured")
			}
			mgr.Register(channels.NewLark(mb, larkCfg.AppID, larkCfg.AppSecret, larkCfg.AllowFrom, 9000, cfg.Workspace))
		case "all":
			mgr.Register(channels.NewCLI(mb))
			if larkCfg := findChannel(cfg, "lark"); larkCfg != nil && larkCfg.Enabled {
				mgr.Register(channels.NewLark(mb, larkCfg.AppID, larkCfg.AppSecret, larkCfg.AllowFrom, 9000, cfg.Workspace))
			}
		}

		// Start agent loop in background
		go loop.Run()

		// Start heartbeat (LLM-powered skip/run decision)
		if cfg.Heartbeat.Enabled {
			go heartbeat.New(mb, provider, cfg.Workspace, cfg.Heartbeat.IntervalMinutes, cfg.LLM.MaxTokens, cfg.LLM.Temperature).Run()
		}

		// Start cron scheduler
		go cronSvc.Run()

		// Start repo pull loop
		if len(cfg.Repos) > 0 {
			go repos.PullLoop(cfg, 10)
		}

		fmt.Println("CCMonet Bot starting...")

		// Start channels (blocks)
		mgr.StartAll()
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("ccmonet-bot v0.1.0")
	},
}

func init() {
	runCmd.Flags().StringVarP(&configPath, "config", "c", "config.json", "Config file path")
	runCmd.Flags().StringVar(&channelName, "channel", "cli", "Channel to run (cli, lark, all)")
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(versionCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func findChannel(cfg *config.Config, typ string) *config.ChannelConfig {
	for _, ch := range cfg.Channels {
		if ch.Type == typ {
			return &ch
		}
	}
	return nil
}

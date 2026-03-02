package channels

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/PlatoX-Type/monet-bot/bus"
)

type CLIChannel struct {
	bus *bus.MessageBus
}

func NewCLI(mb *bus.MessageBus) *CLIChannel {
	return &CLIChannel{bus: mb}
}

func (c *CLIChannel) Name() string { return "cli" }

func (c *CLIChannel) Start() {
	fmt.Println("CCMonet Bot — type a message or /help")
	fmt.Println(strings.Repeat("─", 50))

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			fmt.Println("\nGoodbye.")
			os.Exit(0)
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		c.bus.Inbound <- bus.InboundMessage{
			Channel:   "cli",
			ChatID:    "local",
			User:      "local",
			Text:      text,
			Timestamp: time.Now(),
		}
	}
}

func (c *CLIChannel) Send(chatID, text string) {
	fmt.Printf("\n%s\n", text)
}

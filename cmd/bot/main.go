package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"

	"github.com/malbs/SecretMe/internal/bot"
	"github.com/malbs/SecretMe/internal/config"
	"github.com/malbs/SecretMe/internal/storage"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Connect to PostgreSQL
	store, err := storage.NewPostgresStorage(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer store.Close()

	// Create Telego bot
	telegramBot, err := telego.NewBot(cfg.BotToken)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	// Get bot info for deep links
	botUser, err := telegramBot.GetMe(context.Background())
	if err != nil {
		log.Fatalf("Failed to get bot info: %v", err)
	}
	log.Printf("Bot username: @%s", botUser.Username)

	// Set up graceful shutdown context
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start long polling with explicit update subscriptions
	updates, err := telegramBot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		AllowedUpdates: []string{
			telego.InlineQueryUpdates,
			telego.CallbackQueryUpdates,
			telego.ChosenInlineResultUpdates,
			telego.MessageUpdates,
		},
	})
	if err != nil {
		log.Fatalf("Failed to start long polling: %v", err)
	}

	// Create bot handler
	botHandler, err := th.NewBotHandler(telegramBot, updates)
	if err != nil {
		log.Fatalf("Failed to create bot handler: %v", err)
	}

	// Register handlers
	handler := bot.NewHandler(telegramBot, store, cfg, botUser.Username)
	handler.Register(botHandler)

	// Start handling updates
	log.Println("Bot is running... waiting for inline queries and callbacks")
	if err = botHandler.Start(); err != nil {
		log.Printf("Bot handler stopped with error: %v", err)
	}

	log.Println("Bot stopped")
}

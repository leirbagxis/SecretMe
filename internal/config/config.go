package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// Config holds all configuration values for the application.
type Config struct {
	BotToken    string
	DatabaseURL string
	ENCSalt     string
}

// Load reads environment variables and returns a Config.
func Load() (*Config, error) {
	_ = godotenv.Load() // ignore error if .env doesn't exist

	cfg := &Config{
		BotToken:    os.Getenv("BOT_TOKEN"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		ENCSalt:     os.Getenv("ENC_SALT"),
	}

	if cfg.BotToken == "" {
		return nil, fmt.Errorf("BOT_TOKEN is required")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.ENCSalt == "" {
		return nil, fmt.Errorf("ENC_SALT is required")
	}

	return cfg, nil
}

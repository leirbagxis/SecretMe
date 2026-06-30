package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
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
// Only BOT_TOKEN is strictly required.
// DATABASE_URL defaults to a local Docker Compose setup.
// ENC_SALT is auto-generated with a 32-byte random value if not provided.
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
		cfg.DatabaseURL = "postgres://secretme:secretme@localhost:5432/secretme?sslmode=disable"
		log.Printf("[CONFIG] DATABASE_URL not set, using default: %s", cfg.DatabaseURL)
	}

	if cfg.ENCSalt == "" {
		salt, err := generateSalt()
		if err != nil {
			return nil, fmt.Errorf("generate ENC_SALT: %w", err)
		}
		cfg.ENCSalt = salt
		log.Printf("[CONFIG] ENC_SALT not set, generated random salt")
	}

	return cfg, nil
}

// generateSalt creates a random 32-byte hex string for use as HKDF salt.
func generateSalt() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	return hex.EncodeToString(b), nil
}

# SecretMe — Telegram Secret Messages Bot

## Project

Golang Telegram bot for sending encrypted secret messages in group chats via inline mode.

- **Framework**: [telego](https://github.com/mymmrac/telego) v1.10.0 (Go Telegram Bot API)
- **Language**: Go
- **Status**: Working — implemented and reviewed

## How it works

User sends: `@botname @recipient_or_id Hello, this is secret`

Bot encrypts the message for the recipient only, posts a button in the chat. Only the specified recipient can decrypt and read it. When clicked, the button opens a private chat with the bot and the bot delivers the decrypted message there.

## Key architecture notes

- **Inline mode**: Bot processes inline queries to create secret messages
- **Encryption**: Messages must be encrypted so only the target user can decrypt (use Telegram user ID as key material)
- **Deep link delivery**: Instead of inline callbacks, the button uses a `https://t.me/bot?start=s_TOKEN` deep link. When clicked, the user opens the bot's PV and the bot receives `/start s_TOKEN` to look up and deliver the message.
- **/start handler**: Processes `Message` updates in PV — `/start` shows help, `/start s_TOKEN` looks up the encrypted message and sends it.
- **Group-only**: Bot must work correctly in group/supergroup contexts

## Commands

```bash
go build ./...          # Build
go vet ./...            # Static analysis
go test ./...           # Run tests
go run cmd/bot/main.go  # Run bot (requires BOT_TOKEN env)
```

## Environment

- `BOT_TOKEN` — Telegram Bot API token (required)
- `DATABASE_URL` — PostgreSQL connection string (required)
- `ENC_SALT` — Encryption salt for key derivation (required)

## Conventions

- Keep encryption logic in a dedicated package (e.g., `crypto/` or `cipher/`)
- Bot handlers should be in a dedicated package (e.g., `bot/` or `handlers/`)
- Use Go modules; no vendoring unless needed
- Prefer `net/http` for any webhook server; use long-polling for simplicity if no public URL

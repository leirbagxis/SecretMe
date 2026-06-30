package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/malbs/SecretMe/internal/config"
	"github.com/malbs/SecretMe/internal/crypto"
	"github.com/malbs/SecretMe/internal/storage"
)

// Handler holds dependencies for Telegram bot event handlers.
type Handler struct {
	bot         *telego.Bot
	store       *storage.PostgresStorage
	cfg         *config.Config
	botUsername string
}

// NewHandler creates a new Handler with the given dependencies.
func NewHandler(bot *telego.Bot, store *storage.PostgresStorage, cfg *config.Config, botUsername string) *Handler {
	return &Handler{
		bot:         bot,
		store:       store,
		cfg:         cfg,
		botUsername: botUsername,
	}
}

// Register sets up all update handlers on the provided BotHandler.
func (h *Handler) Register(bh *telegohandler.BotHandler) {
	// Middleware: capture username→userID mappings from all updates
	bh.Use(h.captureUsers)
	bh.Handle(h.handleInlineQuery, telegohandler.AnyInlineQuery())
	bh.Handle(h.handleCallbackQuery, telegohandler.AnyCallbackQuery())
	bh.Handle(h.handleChosenInlineResult, telegohandler.AnyChosenInlineResult())
	bh.Handle(h.handleMessage, telegohandler.AnyMessage())
}

// handleChosenInlineResult saves the inline_message_id when a user sends an inline result to a chat.
// This allows the bot to later edit the message to show a read receipt.
func (h *Handler) handleChosenInlineResult(ctx *telegohandler.Context, update telego.Update) error {
	chosen := update.ChosenInlineResult
	if chosen == nil || chosen.InlineMessageID == "" {
		return nil
	}
	log.Printf("[CHOSEN] Inline result registered")

	// The ResultID is the secret token
	token := chosen.ResultID
	if token == "" {
		return nil
	}

	if err := h.store.UpdateInlineMessageID(ctx, token, chosen.InlineMessageID); err != nil {
		log.Printf("[CHOSEN] Error saving inline_message_id: %v", err)
	}
	return nil
}

// captureUsers is a middleware that records username→userID mappings from any update.
func (h *Handler) captureUsers(ctx *telegohandler.Context, update telego.Update) error {
	log.Printf("[MIDDLEWARE] Processing update")

	type userInfo struct {
		Username string
		ID       int64
	}

	var users []userInfo

	if update.Message != nil && update.Message.From != nil {
		users = append(users, userInfo{Username: update.Message.From.Username, ID: update.Message.From.ID})
	}
	if update.InlineQuery != nil {
		users = append(users, userInfo{Username: update.InlineQuery.From.Username, ID: update.InlineQuery.From.ID})
	}
	if update.CallbackQuery != nil {
		users = append(users, userInfo{Username: update.CallbackQuery.From.Username, ID: update.CallbackQuery.From.ID})
	}

	for _, u := range users {
		if u.Username != "" {
			if err := h.store.SaveUsername(context.Background(), u.Username, u.ID); err != nil {
				log.Printf("[MIDDLEWARE] Error caching username: %v", err)
			}
		}
	}

	return ctx.Next(update)
}

// handleInlineQuery processes inline queries in the format:
//   @bot @recipient message                     → text secret (callback + alert)
//   @bot MEDIA_TOKEN @recipient message          → media secret (deep link → PV)
func (h *Handler) handleInlineQuery(ctx *telegohandler.Context, update telego.Update) error {
	inline := update.InlineQuery
	query := strings.TrimSpace(inline.Query)

	log.Printf("[INLINE] Processing query")

	if query == "" {
		log.Printf("[INLINE] Empty query, ignoring")
		return nil
	}

	// Parse query: first part is either a recipient (@username/ID) or a media token
	parts := strings.SplitN(query, " ", 2)
	if len(parts) < 2 {
		log.Printf("[INLINE] Only one part in query, ignoring")
		return nil
	}

	first := parts[0]
	rest := parts[1]

	// Check if first part is a media token (8 hex chars, not starting with @, not numeric)
	var fileID, fileType string
	if !strings.HasPrefix(first, "@") {
		if _, err := strconv.ParseInt(first, 10, 64); err != nil {
			// Not a numeric ID and not @ — try as media token
			media, err := h.store.GetMediaByToken(ctx, first)
			if err == nil && media.SenderID == inline.From.ID {
				// Valid media token
				fileID = media.FileID
				fileType = media.FileType
				log.Printf("[INLINE] Media token accepted: type=%s", fileType)
				// Delete the media token after use (one-time use)
				_ = h.store.DeleteMedia(ctx, first)
				// Parse recipient and optional message from rest
				parts = strings.SplitN(rest, " ", 2)
				if len(parts) < 1 || parts[0] == "" {
					log.Printf("[INLINE] Media token but no recipient")
					return nil
				}
				first = parts[0]
				rest = ""
				if len(parts) >= 2 {
					rest = parts[1]
				}
			} else if err != nil {
				log.Printf("[INLINE] Media token rejected: not found")
				return nil
			} else {
				log.Printf("[INLINE] Media token rejected: wrong owner")
				return nil
			}
		}
	}

	// Now first = recipient, rest = message text
	recipientStr := first
	messageText := rest
	log.Printf("[INLINE] Parsed: recipient_type=%s message_len=%d fileType=%q",
		map[bool]string{true: "@username", false: "numeric_id"}[strings.HasPrefix(recipientStr, "@")],
		len(messageText), fileType)

	// Parse recipient
	var recipientID int64
	var recipientUsername string
	var cryptoIdentity string
	var err error
	if strings.HasPrefix(recipientStr, "@") {
		username := strings.TrimPrefix(recipientStr, "@")
		if username == "" {
			log.Printf("[INLINE] Empty @username")
			return nil
		}
		cryptoIdentity = crypto.IdentityUsername(username)
		recipientUsername = username
		log.Printf("[INLINE] Recipient by @username")
	} else {
		recipientID, err = strconv.ParseInt(recipientStr, 10, 64)
		if err != nil {
			log.Printf("[INLINE] Invalid recipient format")
			return nil
		}
		cryptoIdentity = crypto.IdentityNumeric(recipientID)
		log.Printf("[INLINE] Recipient by numeric ID")
	}

	// Encrypt the message
	log.Printf("[INLINE] Encrypting message...")
	encryptedData, nonce, err := crypto.Encrypt([]byte(messageText), cryptoIdentity, h.cfg.ENCSalt)
	if err != nil {
		log.Printf("[INLINE] Encrypt error: %v", err)
		return nil
	}
	log.Printf("[INLINE] Encrypted: data=%d bytes nonce=%d bytes", len(encryptedData), len(nonce))

	// Generate a random token and save to storage
	token, err := storage.GenerateToken()
	if err != nil {
		log.Printf("[INLINE] Token generation error: %v", err)
		return nil
	}
	log.Printf("[INLINE] Token generated")

	err = h.store.SaveMessage(ctx, token, 0, inline.From.ID, recipientID, recipientUsername,
		encryptedData, nonce, fileID, fileType)
	if err != nil {
		log.Printf("[INLINE] Save error: %v", err)
		return nil
	}
	log.Printf("[INLINE] Message saved")

	// Build recipient label for display (title is plain text)
	// We prefer @username; fall back to a generic label.
	var recipientLabel string
	var recipientLinkID int64 // for HTML profile link in message content

	if recipientUsername != "" {
		recipientLabel = "@" + recipientUsername
		// Try to look up the user ID for the profile link
		if uid, err := h.store.GetUserIDByUsername(ctx, recipientUsername); err == nil && uid != 0 {
			recipientLinkID = uid
		}
	} else {
		recipientLinkID = recipientID
		// Try to find a cached username for this ID
		if uname, err := h.store.GetUsernameByUserID(ctx, recipientID); err == nil && uname != "" {
			recipientLabel = "@" + uname
		} else {
			recipientLabel = "usuário" // generic fallback, profile link still works
		}
	}

	// Create inline result
	title := fmt.Sprintf("🔒 Mensagem de sussurro para %s", recipientLabel)
	description := "Clique no botão para ler."

	// Build input text with HTML profile link (only if we have a valid user ID)
	var inputText string
	if recipientLinkID != 0 {
		inputText = fmt.Sprintf(
			"🔒 Mensagem de sussurro para <a href=\"tg://user?id=%d\">%s</a>\n\nClique no botão abaixo para ler.",
			recipientLinkID, recipientLabel,
		)
	} else {
		// @username only, no cached ID — plain text
		inputText = fmt.Sprintf(
			"🔒 Mensagem de sussurro para %s\n\nClique no botão abaixo para ler.",
			recipientLabel,
		)
	}

	buttonText := "🔒 Abrir Mensagem Secreta"

	if fileType != "" || utf8.RuneCountInString(messageText) > 200 {
		// Long text or media: button with deep link URL → opens PV for delivery
		deepLink := fmt.Sprintf("https://t.me/%s?start=s_%s", h.botUsername, token)
		result := tu.ResultArticle(
			token,
			title,
			tu.TextMessage(inputText).WithParseMode("HTML"),
		).WithDescription(description).
			WithReplyMarkup(
				tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(buttonText).WithURL(deepLink),
					),
				),
			)
		log.Printf("[INLINE] Answering with URL deep link result")
		err = h.bot.AnswerInlineQuery(ctx,
			tu.InlineQuery(inline.ID, result).WithIsPersonal().WithCacheTime(0))
	} else {
		// Short text: button with callback data → alert in group
		callbackData := fmt.Sprintf("secret:%s", token)
		result := tu.ResultArticle(
			token,
			title,
			tu.TextMessage(inputText).WithParseMode("HTML"),
		).WithDescription(description).
			WithReplyMarkup(
				tu.InlineKeyboard(
					tu.InlineKeyboardRow(
						tu.InlineKeyboardButton(buttonText).WithCallbackData(callbackData),
					),
				),
			)
		log.Printf("[INLINE] Answering with callback result")
		err = h.bot.AnswerInlineQuery(ctx,
			tu.InlineQuery(inline.ID, result).WithIsPersonal().WithCacheTime(0))
	}

	if err != nil {
		log.Printf("[INLINE] AnswerInlineQuery error: %v", err)
		return fmt.Errorf("answer inline query: %w", err)
	}

	log.Printf("[INLINE] Inline query answered successfully!")
	return nil
}

// handleCallbackQuery processes callback queries to decrypt and display secret messages.
func (h *Handler) handleCallbackQuery(ctx *telegohandler.Context, update telego.Update) error {
	callback := update.CallbackQuery

	if callback == nil {
		log.Println("[CALLBACK] ERROR: callback is nil")
		return nil
	}

	log.Printf("[CALLBACK] Processing callback")

	// Parse callback data
	data := callback.Data
	if !strings.HasPrefix(data, "secret:") {
		log.Printf("[CALLBACK] Unknown callback prefix, ignoring")
		return nil
	}

	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		log.Printf("[CALLBACK] Invalid callback format")
		return h.answerCallbackError(callback.ID, "Dados de callback inválidos")
	}

	token := parts[1]

	// Get the message from storage by token
	msg, err := h.store.GetMessageByToken(ctx, token)
	if err != nil {
		log.Printf("[CALLBACK] Message lookup failed: %v", err)
		return h.answerCallbackError(callback.ID, "Mensagem não encontrada ou já lida")
	}
	log.Printf("[CALLBACK] Message found, data_len=%d", len(msg.EncryptedData))

	// Authorization check
	var callerAuthorized bool
	var decryptIdentity string

	if msg.RecipientUsername != "" {
		callerAuthorized = strings.EqualFold(callback.From.Username, msg.RecipientUsername) ||
			callback.From.ID == msg.SenderID
		decryptIdentity = crypto.IdentityUsername(msg.RecipientUsername)
	} else {
		callerAuthorized = callback.From.ID == msg.RecipientID ||
			callback.From.ID == msg.SenderID
		decryptIdentity = crypto.IdentityNumeric(msg.RecipientID)
	}

	if !callerAuthorized {
		log.Printf("[CALLBACK] Authorization failed")
		return h.answerCallbackError(callback.ID, "Esta mensagem não é para você")
	}
	log.Printf("[CALLBACK] Authorization passed")

	// Decrypt
	plaintext, err := crypto.Decrypt(msg.EncryptedData, msg.Nonce, decryptIdentity, h.cfg.ENCSalt)
	if err != nil {
		log.Printf("[CALLBACK] Decrypt error: %v", err)
		return h.answerCallbackError(callback.ID, "Erro ao descriptografar")
	}
	log.Printf("[CALLBACK] Decrypted successfully (%d bytes)", len(plaintext))

	// Mark as read
	readAt := time.Now()
	_ = h.store.MarkAsReadByToken(ctx, token, readAt)

	// Answer callback
	plainStr := string(plaintext)
	if utf8.RuneCountInString(plainStr) <= 200 {
		// Short message: show directly in alert
		log.Printf("[CALLBACK] Answering with alert")
		err = h.bot.AnswerCallbackQuery(ctx, tu.CallbackQuery(callback.ID).
			WithText(plainStr).
			WithShowAlert())
	} else {
		// Long message: redirect to PV via deep link URL
		log.Printf("[CALLBACK] Message too long, redirecting to PV")
		deepLink := fmt.Sprintf("https://t.me/%s?start=s_%s", h.botUsername, token)
		err = h.bot.AnswerCallbackQuery(ctx, tu.CallbackQuery(callback.ID).
			WithText("📝 Mensagem muito longa! Veja no privado do bot.").
			WithURL(deepLink))
	}
	if err != nil {
		log.Printf("[CALLBACK] Error answering callback: %v", err)
		return fmt.Errorf("answer callback query: %w", err)
	}

	// Edit message to show read receipt — only when intended recipient reads
	isRecipient := (msg.RecipientUsername != "" && strings.EqualFold(callback.From.Username, msg.RecipientUsername)) ||
		(msg.RecipientUsername == "" && callback.From.ID == msg.RecipientID)

	if isRecipient {
		readByDisplay := callback.From.Username
		if readByDisplay == "" {
			readByDisplay = callback.From.FirstName
		}

		loc, err := time.LoadLocation("America/Sao_Paulo")
		if err != nil {
			loc = time.UTC
		}
		editText := fmt.Sprintf("📖 Lida por <a href=\"tg://user?id=%d\">%s</a> às %s",
			callback.From.ID, readByDisplay,
			readAt.In(loc).Format("15:04"))

		// Build reply button with sender ID pre-filled
		replyQuery := fmt.Sprintf("%d ", msg.SenderID)
		replyKeyboard := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("💬 Responder").WithSwitchInlineQueryCurrentChat(replyQuery),
			),
		)

		if callback.Message != nil && callback.Message.IsAccessible() {
			if msgObj := callback.Message.Message(); msgObj != nil {
				_, _ = h.bot.EditMessageText(ctx, tu.EditMessageText(
					tu.ID(msgObj.Chat.ID),
					msgObj.MessageID,
					editText,
				).WithParseMode("HTML").WithReplyMarkup(replyKeyboard))
			}
		} else if callback.InlineMessageID != "" {
			_, _ = h.bot.EditMessageText(ctx, &telego.EditMessageTextParams{
				InlineMessageID: callback.InlineMessageID,
				Text:            editText,
				ParseMode:       "HTML",
				ReplyMarkup:     replyKeyboard,
			})
		}
	} else {
		log.Printf("[CALLBACK] Sender read, keeping button intact")
	}

	log.Printf("[CALLBACK] Handler done")
	return nil
}

// handleMessage processes all Message updates:
// - /start (help) and /start s_TOKEN (deliver secret) in PV
// - Media uploads (photo/video/audio/document) in PV → generates a media token
func (h *Handler) handleMessage(ctx *telegohandler.Context, update telego.Update) error {
	msg := update.Message
	if msg == nil {
		return nil
	}

	// ── /start (help) ──
	if msg.Text == "/start" {
		log.Printf("[MSG] Processing /start command")
		_, err := h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID),
			"👋 Eu sou o bot de mensagens secretas!\n\n"+
				"📝 Texto: Use @"+h.botUsername+" @destinatario sua mensagem em qualquer grupo.\n"+
				"📷 Mídia: Envie uma foto/vídeo/áudio aqui no PV, depois use o token no inline.\n"+
				"🔒 A mensagem só pode ser lida pelo destinatário.",
		))
		if err != nil {
			log.Printf("[MSG] Error sending start: %v", err)
		}
		return nil
	}

	// ── /start s_TOKEN (deliver secret in PV) ──
	if strings.HasPrefix(msg.Text, "/start s_") {
		return h.deliverSecret(ctx, msg, strings.TrimPrefix(msg.Text, "/start s_"))
	}

	// ── Media upload in PV: photo, video, audio, document ──
	if msg.Chat.Type == telego.ChatTypePrivate &&
		(msg.Photo != nil || msg.Video != nil || msg.Audio != nil || msg.Document != nil) {
		return h.handleMediaUpload(ctx, msg)
	}

	return nil
}

// handleMediaUpload processes a media file sent to the bot in PV.
// It stores the file_id and returns a token for use in inline mode.
func (h *Handler) handleMediaUpload(ctx context.Context, msg *telego.Message) error {
	var fileID, fileType string
	var typeEmoji string

	if len(msg.Photo) > 0 {
		fileID = msg.Photo[len(msg.Photo)-1].FileID // largest photo
		fileType = "photo"
		typeEmoji = "📷"
	} else if msg.Video != nil {
		fileID = msg.Video.FileID
		fileType = "video"
		typeEmoji = "🎬"
	} else if msg.Audio != nil {
		fileID = msg.Audio.FileID
		fileType = "audio"
		typeEmoji = "🎵"
	} else if msg.Document != nil {
		fileID = msg.Document.FileID
		fileType = "document"
		typeEmoji = "📎"
	} else {
		return nil
	}

	log.Printf("[UPLOAD] %s type=%s", typeEmoji, fileType)

	token, err := storage.GenerateShortToken()
	if err != nil {
		log.Printf("[UPLOAD] Token error: %v", err)
		_, _ = h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID), "❌ Erro ao gerar token.",
		))
		return nil
	}

	if err := h.store.SaveMedia(ctx, token, fileID, fileType, msg.From.ID); err != nil {
		log.Printf("[UPLOAD] Save error: %v", err)
		_, _ = h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID), "❌ Erro ao salvar mídia.",
		))
		return nil
	}

	response := fmt.Sprintf(
		"%s Mídia recebida!\n\n"+
			"📌 Token: <code>%s</code>\n\n"+
			"Agora use no inline de qualquer grupo:\n"+
			"<code>@%s %s @destinatario sua mensagem</code>",
		typeEmoji, token, h.botUsername, token,
	)
	_, err = h.bot.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), response).
		WithParseMode("HTML"))
	if err != nil {
		log.Printf("[UPLOAD] Error sending response: %v", err)
		return fmt.Errorf("send upload response: %w", err)
	}

	log.Printf("[UPLOAD] Media stored and token sent to user")
	return nil
}

// deliverSecret looks up the token, authorizes, decrypts, and sends the content in PV.
func (h *Handler) deliverSecret(ctx context.Context, msg *telego.Message, token string) error {
	log.Printf("[DELIVER] Processing deep link delivery")

	if token == "" {
		return nil
	}

	secret, err := h.store.GetMessageByToken(ctx, token)
	if err != nil {
		log.Printf("[DELIVER] Token not found")
		_, _ = h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID), "❌ Mensagem não encontrada ou já foi lida.",
		))
		return nil
	}

	// Authorize
	var authorized bool
	if secret.RecipientUsername != "" {
		authorized = strings.EqualFold(msg.From.Username, secret.RecipientUsername) ||
			msg.From.ID == secret.SenderID
	} else {
		authorized = msg.From.ID == secret.RecipientID ||
			msg.From.ID == secret.SenderID
	}
	if !authorized {
		log.Printf("[DELIVER] Authorization failed")
		_, _ = h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID), "❌ Esta mensagem não é para você.",
		))
		return nil
	}

	// Decrypt
	var decryptIdentity string
	if secret.RecipientUsername != "" {
		decryptIdentity = crypto.IdentityUsername(secret.RecipientUsername)
	} else {
		decryptIdentity = crypto.IdentityNumeric(secret.RecipientID)
	}
	plaintext, err := crypto.Decrypt(secret.EncryptedData, secret.Nonce, decryptIdentity, h.cfg.ENCSalt)
	if err != nil {
		log.Printf("[DELIVER] Decrypt error: %v", err)
		_, _ = h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID), "❌ Erro ao descriptografar.",
		))
		return nil
	}

	// Mark as read
	_ = h.store.MarkAsReadByToken(ctx, token, time.Now())

	// Send media or text in PV
	plainStr := strings.TrimSpace(string(plaintext))
	var caption string
	if plainStr != "" {
		caption = fmt.Sprintf("📩 Mensagem secreta recebida!\n\n%s", plainStr)
	} else {
		caption = "📩 Mensagem secreta recebida!"
	}

	// Build reply keyboard for PV delivery
	replyQuery := fmt.Sprintf("%d ", secret.SenderID)
	replyKeyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("💬 Responder no grupo").WithSwitchInlineQuery(replyQuery),
		),
	)

	if secret.FileType == "photo" && secret.FileID != "" {
		log.Printf("[DELIVER] Sending photo (spoiler)")
		_, err = h.bot.SendPhoto(ctx, &telego.SendPhotoParams{
			ChatID:      tu.ID(msg.Chat.ID),
			Photo:       telego.InputFile{FileID: secret.FileID},
			Caption:     caption,
			HasSpoiler:  true,
			ReplyMarkup: replyKeyboard,
		})
	} else if secret.FileType == "video" && secret.FileID != "" {
		log.Printf("[DELIVER] Sending video (spoiler)")
		_, err = h.bot.SendVideo(ctx, &telego.SendVideoParams{
			ChatID:      tu.ID(msg.Chat.ID),
			Video:       telego.InputFile{FileID: secret.FileID},
			Caption:     caption,
			HasSpoiler:  true,
			ReplyMarkup: replyKeyboard,
		})
	} else if secret.FileType == "audio" && secret.FileID != "" {
		log.Printf("[DELIVER] Sending audio")
		_, err = h.bot.SendAudio(ctx, &telego.SendAudioParams{
			ChatID:      tu.ID(msg.Chat.ID),
			Audio:       telego.InputFile{FileID: secret.FileID},
			Caption:     caption,
			ReplyMarkup: replyKeyboard,
		})
	} else if secret.FileType == "document" && secret.FileID != "" {
		log.Printf("[DELIVER] Sending document")
		_, err = h.bot.SendDocument(ctx, &telego.SendDocumentParams{
			ChatID:      tu.ID(msg.Chat.ID),
			Document:    telego.InputFile{FileID: secret.FileID},
			Caption:     caption,
			ReplyMarkup: replyKeyboard,
		})
	} else {
		if plainStr == "" {
			log.Printf("[DELIVER] Empty message, nothing to send")
			_, _ = h.bot.SendMessage(ctx, tu.Message(
				tu.ID(msg.Chat.ID), "❌ Mensagem vazia.",
			))
			return nil
		}
		_, err = h.bot.SendMessage(ctx, tu.Message(
			tu.ID(msg.Chat.ID), caption,
		).WithReplyMarkup(replyKeyboard))
	}

	if err != nil {
		log.Printf("[DELIVER] Error sending: %v", err)
		return fmt.Errorf("deliver secret: %w", err)
	}

	// If we have an inline_message_id, edit the group message to show read receipt
	isRecipient := (secret.RecipientUsername != "" && strings.EqualFold(msg.From.Username, secret.RecipientUsername)) ||
		(secret.RecipientUsername == "" && msg.From.ID == secret.RecipientID)

	if isRecipient && secret.InlineMessageID != "" {
		readByDisplay := msg.From.Username
		if readByDisplay == "" {
			readByDisplay = msg.From.FirstName
		}
		loc, _ := time.LoadLocation("America/Sao_Paulo")
		if loc == nil {
			loc = time.UTC
		}
		editText := fmt.Sprintf("📖 Lida por <a href=\"tg://user?id=%d\">%s</a> às %s",
			msg.From.ID, readByDisplay,
			time.Now().In(loc).Format("15:04"))
		_, err = h.bot.EditMessageText(ctx, &telego.EditMessageTextParams{
			InlineMessageID: secret.InlineMessageID,
			Text:            editText,
			ParseMode:       "HTML",
		})
		if err != nil {
			log.Printf("[DELIVER] Error editing inline message (non-fatal): %v", err)
		} else {
			log.Printf("[DELIVER] Inline message updated to read receipt")
		}
	}

	log.Printf("[DELIVER] Delivered successfully!")
	return nil
}

func (h *Handler) answerCallbackError(queryID, text string) error {
	err := h.bot.AnswerCallbackQuery(context.Background(), tu.CallbackQuery(queryID).WithText(text).WithShowAlert())
	if err != nil {
		return fmt.Errorf("answer callback query: %w", err)
	}
	return nil
}

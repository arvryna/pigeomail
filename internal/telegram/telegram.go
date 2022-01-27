package telegram

import (
	"context"
	"fmt"
	"html"
	"time"

	"pigeomail/internal/repository"
	"pigeomail/rabbitmq"

	"github.com/go-logr/logr"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/streadway/amqp"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	updates  tgbotapi.UpdatesChannel
	repo     repository.IEmailRepository
	consumer rabbitmq.IRMQEmailConsumer
	domain   string
	logger   logr.Logger
}

func NewTGBot(
	config *Config,
	rmqCfg *rabbitmq.Config,
	repo repository.IEmailRepository,
	domain string,
	log logr.Logger,
) (*Bot, error) {
	bot, err := tgbotapi.NewBotAPI(config.Token)
	if err != nil {
		return nil, err
	}

	bot.Debug = config.Debug

	log.Info("authorized", "account", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	var consumer rabbitmq.IRMQEmailConsumer
	if consumer, err = rabbitmq.NewRMQEmailConsumer(rmqCfg, log); err != nil {
		return nil, err
	}

	return &Bot{
		api:      bot,
		updates:  updates,
		repo:     repo,
		consumer: consumer,
		domain:   domain,
		logger:   log,
	}, nil
}

func (b *Bot) handleCommand(update *tgbotapi.Update) {
	// Extract the command from the Message.
	switch update.Message.Command() {
	case createCommand:
		b.handleCreateCommandStep1(update)
	case listCommand:
		b.handleListCommand(update)
	case deleteCommand:
		b.handleDeleteCommandStep1(update)
	case helpCommand, startCommand:
		b.handleHelpCommand(update)
	default:
		msg := tgbotapi.NewMessage(update.Message.Chat.ID, "I don't know that command")
		if _, err := b.api.Send(msg); err != nil {
			b.logger.Error(err, "error send message")
		}
	}
}

func (b *Bot) incomingEmailConsumer(msg *amqp.Delivery) {
	from, ok := msg.Headers["from"]
	if !ok {
		b.logger.Error(nil, "error to extract 'from' header from message")
		_ = msg.Reject(false)
	}

	to, ok := msg.Headers["to"]
	if !ok {
		b.logger.Error(nil, "error to extract 'to' header from message")
		_ = msg.Reject(false)
	}

	subject, ok := msg.Headers["subject"]
	if !ok {
		b.logger.Error(nil, "error to extract 'subject' header from message")
		_ = msg.Reject(false)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	chatID, err := b.repo.GetChatIDByEmail(ctx, to.(string))
	if err != nil {
		b.logger.Error(err, "chatID not found", "email", to)
		_ = msg.Reject(false)
	}

	textTemplate := `
<b>From:</b> %s
<b>To:</b> %s
<b>Subject:</b> %s
----------------
%s
----------------
`
	if len(msg.Body) > 4096 {
		text := fmt.Sprintf(
			textTemplate,
			html.EscapeString(from.(string)),
			html.EscapeString(to.(string)),
			html.EscapeString(subject.(string)),
			html.EscapeString(string(msg.Body[:3000])),
		)

		tgMsg := tgbotapi.NewMessage(chatID, text)
		tgMsg.ParseMode = tgbotapi.ModeHTML

		if _, err = b.api.Send(tgMsg); err != nil {
			b.logger.Error(err, "error send message")
		}

		for i := 3000; i < len(msg.Body); i += 4096 {
			y := i + 4096
			if y > len(msg.Body) {
				y = len(msg.Body)
			}

			tgMsg = tgbotapi.NewMessage(chatID, html.EscapeString(string(msg.Body[i:y])))
			tgMsg.ParseMode = tgbotapi.ModeHTML

			if _, err = b.api.Send(tgMsg); err != nil {
				b.logger.Error(err, "error send message")
			}
		}

		_ = msg.Ack(false)
		return
	}

	text := fmt.Sprintf(
		textTemplate,
		html.EscapeString(from.(string)),
		html.EscapeString(to.(string)),
		html.EscapeString(subject.(string)),
		html.EscapeString(string(msg.Body)),
	)

	tgMsg := tgbotapi.NewMessage(chatID, text)
	tgMsg.ParseMode = tgbotapi.ModeHTML

	if _, err = b.api.Send(tgMsg); err != nil {
		b.logger.Error(err, "error send message")
	}

	_ = msg.Ack(false)
}

func (b *Bot) runConsumer() {
	b.consumer.ConsumeIncomingEmail(b.incomingEmailConsumer)
}

func (b *Bot) Run() {
	go b.runConsumer()

	for update := range b.updates {
		update := update
		if !validateIncomingMessage(update.Message) {
			continue
		}

		if update.Message.IsCommand() {
			b.handleCommand(&update)
			continue
		}

		b.handleUserInput(&update)
	}
}

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/k0kubun/pp/v3"
)

type BotStats struct {
	startTime      time.Time
	tgMsgsReceived int64
	tgMsgsSent     int64
	apiReqCnt      int64
	errors         int64
}

const promptFile = "Prompt.md"

func main() {
	tgBotToken := os.Getenv("TG_BOT_TOKEN")
	if tgBotToken == "" {
		log.Panic("TG_BOT_TOKEN not set in environment variables, please set it")
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	stats := &BotStats{
		startTime: time.Now(),
	}

	tgStream := make(chan *models.Update, 1024)

	opts := []bot.Option{
		bot.WithDefaultHandler(func(ctx context.Context, b *bot.Bot, update *models.Update) {
			select {
			case tgStream <- update:
				// Successfully sent update
			case <-ctx.Done():
				// Context cancelled, close channel and return
				close(tgStream)
				return
			}
		}),
	}

	luffy, err := bot.New(tgBotToken, opts...)
	if err != nil {
		log.Panic(err)
		return
	}

	go luffy.Start(ctx)

	ai := anthropic.NewClient()
	conversation := []anthropic.MessageParam{}

	promptTxt, err := os.ReadFile(promptFile)
	if err != nil {
		log.Panic(err)
		return
	}
	fmt.Printf("Found system prompt %v: %v", promptFile, string(promptTxt))
	systemPrompt := []anthropic.TextBlockParam{
		{Text: string(promptTxt)},
	}

	for {
		select {
		case tgMsg := <-tgStream:
			pp.Println(tgMsg.Message.Text)
			if tgMsg.Message != nil {
				stats.tgMsgsReceived += 1

				text := tgMsg.Message.Text
				chatID := tgMsg.Message.Chat.ID

				userMessage := anthropic.NewUserMessage(anthropic.NewTextBlock(text))
				conversation = append(conversation, userMessage)

				sentMsg, err := luffy.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   "🤔 Luffy is thinking...",
				})
				if err != nil {
					return
				}

				stream := ai.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
					Model:     anthropic.ModelClaude4Sonnet20250514,
					MaxTokens: int64(1024),
					Messages:  conversation,
					System:    systemPrompt,
				})

				message := anthropic.Message{}
				lastSendTime := time.Now()
				batchThreshold := 1 * time.Second
				firstEdit := true
				lastSentText := ""
				for stream.Next() {
					event := stream.Current()
					err := message.Accumulate(event)
					if err != nil {
						panic(err)
					}

					now := time.Now()
					if firstEdit || now.Sub(lastSendTime) >= batchThreshold {
						if len(message.Content) > 0 && len(message.Content[0].Text) > 0 {
							text := message.Content[0].Text

							// Only edit if the content has changed
							if text != lastSentText {
								if firstEdit {
									_, err = luffy.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
										ChatID:    chatID,
										MessageID: sentMsg.ID,
										Reaction: []models.ReactionType{
											{
												Type: models.ReactionTypeTypeEmoji,
												ReactionTypeEmoji: &models.ReactionTypeEmoji{
													Type:  models.ReactionTypeTypeEmoji,
													Emoji: "✍",
												},
											},
										},
									})
									if err != nil {
										log.Panic(err)
										return
									}
									firstEdit = false
								}

								_, err = luffy.EditMessageText(ctx, &bot.EditMessageTextParams{
									ChatID:    chatID,
									MessageID: sentMsg.ID,
									Text:      text,
								})
								if err != nil {
									log.Panic(err)
									return
								}
								lastSentText = text
								lastSendTime = now
							}
						}
					}
				}

				// Send final message with HTML parsing if content is complete
				finalText := ""
				if len(message.Content) > 0 {
					finalText = message.Content[0].Text
				}

				_, err = luffy.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    chatID,
					MessageID: sentMsg.ID,
					Text:      finalText,
					ParseMode: models.ParseModeHTML,
				})
				if err != nil {
					log.Panic(err)
					return
				}

				if stream.Err() != nil {
					log.Panic(stream.Err())
					continue
				}

				// Only append message if it has content
				if len(message.Content) > 0 {
					conversation = append(conversation, message.ToParam())
				}

				_, err = luffy.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
					ChatID:    chatID,
					MessageID: sentMsg.ID,
				})
				if err != nil {
					log.Panic(err)
				}

			}
		case <-ctx.Done():
			fmt.Println("Luffy is tired... going to sleep")
			pp.Print(stats)
			return
		}
	}
}

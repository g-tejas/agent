package main

import (
	"context"
	"encoding/json"
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

const conversationFile = "conversation.json"
const promptFile = "Prompt.md"

func saveConversation(conversation []anthropic.MessageParam) error {
	file, err := os.Create(conversationFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(conversation)
}

func loadConversation() ([]anthropic.MessageParam, error) {
	var conversation []anthropic.MessageParam

	file, err := os.Open(conversationFile)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist, return empty slice
			return conversation, nil
		}
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&conversation)
	return conversation, err
}

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
			tgStream <- update
			// TODO: close the channel, when we get a context update
		}),
	}

	luffy, err := bot.New(tgBotToken, opts...)
	if err != nil {
		log.Panic(err)
		return
	}

	go luffy.Start(ctx)

	ai := anthropic.NewClient()
	conversation, err := loadConversation()
	if err != nil {
		log.Printf("Error loading conversation: %v", err)
		conversation = []anthropic.MessageParam{}
	} else {
		fmt.Printf("Loaded %d messages from previous session\n", len(conversation))
	}

	defer func() {
		if err := saveConversation(conversation); err != nil {
			log.Printf("Error saving conversation: %v", err)
		} else {
			fmt.Printf("Saved %d messages\n", len(conversation))
		}
	}()

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
				batchThreshold := 100 * time.Millisecond
				firstEdit := true
				for stream.Next() {
					event := stream.Current()
					err := message.Accumulate(event)
					if err != nil {
						panic(err)
					}

					now := time.Now()
					if firstEdit || now.Sub(lastSendTime) >= batchThreshold {
						if len(message.Content) > 0 && len(message.Content[0].Text) > 0 {
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
								Text:      message.Content[0].Text,
							})
							if err != nil {
								log.Panic(err)
								return
							}
							lastSendTime = now
						}
					}
				}
				conversation = append(conversation, message.ToParam())

				if stream.Err() != nil {
					log.Panic(stream.Err())
					continue
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

	// for update := range updates {
	// 	if update.Message != nil {
	// 		log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

	// 		chatID := update.Message.Chat.ID

	// 		tgMsg := tgbotapi.NewMessage(chatID, "")
	// 		tgMsg.ReplyToMessageID = update.Message.MessageID
	// 		sentMsg, err := bot.Send(tgMsg)
	// 		if err != nil {
	// 			log.Panic(err)
	// 			break
	//
	//
	// 		// // Make the call to the anthropic api servers
	// 		// message, err := ai.Messages.New(context.TODO(), anthropic.MessageNewParams{
	// 		// 	Model:     anthropic.ModelClaude4Sonnet20250514,
	// 		// 	MaxTokens: int64(1024),
	// 		// 	Messages:  conversation,
	// 		// 	System: systemPrompt,
	// 		// })
	// 		// if err != nil {
	// 		// 	log.Panic(err)
	// 		// }
	// 		// conversation = append(conversation, message.ToParam())

	// 		// for _, content := range message.Content {
	// 		// 	switch content.Type {
	// 		// 	case "text":
	// 		// 		msg := tgbotapi.NewMessage(update.Message.Chat.ID, content.Text)
	// 		// 		msg.ReplyToMessageID = update.Message.MessageID

	// 		// 		// Delete the thinking Message first
	// 		// 		deleteMsg := tgbotapi.NewDeleteMessage(msg.ChatID, sentMsg.MessageID)
	// 		// 		bot.Send(deleteMsg)

	// 		// 		bot.Send(msg)
	// 		// 	}
	// 		// }
	// 	}
	// }
}

func NewAgent(client *anthropic.Client, getUserMessage func() (string, bool)) *Agent {
	return &Agent{
		client:         client,
		getUserMessage: getUserMessage,
	}
}

type Agent struct {
	client         *anthropic.Client
	getUserMessage func() (string, bool)
}

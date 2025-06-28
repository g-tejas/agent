package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
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

// formatForTelegram converts markdown text to Telegram-compatible HTML format
func formatForTelegram(text string) string {
	// Store code blocks temporarily to avoid processing their content
	codeBlocks := make(map[string]string)
	codeBlockCounter := 0
	
	// Extract and temporarily replace code blocks
	codeBlockRegex := regexp.MustCompile("```([a-zA-Z]*)\n?((?s:.*?))\n?```")
	text = codeBlockRegex.ReplaceAllStringFunc(text, func(match string) string {
		parts := codeBlockRegex.FindStringSubmatch(match)
		if len(parts) >= 3 {
			language := parts[1]
			code := strings.TrimSpace(parts[2])
			
			// Escape HTML in code content
			code = strings.ReplaceAll(code, "&", "&amp;")
			code = strings.ReplaceAll(code, "<", "&lt;")
			code = strings.ReplaceAll(code, ">", "&gt;")
			
			placeholder := fmt.Sprintf("__CODEBLOCK_%d__", codeBlockCounter)
			if language != "" {
				codeBlocks[placeholder] = fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>", language, code)
			} else {
				codeBlocks[placeholder] = fmt.Sprintf("<pre>%s</pre>", code)
			}
			codeBlockCounter++
			return placeholder
		}
		return match
	})

	// Extract and temporarily replace inline code
	inlineCodes := make(map[string]string)
	inlineCodeCounter := 0
	inlineCodeRegex := regexp.MustCompile("`([^`]+)`")
	text = inlineCodeRegex.ReplaceAllStringFunc(text, func(match string) string {
		parts := inlineCodeRegex.FindStringSubmatch(match)
		if len(parts) >= 2 {
			code := parts[1]
			// Escape HTML in inline code
			code = strings.ReplaceAll(code, "&", "&amp;")
			code = strings.ReplaceAll(code, "<", "&lt;")
			code = strings.ReplaceAll(code, ">", "&gt;")
			
			placeholder := fmt.Sprintf("__INLINE_%d__", inlineCodeCounter)
			inlineCodes[placeholder] = fmt.Sprintf("<code>%s</code>", code)
			inlineCodeCounter++
			return placeholder
		}
		return match
	})

	// Now escape remaining HTML entities (after extracting code)
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// Convert markdown headers to bold text, but strip existing markdown formatting first
	headerRegex := regexp.MustCompile(`(?m)^#{1,6}\s*(.+)$`)
	text = headerRegex.ReplaceAllStringFunc(text, func(match string) string {
		parts := headerRegex.FindStringSubmatch(match)
		if len(parts) >= 2 {
			content := strings.TrimSpace(parts[1])
			// Strip markdown formatting from headers to avoid double conversion
			content = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(content, "$1")
			content = regexp.MustCompile(`__([^_]+)__`).ReplaceAllString(content, "$1")
			content = regexp.MustCompile(`\*([^*]+)\*`).ReplaceAllString(content, "$1")
			content = regexp.MustCompile(`~~([^~]+)~~`).ReplaceAllString(content, "$1")
			
			// Don't wrap empty or single character content
			if len(strings.TrimSpace(content)) > 1 {
				return fmt.Sprintf("<b>%s</b>", content)
			}
			return content
		}
		return match
	})

	// Convert **bold** to bold (for headers and section titles)
	boldRegex := regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	text = boldRegex.ReplaceAllString(text, "<b>$1</b>")

	// Convert __underline__ to underline (for key point emphasis)
	underlineRegex := regexp.MustCompile(`__([^_\n]+)__`)
	text = underlineRegex.ReplaceAllString(text, "<u>$1</u>")

	// Convert *italic* but be careful not to match ** patterns
	italicRegex := regexp.MustCompile(`(?<!\*)\*([^*\n]+)\*(?!\*)`)
	text = italicRegex.ReplaceAllString(text, "<i>$1</i>")

	// Convert strikethrough (if using ~~text~~)
	strikethroughRegex := regexp.MustCompile(`~~([^~\n]+)~~`)
	text = strikethroughRegex.ReplaceAllString(text, "<s>$1</s>")

	// Convert links
	linkRegex := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	text = linkRegex.ReplaceAllString(text, "<a href=\"$2\">$1</a>")

	// Restore code blocks
	for placeholder, code := range codeBlocks {
		text = strings.ReplaceAll(text, placeholder, code)
	}

	// Restore inline codes
	for placeholder, code := range inlineCodes {
		text = strings.ReplaceAll(text, placeholder, code)
	}

	// Clean up any malformed HTML or duplicate tags
	text = cleanupHTML(text)

	return text
}

// cleanupHTML fixes common HTML issues that could cause Telegram parsing errors
func cleanupHTML(html string) string {
	// Remove empty tags
	emptyTagRegex := regexp.MustCompile(`<(b|i|u|s|code)>\s*</\1>`)
	html = emptyTagRegex.ReplaceAllString(html, "")
	
	// Fix nested identical tags (e.g., <b><b>text</b></b> -> <b>text</b>)
	for _, tag := range []string{"b", "i", "u", "s", "code"} {
		nestedRegex := regexp.MustCompile(fmt.Sprintf(`<%s>(\s*<%s>(.+?)</%s>\s*)</%s>`, tag, tag, tag, tag))
		for nestedRegex.MatchString(html) {
			html = nestedRegex.ReplaceAllString(html, fmt.Sprintf(`<%s>$2</%s>`, tag, tag))
		}
	}
	
	// Remove orphaned single characters in tags (like <b>#</b>)
	orphanRegex := regexp.MustCompile(`<(b|i|u|s)>([^a-zA-Z0-9\s])</\1>`)
	html = orphanRegex.ReplaceAllString(html, "$2")
	
	// Clean up multiple consecutive whitespace while preserving single spaces
	whitespaceRegex := regexp.MustCompile(`\s+`)
	html = whitespaceRegex.ReplaceAllString(html, " ")
	
	return strings.TrimSpace(html)
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
							formattedText := formatForTelegram(message.Content[0].Text)
							
							// Only edit if the content has changed
							if formattedText != lastSentText {
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
									Text:      formattedText,
									ParseMode: models.ParseModeHTML,
								})
								if err != nil {
									log.Panic(err)
									return
								}
								lastSentText = formattedText
								lastSendTime = now
							}
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

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

type McpServer struct {
	Url             string
	Name            string
	AuthToken       string
	DefinitionParam anthropic.BetaRequestMCPServerURLDefinitionParam
}

func NewMcpServer(url string, name string, authToken string) *McpServer {
	return &McpServer{
		Url:       url,
		Name:      name,
		AuthToken: authToken,
		DefinitionParam: anthropic.BetaRequestMCPServerURLDefinitionParam{
			URL:                url,
			Name:               name,
			AuthorizationToken: param.NewOpt(authToken),
			ToolConfiguration: anthropic.BetaRequestMCPServerToolConfigurationParam{
				Enabled: anthropic.Bool(true),
			},
		},
	}
}

func (server *McpServer) GetDefinitionParam() anthropic.BetaRequestMCPServerURLDefinitionParam {
	return server.DefinitionParam
}

func (server *McpServer) DiscoverTools() (string, error) {
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]interface{}{},
	}

	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", server.Url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	if server.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+server.AuthToken)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err == nil {
		fmt.Println(prettyJSON.String())
	} else {
		fmt.Println(string(body))
	}

	return string(body), nil
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

	calendarMcpServer := NewMcpServer("https://mcp.zapier.com/api/mcp/mcp/", "Zapier", "")

	calendarMcpToolDefinitions, err := calendarMcpServer.DiscoverTools()
	if err != nil {
		log.Panic(err)
	}

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

	// Initialize Anthropic client with MCP beta support
	ai := anthropic.NewClient(option.WithHeader("anthropic-beta", anthropic.AnthropicBetaMCPClient2025_04_04))
	conversation := []anthropic.BetaMessageParam{}

	promptTxt, err := os.ReadFile(promptFile)
	if err != nil {
		log.Panic(err)
		return
	}
	fmt.Printf("Found system prompt %v: %v", promptFile, string(promptTxt))

	systemPromptText := string(promptTxt) + calendarMcpToolDefinitions
	systemPrompt := []anthropic.BetaTextBlockParam{
		{Text: systemPromptText},
	}

	for {
		select {
		case tgMsg := <-tgStream:
			pp.Println(tgMsg.Message.Text)
			if tgMsg.Message != nil {
				stats.tgMsgsReceived += 1

				text := tgMsg.Message.Text
				chatID := tgMsg.Message.Chat.ID

				userMessage := anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text))
				conversation = append(conversation, userMessage)
				log.Printf("Added user message to conversation. Total messages: %d", len(conversation))

				sentMsg, err := luffy.SendMessage(ctx, &bot.SendMessageParams{
					ChatID: chatID,
					Text:   "🤔 Luffy is thinking...",
				})
				if err != nil {
					return
				}

				// Debug: Log conversation before sending to API
				log.Printf("Sending %d messages to API", len(conversation))
				for i, msg := range conversation {
					log.Printf("Message %d: role=%s", i, msg.Role)
				}

				// Use Beta streaming API with MCP support
				stream := ai.Beta.Messages.NewStreaming(ctx, anthropic.BetaMessageNewParams{
					Model:     anthropic.ModelClaude4Sonnet20250514,
					MaxTokens: int64(1024),
					Messages:  conversation,
					System:    systemPrompt,
					MCPServers: []anthropic.BetaRequestMCPServerURLDefinitionParam{
						calendarMcpServer.GetDefinitionParam(),
					},
				})

				message := anthropic.BetaMessage{}
				lastSendTime := time.Now()
				batchThreshold := 1 * time.Second
				firstEdit := true
				lastSentText := ""

				var allTextContent strings.Builder
				toolsExecuted := false

				for stream.Next() {
					event := stream.Current()
					err := message.Accumulate(event)
					if err != nil {
						log.Printf("Error accumulating event: %v", err)
						continue
					}

					// Log all events for debugging
					switch eventVariant := event.AsAny().(type) {
					case anthropic.BetaRawContentBlockDeltaEvent:
						switch deltaVariant := eventVariant.Delta.AsAny().(type) {
						case anthropic.BetaTextDelta:
							// Append new text to our running content
							allTextContent.WriteString(deltaVariant.Text)

							now := time.Now()
							if firstEdit || now.Sub(lastSendTime) >= batchThreshold {
								currentText := allTextContent.String()

								// Only edit if the content has changed
								if currentText != lastSentText && currentText != "" {
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
											log.Printf("Error setting reaction: %v", err)
										}
										firstEdit = false
									}

									_, err = luffy.EditMessageText(ctx, &bot.EditMessageTextParams{
										ChatID:    chatID,
										MessageID: sentMsg.ID,
										Text:      currentText,
									})
									if err != nil {
										log.Printf("Error editing message: %v", err)
									} else {
										lastSentText = currentText
										lastSendTime = now
									}
								}
							}
						}
					case anthropic.BetaRawMessageStartEvent:
						log.Printf("🚀 Message start event")
					case anthropic.BetaRawMessageStopEvent:
						log.Printf("🏁 Message stop event - final message ready")
					case anthropic.BetaRawContentBlockStartEvent:
						switch blockType := eventVariant.ContentBlock.AsAny().(type) {
						case anthropic.BetaToolUseBlock:
							log.Printf("🔧 Tool use block started: %s (ID: %s)", blockType.Name, blockType.ID)
						case anthropic.BetaTextBlock:
							log.Printf("📝 Text block started")
						default:
							log.Printf("📦 Content block start: %T", blockType)
						}
					case anthropic.BetaRawContentBlockStopEvent:
						log.Printf("Content block stop")
						// Check if this was a tool use block
						if len(message.Content) > 0 {
							for i, content := range message.Content {
								switch contentType := content.AsAny().(type) {
								case anthropic.BetaToolUseBlock:
									toolsExecuted = true
									log.Printf("🔧 Tool use detected - Block %d: %s", i, contentType.Name)
									log.Printf("🔧 Tool ID: %s", contentType.ID)
									log.Printf("🔧 Tool Input Parameters:")
									if inputBytes, err := json.MarshalIndent(contentType.Input, "    ", "  "); err == nil {
										log.Printf("    %s", string(inputBytes))
									} else {
										log.Printf("🔧 Tool Input: %+v", contentType.Input)
									}
								case anthropic.BetaTextBlock:
									log.Printf("📝 Text block %d: %d characters", i, len(contentType.Text))
								default:
									log.Printf("❓ Unknown content block %d type: %T", i, contentType)
								}
							}
						}
					default:
						log.Printf("Other event: %T", eventVariant)
					}
				}

				if stream.Err() != nil {
					log.Printf("Stream error: %v", stream.Err())
					continue
				}

				// Debug: Log the message state
				log.Printf("Message accumulated. Content blocks: %d, Tools executed: %v", len(message.Content), toolsExecuted)

				// Get final response text - use accumulated text content
				finalText := allTextContent.String()
				log.Printf("Accumulated text length: %d", len(finalText))

				// If tools were executed, we might have additional content in message blocks
				if len(message.Content) > 0 {
					var completeText strings.Builder
					var toolResults []string

					for i, content := range message.Content {
						switch contentType := content.AsAny().(type) {
						case anthropic.BetaTextBlock:
							log.Printf("📝 Final text block %d: %d chars", i, len(contentType.Text))
							completeText.WriteString(contentType.Text)
						case anthropic.BetaToolUseBlock:
							log.Printf("🔧 Final tool use block %d: %s (ID: %s)", i, contentType.Name, contentType.ID)
						default:
							log.Printf("❓ Final unknown block %d: %T", i, contentType)
						}
					}

					// Use complete text if it's longer than our accumulated version
					completeTextStr := completeText.String()
					if len(completeTextStr) > len(finalText) {
						finalText = completeTextStr
						log.Printf("Using complete message text length: %d", len(finalText))
					}

					// Log tool results summary
					if len(toolResults) > 0 {
						log.Printf("🔧 Tool results summary: %d results", len(toolResults))
						for i, result := range toolResults {
							log.Printf("🔧   Result %d: %.200s...", i, result)
						}
					}
				}

				// Fallback to lastSentText if finalText is still empty
				if finalText == "" {
					finalText = lastSentText
					log.Printf("Using lastSentText as fallback, length: %d", len(finalText))
				}

				// Add response to conversation (only if we have content)
				if finalText != "" {
					// Create a proper beta assistant message
					assistantMessage := anthropic.BetaMessageParam{
						Role:    anthropic.BetaMessageParamRoleAssistant,
						Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(finalText)},
					}
					conversation = append(conversation, assistantMessage)
					log.Printf("Added assistant message to conversation")
				}

				// Only send final message if we have content and it's different from what's already shown
				if finalText != "" && finalText != lastSentText {
					log.Printf("Sending final message update")
					_, err = luffy.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    chatID,
						MessageID: sentMsg.ID,
						Text:      finalText,
						ParseMode: models.ParseModeHTML,
					})
					if err != nil {
						log.Printf("Error editing final message: %v", err)
					}
				} else if finalText == lastSentText {
					log.Printf("Final text same as last sent, skipping update")
				} else {
					// If no content was generated, send a fallback message
					log.Printf("No content generated, sending fallback message")
					_, err = luffy.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    chatID,
						MessageID: sentMsg.ID,
						Text:      "🤖 I encountered an issue processing your request. Please try again.",
					})
					if err != nil {
						log.Printf("Error editing fallback message: %v", err)
					}
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

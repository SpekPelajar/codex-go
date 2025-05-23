package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/epuerta/codex-go/internal/config"
	"github.com/epuerta/codex-go/internal/logging"
	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
)

// ToolDefinition represents a tool that can be called by the AI
type ToolDefinition struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef represents a function definition
type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// OpenAIAgent implements the Agent interface using OpenAI
type OpenAIAgent struct {
	client           *openai.Client
	config           *config.Config
	tools            []ToolDefinition
	currentContext   context.Context
	cancelFunc       context.CancelFunc
	sessionID        string
	history          *ConversationHistory
	historyOpts      HistoryOptions
	mu               sync.Mutex
	currentHandler   ResponseHandler
	pendingToolCalls map[string]bool // Map of CallID -> true (pending)
	pendingMu        sync.Mutex      // Mutex for pendingToolCalls map
	logger           logging.Logger
}

// NewOpenAIAgent creates a new OpenAI agent
func NewOpenAIAgent(cfg *config.Config, logger logging.Logger) (*OpenAIAgent, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("OpenAI API key is required")
	}

	clientConfig := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		clientConfig.BaseURL = cfg.BaseURL
	}

	client := openai.NewClientWithConfig(clientConfig)

	// Generate a session ID
	sessionID := uuid.New().String()

	// Create history options
	historyOpts := DefaultHistoryOptions()
	historyOpts.SessionID = sessionID

	// Load instructions from config if available
	if cfg.Instructions != "" {
		historyOpts.SystemPrompt = cfg.Instructions
	}

	// Initialize conversation history
	history, err := NewConversationHistory(historyOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize conversation history: %w", err)
	}

	// Default tools
	tools := []ToolDefinition{
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "shell",
				Description: "Execute a shell command",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{
							"type":        "string",
							"description": "The shell command to execute",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "read_file",
				Description: "Read the contents of a file",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "The path to the file",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "write_file",
				Description: "Write content to a file, replacing existing content or creating a new file. Use patch_file for modifying existing files.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "The path to the file",
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The full content to write",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "patch_file",
				Description: "Modify an existing file by applying a patch in a specific format. Preferred for edits over write_file.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						// Note: The actual implementation uses a custom format, not standard diff args.
						// Describe the expected custom format in the parameter description.
						"patch_content": map[string]interface{}{
							"type":        "string",
							"description": "The patch content, including // FILE:, // EDIT:, // END_EDIT, ADD:, and DEL: markers.",
						},
					},
					"required": []string{"patch_content"},
				},
			},
		},
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "list_directory",
				Description: "List the contents of a directory",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "The path to the directory",
						},
					},
					"required": []string{"path"},
				},
			},
		},
	}

	// If logger is nil, use a nil logger to avoid null pointer issues
	if logger == nil {
		logger = &logging.NilLogger{}
	}

	// Create agent
	agent := &OpenAIAgent{
		client:           client,
		config:           cfg,
		tools:            tools,
		sessionID:        sessionID,
		history:          history,
		historyOpts:      historyOpts,
		logger:           logger,
		pendingToolCalls: make(map[string]bool), // Initialize the map
	}

	return agent, nil
}

// SendMessage sends a message to OpenAI and streams the response
// It returns true if the stream finished requesting tool calls, false otherwise.
func (a *OpenAIAgent) SendMessage(ctx context.Context, messages []Message, handler ResponseHandler) (bool, error) {
	a.mu.Lock()
	// Cancel any ongoing request
	if a.cancelFunc != nil {
		a.logger.Log("[DEBUG] Agent.SendMessage: Cancelling previous context/request.")
		a.cancelFunc()
	}

	// Store the handler for potential follow-up calls
	a.currentHandler = handler

	// Create a new context with cancellation
	a.currentContext, a.cancelFunc = context.WithCancel(ctx)
	a.mu.Unlock() // Unlock main mutex early

	// --- BEGIN CANCELLATION HANDLING ---
	var abortedToolResults []Message
	a.pendingMu.Lock()
	if len(a.pendingToolCalls) > 0 {
		a.logger.Log("[INFO] Agent.SendMessage: Found %d pending tool calls from previous cancelled interaction.", len(a.pendingToolCalls))
		for callID := range a.pendingToolCalls {
			abortedResultContent := map[string]interface{}{"error": "execution cancelled by user"}
			// We might not know the function name here, but ToolCallID is the important part
			abortedToolResults = append(abortedToolResults, Message{
				Role:       openai.ChatMessageRoleTool,
				Content:    string(mustMarshal(abortedResultContent)),
				ToolCallID: callID,
				// Name:       "unknown_cancelled_function", // Or leave empty
			})
			a.logger.Log("[DEBUG] Agent.SendMessage: Created aborted result for CallID %s", callID)
		}
		// Clear the pending map after processing
		a.pendingToolCalls = make(map[string]bool)
		a.logger.Log("[DEBUG] Agent.SendMessage: Cleared pendingToolCalls map.")
	}
	a.pendingMu.Unlock()

	// Add the aborted results AND the new user messages to history
	if len(abortedToolResults) > 0 {
		a.history.AddMessages(abortedToolResults) // Add aborted results first
		a.logger.Log("[DEBUG] Agent.SendMessage: Added %d aborted tool results to history.", len(abortedToolResults))
	}
	if len(messages) > 0 {
		a.history.AddMessages(messages) // Then add the new user message(s)
		a.logger.Log("[DEBUG] Agent.SendMessage: Added %d new message(s) from user to history.", len(messages))
	}
	// --- END CANCELLATION HANDLING ---

	// Get context-aware messages from history
	historyMessages := a.history.GetMessagesForContext()

	// Convert messages to OpenAI format
	var openAIMessages []openai.ChatCompletionMessage
	for _, msg := range historyMessages {
		// Create the base message
		apiMsg := openai.ChatCompletionMessage{
			Role:    msg.Role,
			Content: msg.Content, // Content is used for user, system, assistant (text), tool (result JSON)
		}

		// Handle Assistant requesting tool calls
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0 {
			apiMsg.ToolCalls = make([]openai.ToolCall, len(msg.ToolCalls))
			for i, tc := range msg.ToolCalls {
				apiMsg.ToolCalls[i] = openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolType(tc.Type), // Assuming type is compatible (e.g., "function")
					Function: openai.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
			// Content might be empty or null when tool calls are present
			apiMsg.Content = "" // Or check if msg.Content should be preserved
		}

		// Handle Tool results
		if msg.Role == openai.ChatMessageRoleTool {
			apiMsg.ToolCallID = msg.ToolCallID
		}

		openAIMessages = append(openAIMessages, apiMsg)
	}

	// --- ADD LOGGING ---
	historyForAPILog, _ := json.MarshalIndent(openAIMessages, "", "  ")
	a.logger.Log("[DEBUG] Agent.SendMessage: History being sent to API:\n%s", string(historyForAPILog))
	// --- END LOGGING ---

	// Create the request
	req := openai.ChatCompletionRequest{
		Model:       a.config.Model,
		Messages:    openAIMessages,
		Temperature: 0.7,
		Tools:       convertToolDefinitions(a.tools),
		Stream:      true,
	}

	// Start thinking timer
	startTime := time.Now()

	a.logger.Log("[DEBUG] Agent.SendMessage: Creating stream request...")
	stream, err := a.client.CreateChatCompletionStream(a.currentContext, req)
	if err != nil {
		a.logger.Log("[ERROR] Agent.SendMessage: Error creating stream: %v", err)
		return false, fmt.Errorf("error creating chat completion stream: %w", err) // Return false on error
	}
	defer stream.Close()
	a.logger.Log("[DEBUG] Agent.SendMessage: Stream created successfully. Starting Recv() loop.")

	accumulatingToolCalls := make(map[string]*openai.FunctionCall)
	var currentContent string
	currentRole := openai.ChatMessageRoleAssistant
	streamEndedWithToolCall := false // Flag
	processingToolCall := false      // NEW Flag: Set to true once any tool delta is received

	// Process the stream
	for {
		a.logger.Log("[DEBUG] Agent.SendMessage: Calling stream.Recv()...")
		response, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				a.logger.Log("[DEBUG] Agent.SendMessage: Received EOF from stream.")
				break // Exit loop on EOF
			}
			a.logger.Log("[ERROR] Agent.SendMessage: Error receiving from stream: %v", err)
			return false, fmt.Errorf("error receiving from stream: %w", err) // Return false on error
		}
		a.logger.Log("[DEBUG] Agent.SendMessage: stream.Recv() successful. Choices: %d", len(response.Choices))

		if len(response.Choices) > 0 {
			choice := response.Choices[0]
			a.logger.Log("[DEBUG] Agent.SendMessage: Processing choice 0. Delta Content: %t, Delta ToolCalls: %t, FinishReason: %s", choice.Delta.Content != "", choice.Delta.ToolCalls != nil, choice.FinishReason)

			if choice.Delta.Role != "" {
				currentRole = choice.Delta.Role
			}

			// --- Check if we are starting to process tool calls ---
			if choice.Delta.ToolCalls != nil && len(choice.Delta.ToolCalls) > 0 {
				if !processingToolCall {
					a.logger.Log("[DEBUG] Agent.SendMessage: Detected first tool call delta. Switching to tool call processing mode.")
					processingToolCall = true
					// Optional: Clear any potentially accumulated 'currentContent' when tool calls start?
					// currentContent = ""
				}
			}

			// --- Process Delta Content ONLY if NOT in tool call mode ---
			if choice.Delta.Content != "" && !processingToolCall {
				currentContent += choice.Delta.Content
				// Send message update to handler for real-time display
				// We send the update regardless of tool calls now,
				// because the *history* addition is handled *after* the loop based on finish_reason.
				a.logger.Log("[DEBUG] Agent.SendMessage: Calling handler with type 'message' update. Current content length: %d", len(currentContent))
				itemToSend := ResponseItem{
					Type: "message",
					Message: &Message{
						Role:    currentRole,
						Content: currentContent,
					},
					ThinkingDuration: time.Since(startTime).Milliseconds(),
				}
				jsonData, err := json.Marshal(itemToSend)
				if err == nil {
					handler(string(jsonData))
				}
			} else if choice.Delta.Content != "" && processingToolCall {
				a.logger.Log("[DEBUG] Agent.SendMessage: Ignoring delta content because we are processing tool calls.")
			}

			// --- Accumulate Tool Calls if in tool call mode ---
			if processingToolCall && choice.Delta.ToolCalls != nil {
				streamEndedWithToolCall = true // Mark that we are processing tool calls
				a.logger.Log("[DEBUG] Agent.SendMessage: Processing Delta.ToolCalls.")
				for _, toolCallChunk := range choice.Delta.ToolCalls {
					if toolCallChunk.ID == "" {
						continue
					}
					if _, exists := accumulatingToolCalls[toolCallChunk.ID]; !exists {
						a.logger.Log("[DEBUG] Agent.SendMessage: Initializing new tool call buffer for ID: %s", toolCallChunk.ID)
						accumulatingToolCalls[toolCallChunk.ID] = &openai.FunctionCall{Name: toolCallChunk.Function.Name}
					}
					if toolCallChunk.Function.Arguments != "" {
						a.logger.Log("[DEBUG] Agent.SendMessage: Appending arguments chunk '%s' to tool call ID: %s", toolCallChunk.Function.Arguments, toolCallChunk.ID)
						accumulatingToolCalls[toolCallChunk.ID].Arguments += toolCallChunk.Function.Arguments
					}
				}
			}

			// --- Check FinishReason and Send Function Calls to Handler ---
			if choice.FinishReason != "" {
				if choice.FinishReason == "tool_calls" {
					streamEndedWithToolCall = true // Confirm flag
					a.logger.Log("[DEBUG] Agent.SendMessage: FinishReason is 'tool_calls'. Sending function calls to handler.")

					// Send function call items to handler IMMEDIATELY
					for id, completedCall := range accumulatingToolCalls {
						functionCall := &FunctionCall{
							Name:      completedCall.Name,
							Arguments: completedCall.Arguments,
							ID:        id,
						}
						// Track pending call
						a.pendingMu.Lock()
						if a.pendingToolCalls == nil {
							a.pendingToolCalls = make(map[string]bool)
						}
						a.pendingToolCalls[id] = true
						a.logger.Log("[DEBUG] Agent.SendMessage: Added CallID %s to pendingToolCalls", id)
						a.pendingMu.Unlock()

						a.logger.Log("[DEBUG] Agent.SendMessage: Calling handler with type 'function_call'. Name: %s, Args: '%s', ID: %s", functionCall.Name, functionCall.Arguments, functionCall.ID)
						itemToSend := ResponseItem{
							Type:             "function_call",
							FunctionCall:     &FunctionCall{Name: functionCall.Name, Arguments: functionCall.Arguments, ID: functionCall.ID},
							ThinkingDuration: time.Since(startTime).Milliseconds(),
						}
						jsonData, err := json.Marshal(itemToSend)
						if err == nil {
							handler(string(jsonData))
							a.logger.Log("[DEBUG] Agent.SendMessage: Sent function_call item as JSON string.")
						}
					}
					// DO NOT add to history here. History is added AFTER the loop.
				} else {
					// Handle non-tool_call finish reasons (e.g., 'stop')
					a.logger.Log("[DEBUG] Agent.SendMessage: FinishReason is '%s'.", choice.FinishReason)
					// History addition happens after the loop based on streamEndedWithToolCall flag.
				}
			}
		}
	} // End stream processing loop

	a.logger.Log("[DEBUG] Agent.SendMessage: Exited Recv() loop.")

	// --- Add Final Assistant Message to History AFTER loop ---
	if a.history != nil {
		if streamEndedWithToolCall {
			// Add assistant message with ONLY tool calls
			assistantMsgToolCalls := []ToolCall{}
			for id, completedCall := range accumulatingToolCalls {
				args := completedCall.Arguments
				if args == "" {
					args = "{}"
				}
				assistantMsgToolCalls = append(assistantMsgToolCalls, ToolCall{
					ID:   id,
					Type: string(openai.ToolTypeFunction),
					Function: FunctionCall{
						Name:      completedCall.Name,
						Arguments: args,
					},
				})
			}
			if len(assistantMsgToolCalls) > 0 { // Only add if there were actual tool calls requested
				assistantMsg := Message{
					Role:      openai.ChatMessageRoleAssistant,
					ToolCalls: assistantMsgToolCalls,
					Content:   "", // Explicitly empty content
				}
				a.history.AddMessage(assistantMsg)
				a.logger.Log("[DEBUG] Agent.SendMessage: Added final assistant message (ToolCalls only) to history.")
			} else {
				a.logger.Log("[WARN] Agent.SendMessage: Stream ended with tool_calls reason, but no tool calls were accumulated.")
			}
		} else if currentContent != "" {
			// Add assistant message with ONLY text content
			assistantMsg := Message{
				Role:    currentRole, // Should be assistant
				Content: currentContent,
			}
			a.history.AddMessage(assistantMsg)
			a.logger.Log("[DEBUG] Agent.SendMessage: Added final assistant message (Text only) to history.")
		}
	} else {
		a.logger.Log("[ERROR] Agent.SendMessage: History is nil when trying to add final assistant message.")
	}

	a.logger.Log("[DEBUG] Agent.SendMessage: Function returning. Stream ended with tool call: %t", streamEndedWithToolCall)
	return streamEndedWithToolCall, nil // Return the flag and nil error
}

// SendFileChange sends a file change to the AI for approval
func (a *OpenAIAgent) SendFileChange(ctx context.Context, filePath string, diff string) (*FileChangeConfirmation, error) {
	// In a real implementation, this would send the diff to the AI for approval
	// For now, we'll just return an automatic approval
	return &FileChangeConfirmation{
		Approved: true,
	}, nil
}

// GetCommandConfirmation gets user confirmation for a command
func (a *OpenAIAgent) GetCommandConfirmation(ctx context.Context, command string, args []string) (*CommandConfirmation, error) {
	// In a real implementation, this would get confirmation from the user or AI
	// For now, we'll just return an automatic approval
	return &CommandConfirmation{
		Approved: true,
	}, nil
}

// Cancel cancels the current streaming response and marks pending tool calls for abort handling.
func (a *OpenAIAgent) Cancel() {
	a.mu.Lock() // Lock main mutex for cancelFunc
	if a.cancelFunc != nil {
		a.logger.Log("[DEBUG] Agent.Cancel: Calling context cancelFunc().")
		a.cancelFunc()
		a.cancelFunc = nil // Prevent repeated calls
	} else {
		a.logger.Log("[DEBUG] Agent.Cancel: No active context cancelFunc to call.")
	}
	a.mu.Unlock()

	// Note: We don't clear pendingToolCalls here. The map now correctly represents
	// calls that were issued but whose results were not processed before cancellation.
	// The SendMessage function will handle clearing them on the *next* interaction.
	a.logger.Log("[DEBUG] Agent.Cancel: Cancellation requested. Pending tool calls will be handled on next SendMessage.")
}

// Close closes the agent and releases any resources
func (a *OpenAIAgent) Close() error {
	a.Cancel()

	// Save history before closing
	if a.history != nil {
		a.history.Save(a.historyOpts.HistoryPath)
	}

	return nil
}

// ClearHistory clears the conversation history
func (a *OpenAIAgent) ClearHistory() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.history != nil {
		a.history.Clear()
		a.history.Save(a.historyOpts.HistoryPath)
	}
}

// GetHistory returns the conversation history
func (a *OpenAIAgent) GetHistory() *ConversationHistory {
	return a.history
}

// SendFunctionResult adds the tool result to history and then triggers the next AI response stream.
func (a *OpenAIAgent) SendFunctionResult(ctx context.Context, callID, functionName, output string, success bool) error {
	a.mu.Lock()
	// Get the handler before potentially unlocking in defer
	handler := a.currentHandler
	a.mu.Unlock()

	a.logger.Log("[DEBUG] Agent.SendFunctionResult: Received result for CallID: %s, Name: %s, Success: %t", callID, functionName, success)

	// --- BEGIN Remove from Pending Tool Calls ---
	a.pendingMu.Lock()
	if _, exists := a.pendingToolCalls[callID]; exists {
		delete(a.pendingToolCalls, callID)
		a.logger.Log("[DEBUG] Agent.SendFunctionResult: Removed CallID %s from pendingToolCalls", callID)
	} else {
		// This might happen if SendFunctionResult is called unexpectedly or after a cancellation was already processed.
		a.logger.Log("[WARN] Agent.SendFunctionResult: CallID %s not found in pendingToolCalls when trying to remove.", callID)
	}
	a.pendingMu.Unlock()
	// --- END Remove from Pending Tool Calls ---

	// 1. Create the tool result message to add to history
	var content map[string]interface{}
	if success {
		content = map[string]interface{}{"output": output}
	} else {
		content = map[string]interface{}{"error": output}
	}
	// Create the Tool Result message part
	toolResultMessage := Message{
		Role:       openai.ChatMessageRoleTool,
		Content:    string(json.RawMessage(mustMarshal(content))), // Ensure content is valid JSON string
		ToolCallID: callID,
		Name:       functionName,
	}

	if a.history != nil {
		// Add ONLY the tool result message to history. The assistant message
		// with the tool call request is already present from SendMessage.
		a.history.AddMessage(toolResultMessage)
		a.logger.Log("[DEBUG] Agent.SendFunctionResult: Tool result message added to history.")
	} else {
		a.logger.Log("[ERROR] Agent.SendFunctionResult: History is nil, cannot add tool result message.")
		return fmt.Errorf("agent history is nil") // Return error if history doesn't exist
	}

	// 2. Check if a handler is available (meaning SendMessage is waiting)
	if handler == nil {
		a.logger.Log("[WARN] Agent.SendFunctionResult: No current handler available to send follow-up request.")
		// This might happen if the original SendMessage context was cancelled
		return nil // Or return an error?
	}

	// 3. Prepare and send the follow-up request to OpenAI
	a.logger.Log("[DEBUG] Agent.SendFunctionResult: Preparing follow-up OpenAI request.")
	historyMessages := a.history.GetMessagesForContext()
	var openAIMessages []openai.ChatCompletionMessage

	// --- FILTERING HISTORY FOR API ---
	// Ensure the sequence Assistant(ToolCall) -> Tool(Result) is strictly maintained
	// Skip any intermediate Assistant(Content) messages.
	toolCallIDsExpected := make(map[string]bool) // Keep track of tool IDs we expect results for

	for i := 0; i < len(historyMessages); i++ {
		msg := historyMessages[i]
		apiMsg := openai.ChatCompletionMessage{
			Role:    msg.Role,
			Content: msg.Content, // May be overridden below
		}
		addMsg := true // Flag to control if we add the message

		if msg.Role == openai.ChatMessageRoleAssistant {
			if len(msg.ToolCalls) > 0 {
				// This is an assistant message requesting tool calls
				apiMsg.ToolCalls = make([]openai.ToolCall, len(msg.ToolCalls))
				for j, tc := range msg.ToolCalls {
					apiMsg.ToolCalls[j] = openai.ToolCall{
						ID:   tc.ID,
						Type: openai.ToolType(tc.Type),
						Function: openai.FunctionCall{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
					// Mark this tool call ID as expected
					toolCallIDsExpected[tc.ID] = true
				}
				apiMsg.Content = "" // Content MUST be empty/null when tool calls are present
			} else if len(toolCallIDsExpected) > 0 {
				// This is a text message from the assistant, BUT we are still expecting tool results.
				// This is the message we need to SKIP.
				a.logger.Log("[DEBUG] Agent.SendFunctionResult: Skipping assistant text message (Role: %s, Content: %d chars) because tool results are pending.", msg.Role, len(msg.Content))
				addMsg = false
			}
			// Otherwise, it's a normal assistant text message when no tool calls are pending - keep it.
		}

		if msg.Role == openai.ChatMessageRoleTool {
			// This is a tool result message
			apiMsg.ToolCallID = msg.ToolCallID
			// Mark this tool call ID as fulfilled
			if _, exists := toolCallIDsExpected[msg.ToolCallID]; exists {
				delete(toolCallIDsExpected, msg.ToolCallID)
				a.logger.Log("[DEBUG] Agent.SendFunctionResult: Matched Tool Result for ID %s.", msg.ToolCallID)
			} else {
				// This shouldn't normally happen if history is consistent
				a.logger.Log("[WARN] Agent.SendFunctionResult: Encountered Tool Result for unexpected ID %s.", msg.ToolCallID)
			}
		}

		if addMsg {
			openAIMessages = append(openAIMessages, apiMsg)
		}
	}
	// --- END FILTERING ---

	// --- ADD LOGGING ---
	historyForAPILog, _ := json.MarshalIndent(openAIMessages, "", "  ")
	a.logger.Log("[DEBUG] Agent.SendFunctionResult: Filtered History being sent to API:\n%s", string(historyForAPILog))
	// --- END LOGGING ---

	req := openai.ChatCompletionRequest{
		Model:       a.config.Model,
		Messages:    openAIMessages,
		Temperature: 0.7,
		Tools:       convertToolDefinitions(a.tools),
		Stream:      true,
	}

	a.logger.Log("[DEBUG] Agent.SendFunctionResult: Making follow-up CreateChatCompletionStream call.")
	stream, err := a.client.CreateChatCompletionStream(ctx, req) // Use the passed context
	if err != nil {
		a.logger.Log("[ERROR] Agent.SendFunctionResult: Error creating follow-up stream: %v", err)
		// Should we maybe inform the handler of this error?
		// For now, just return the error.
		return fmt.Errorf("error creating follow-up chat completion stream: %w", err)
	}
	defer stream.Close()

	// 4. Process the new stream, sending results back via the original handler
	a.logger.Log("[DEBUG] Agent.SendFunctionResult: Processing follow-up stream...")
	startTime := time.Now() // Reset start time for this response phase
	var currentContent string
	currentRole := openai.ChatMessageRoleAssistant // Expecting assistant response now
	var currentFunctionCall *openai.FunctionCall   // Added for potential nested calls
	var currentFunctionCallID string               // Added for potential nested calls

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			a.logger.Log("[DEBUG] Agent.SendFunctionResult: Received EOF from follow-up stream.")
			break
		}
		if err != nil {
			a.logger.Log("[ERROR] Agent.SendFunctionResult: Error receiving from follow-up stream: %v", err)
			// Inform handler?
			return fmt.Errorf("error receiving from follow-up stream: %w", err)
		}

		if len(response.Choices) > 0 {
			choice := response.Choices[0]
			a.logger.Log("[DEBUG] Agent.SendFunctionResult: Processing choice 0. Delta Content: %t, Delta ToolCalls: %t, FinishReason: %s", choice.Delta.Content != "", choice.Delta.ToolCalls != nil, choice.FinishReason)

			// Handle delta content (for text response)
			if choice.Delta.Content != "" {
				currentContent += choice.Delta.Content
				a.logger.Log("[DEBUG] Agent.SendFunctionResult: Calling handler with type 'message'. Current content length: %d", len(currentContent))
				itemToSend := ResponseItem{
					Type: "message",
					Message: &Message{
						Role:    currentRole,
						Content: currentContent,
					},
					ThinkingDuration: time.Since(startTime).Milliseconds(),
				}
				jsonData, err := json.Marshal(itemToSend)
				if err != nil {
					a.logger.Log("[ERROR] Agent.SendFunctionResult: Failed to marshal message item: %v", err)
				} else {
					handler(string(jsonData))
				}
			}

			// Handle accumulating tool calls data (for potential recursive calls)
			if choice.Delta.ToolCalls != nil && len(choice.Delta.ToolCalls) > 0 {
				a.logger.Log("[DEBUG] Agent.SendFunctionResult: Processing Delta.ToolCalls (nested).")
				toolCall := choice.Delta.ToolCalls[0]

				if currentFunctionCall == nil {
					a.logger.Log("[DEBUG] Agent.SendFunctionResult: Initializing new function call (nested). Name: %s, ID: %s", toolCall.Function.Name, toolCall.ID)
					currentFunctionCall = &openai.FunctionCall{
						Name:      toolCall.Function.Name,
						Arguments: toolCall.Function.Arguments,
					}
					currentFunctionCallID = toolCall.ID
				} else {
					a.logger.Log("[DEBUG] Agent.SendFunctionResult: Appending to existing function call arguments (nested).")
					currentFunctionCall.Arguments += toolCall.Function.Arguments
				}
			}

			// Check for FinishReason SEPARATELY (for potential recursive calls)
			if choice.FinishReason == "tool_calls" && currentFunctionCall != nil {
				a.logger.Log("[DEBUG] Agent.SendFunctionResult: FinishReason is 'tool_calls' (nested). Preparing function call item.")

				// --- BEGIN FIX: Add Assistant message for nested tool call ---
				nestedToolCalls := []ToolCall{
					{
						ID:   currentFunctionCallID,
						Type: string(openai.ToolTypeFunction), // Assuming function
						Function: FunctionCall{
							Name:      currentFunctionCall.Name,
							Arguments: currentFunctionCall.Arguments, // Already accumulated
						},
					},
				}
				// Add this assistant message to history NOW
				if a.history != nil {
					a.history.AddMessage(Message{
						Role:      openai.ChatMessageRoleAssistant,
						ToolCalls: nestedToolCalls,
					})
					a.logger.Log("[DEBUG] Agent.SendFunctionResult: Added assistant message with NESTED ToolCalls to history.")
				} else {
					a.logger.Log("[ERROR] Agent.SendFunctionResult: History is nil, cannot add nested assistant message with ToolCalls.")
				}
				// --- END FIX ---

				functionCall := &FunctionCall{ // Prepare item for handler
					Name:      currentFunctionCall.Name,
					Arguments: currentFunctionCall.Arguments,
					ID:        currentFunctionCallID,
				}

				a.logger.Log("[DEBUG] Agent.SendFunctionResult: Calling handler with type 'function_call' (nested). Name: %s, Args: '%s', ID: %s", functionCall.Name, functionCall.Arguments, functionCall.ID)
				itemToSend := ResponseItem{
					Type:             "function_call",
					FunctionCall:     &FunctionCall{Name: functionCall.Name, Arguments: functionCall.Arguments, ID: functionCall.ID},
					ThinkingDuration: time.Since(startTime).Milliseconds(),
				}
				// Marshal and send JSON string via handler
				jsonData, err := json.Marshal(itemToSend)
				if err != nil {
					a.logger.Log("[ERROR] Agent.SendFunctionResult: Failed to marshal function_call item: %v", err)
					// Consider sending an error message back to the app
				} else {
					handler(string(jsonData))
					a.logger.Log("[DEBUG] Agent.SendFunctionResult: Sent function_call item as JSON string.")
				}

				// Reset for next potential call in this stream
				currentFunctionCall = nil
				currentFunctionCallID = ""
			}
		}
	}

	a.logger.Log("[DEBUG] Agent.SendFunctionResult: Follow-up stream processing finished.")
	// Add the final assistant message from this stream to history
	if currentContent != "" {
		if a.history != nil {
			a.history.AddMessage(Message{
				Role:    currentRole,
				Content: currentContent,
			})
			a.logger.Log("[DEBUG] Agent.SendFunctionResult: Added final assistant message to history.")
		}
	}

	// --- FIX: Signal completion of the follow-up stream ---
	// If we finished processing the stream and the last action wasn't requesting another tool call,
	// signal completion back to the App.
	if currentFunctionCall == nil { // If we are not expecting another tool call
		a.logger.Log("[DEBUG] Agent.SendFunctionResult: Follow-up stream finished without further tool calls. Sending completion signal.")
		// Use the handler to send the new completion message
		completionItem := ResponseItem{Type: "followup_complete"} // Use a unique type
		jsonData, err := json.Marshal(completionItem)
		if err != nil {
			a.logger.Log("[ERROR] Agent.SendFunctionResult: Failed to marshal followup_complete item: %v", err)
		} else {
			handler(string(jsonData))
		}
	} else {
		a.logger.Log("[DEBUG] Agent.SendFunctionResult: Follow-up stream ended with pending tool call. NOT sending completion signal yet.")
	}

	return nil
}

// Helper function to convert ToolDefinition to openai.Tool
func convertToolDefinitions(tools []ToolDefinition) []openai.Tool {
	var result []openai.Tool
	for _, tool := range tools {
		// Convert FunctionDef to openai.FunctionDefinition
		bytes, _ := json.Marshal(tool.Function.Parameters)
		var params json.RawMessage = bytes

		result = append(result, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  params,
			},
		})
	}
	return result
}

// FileChange represents a change to a file
type FileChange struct {
	Filename    string
	Description string
	Content     string
}

// SendFileChanges sends file changes to the agent for context
func (a *OpenAIAgent) SendFileChanges(ctx context.Context, changes []FileChange) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, change := range changes {
		a.history.AddToolMessage("edit_file", map[string]interface{}{
			"target_file":  change.Filename,
			"instructions": change.Description,
			"code_edit":    change.Content,
		}, "")
	}

	// Save history to disk
	return a.history.Save(a.historyOpts.HistoryPath)
}

// AddSystemMessage adds a system message to the conversation history
func (a *OpenAIAgent) AddSystemMessage(content string) error {
	// Don't add debug messages to history
	if strings.Contains(content, "DEBUG:") {
		// Instead, print to stderr
		// fmt.Fprintf(os.Stderr, "%s\n", content)
		return nil
	}

	// If we have a history instance, add the message to it
	if a.history != nil {
		a.history.AddMessage(Message{
			Role:    "system",
			Content: content,
		})
	}
	return nil
}

// GetLastAssistantMessage returns the most recent assistant message
func (a *OpenAIAgent) GetLastAssistantMessage() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Use logger for debugging
	if a.logger != nil && a.logger.IsEnabled() {
		a.logger.Log("Looking for last assistant message")
	}

	if a.history == nil {
		if a.logger != nil && a.logger.IsEnabled() {
			a.logger.Log("History is nil")
		}
		return "", false
	}

	messages := a.history.GetMessages()
	if len(messages) == 0 {
		if a.logger != nil && a.logger.IsEnabled() {
			a.logger.Log("No messages in history")
		}
		return "", false
	}

	// Find the most recent assistant message
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			if a.logger != nil && a.logger.IsEnabled() {
				a.logger.Log("Found assistant message: %s", messages[i].Content)
			}
			return messages[i].Content, true
		}
	}

	if a.logger != nil && a.logger.IsEnabled() {
		a.logger.Log("No assistant messages found")
	}
	return "", false
}

// Placeholder definition for logDebug if it doesn't exist
// Ensure you have a proper logging mechanism (e.g., writing to a file)
// For now, just print to stderr for visibility during execution.
func (a *OpenAIAgent) logAgentDebug(format string, args ...interface{}) {
	if a.logger != nil && a.logger.IsEnabled() {
		a.logger.Log(format, args...)
	}
}

// FinalizeInteraction clears the current handler, signifying the end of a request-response cycle.
func (a *OpenAIAgent) FinalizeInteraction() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logger.Log("[DEBUG] Agent.FinalizeInteraction: Clearing currentHandler.")
	a.currentHandler = nil
	if a.cancelFunc != nil { // Also cancel context if still active
		a.cancelFunc()
		a.cancelFunc = nil
	}
}

// mustMarshal marshals v to JSON, panicking on error.
func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// In a real application, handle this more gracefully
		panic(fmt.Sprintf("failed to marshal content for tool result: %v", err))
	}
	return data
}

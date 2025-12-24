package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"
	brainv1 "github.com/focusd-so/brain/gen/brain/v1"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

type AgentSession struct {
	mu        *sync.Mutex
	toolCalls map[string]chan *brainv1.AgentSessionRequest_ToolCallResponse
}

func (s *ServiceImpl) AgentSession(ctx context.Context, stream *connect.BidiStream[brainv1.AgentSessionRequest, brainv1.AgentSessionResponse]) error {
	slog.Info("AgentSession: starting new session")

	a := &AgentSession{
		toolCalls: make(map[string]chan *brainv1.AgentSessionRequest_ToolCallResponse),
		mu:        &sync.Mutex{},
	}

	slog.Info("AgentSession: waiting for initial message")
	message, err := stream.Receive()
	if err != nil {
		slog.Error("AgentSession: failed to receive initial message", "error", err)
		return err
	}

	if message.GetRunRequest() == nil {
		slog.Error("AgentSession: missing run request")
		return fmt.Errorf("missing run request")
	}

	slog.Info("AgentSession: received run request",
		"instruction", message.GetRunRequest().GetInstruction(),
		"num_agents", len(message.GetRunRequest().GetAgents()),
		"user_message", message.GetRunRequest().GetUserMessage())

	slog.Info("AgentSession: creating Gemini model")
	model, err := gemini.NewModel(ctx, "gemini-2.5-pro", &genai.ClientConfig{
		APIKey: os.Getenv("FOCUSD_GEMINI_API_KEY"),
	})
	if err != nil {
		slog.Error("AgentSession: failed to create model", "error", err)
		log.Fatalf("Failed to create model: %v", err)
	}

	subAgents := []agent.Agent{}

	slog.Info("AgentSession: constructing subagents")
	// construct subagents
	for _, agent := range message.GetRunRequest().GetAgents() {
		slog.Info("AgentSession: processing agent",
			"name", agent.GetName(),
			"num_tools", len(agent.GetTools()))
		cfg := llmagent.Config{
			Model:       model,
			Name:        agent.GetName(),
			Instruction: agent.GetInstruction(),
		}

		for _, t := range agent.GetTools() {
			fntoolcfg := functiontool.Config{
				Name:        t.GetName(),
				Description: t.GetDescription(),
			}

			if t.GetInputSchema() != "" {
				var inputSchema jsonschema.Schema
				if err := json.Unmarshal([]byte(t.GetInputSchema()), &inputSchema); err != nil {
					log.Fatalf("Failed to unmarshal input schema: %v", err)
				}
				// Only set InputSchema if it has properties or is not just a bare object
				// The Gemini SDK rejects bare {"type":"object"} with no properties
				if inputSchema.Properties != nil && len(inputSchema.Properties) > 0 {
					fntoolcfg.InputSchema = &inputSchema
				} else {
					// Set to empty schema - SDK requires InputSchema to be set, can't be nil
					fntoolcfg.InputSchema = &jsonschema.Schema{}
				}
			} else {
				// Set to empty schema - SDK requires InputSchema to be set, can't be nil
				fntoolcfg.InputSchema = &jsonschema.Schema{}
			}
			if t.GetOutputSchema() != "" {
				var outputSchema jsonschema.Schema
				if err := json.Unmarshal([]byte(t.GetOutputSchema()), &outputSchema); err != nil {
					return fmt.Errorf("Failed to unmarshal output schema: %v", err)
				}
				fntoolcfg.OutputSchema = &outputSchema
			}

			slog.Info("AgentSession: creating function tool", "tool_name", t.GetName())
			fntool, err := functiontool.New(fntoolcfg, func(ctx tool.Context, input map[string]any) (any, error) {
				inputJSON, err := json.Marshal(input)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal tool input: %w", err)
				}

				requestID := uuid.New().String()

				// Send tool call request to client
				if err := stream.Send(&brainv1.AgentSessionResponse{
					Message: &brainv1.AgentSessionResponse_ToolCallRequest_{
						ToolCallRequest: &brainv1.AgentSessionResponse_ToolCallRequest{
							RequestId: requestID,
							ToolName:  t.GetName(),
							Input:     string(inputJSON),
						},
					},
				}); err != nil {
					return nil, fmt.Errorf("failed to send tool call request: %w", err)
				}

				slog.Info("awaiting tool call response", "request_id", requestID)

				// Create channel for this request
				a.mu.Lock()
				a.toolCalls[requestID] = make(chan *brainv1.AgentSessionRequest_ToolCallResponse, 1)
				a.mu.Unlock()

				// Wait for tool call response from client with timeout
				select {
				case response := <-a.toolCalls[requestID]:
					// Clean up the channel
					a.mu.Lock()
					delete(a.toolCalls, requestID)
					a.mu.Unlock()

					if response == nil {
						slog.Error("AgentSession: no tool call response received", "request_id", requestID)
						return nil, fmt.Errorf("no tool call response received")
					}
					return response.GetOutput(), nil
				case <-time.After(3 * time.Minute):
					// Clean up the channel on timeout
					a.mu.Lock()
					delete(a.toolCalls, requestID)
					a.mu.Unlock()

					slog.Error("AgentSession: tool call response timeout", "request_id", requestID)
					return nil, fmt.Errorf("tool call response timeout after 3 minutes")
				}
			})
			if err != nil {
				return fmt.Errorf("failed to create tool: %w", err)
			}

			cfg.Tools = append(cfg.Tools, fntool)
		}

		slog.Info("AgentSession: creating subagent", "name", agent.GetName())
		subAgent, err := llmagent.New(cfg)
		if err != nil {
			slog.Error("AgentSession: failed to create subagent", "name", agent.GetName(), "error", err)
			return err
		}
		subAgents = append(subAgents, subAgent)
	}

	slog.Info("AgentSession: all subagents created", "count", len(subAgents))

	slog.Info("AgentSession: starting message receiver goroutine")
	go func() {
		for {
			message, err := stream.Receive()
			if err != nil {
				slog.Info("AgentSession: message receiver exiting", "error", err)
				break
			}

			switch message.Message.(type) {
			case *brainv1.AgentSessionRequest_ToolCallResponse_:
				toolCallResponse := message.GetToolCallResponse()
				if toolCallResponse == nil {
					slog.Warn("AgentSession: received nil tool call response")
					continue
				}

				slog.Info("AgentSession: received tool call response",
					"request_id", toolCallResponse.GetRequestId())
				a.toolCalls[toolCallResponse.GetRequestId()] <- toolCallResponse
			case *brainv1.AgentSessionRequest_SessionEnd_:
				slog.Info("AgentSession: session ended", "reason", message.GetSessionEnd().GetReason())
				break
			}
		}
	}()

	slog.Info("AgentSession: creating root agent")
	rootAgent, err := llmagent.New(llmagent.Config{
		Model:       model,
		Name:        "root_agent",
		Instruction: message.GetRunRequest().GetInstruction(),
		SubAgents:   subAgents,
	})
	if err != nil {
		slog.Error("AgentSession: failed to create root agent", "error", err)
		return err
	}

	slog.Info("AgentSession: creating runner")
	sessService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "focusd",
		Agent:          rootAgent,
		SessionService: sessService,
	})
	if err != nil {
		slog.Error("AgentSession: failed to create runner", "error", err)
		return fmt.Errorf("failed to create runner: %w", err)
	}

	// Create the user message content
	userMsg := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			&genai.Part{
				Text: message.GetRunRequest().GetUserMessage(),
			},
		},
	}

	// Generate session ID and create session
	sessionID := uuid.New().String()
	slog.Info("AgentSession: creating session", "session_id", sessionID)
	_, err = sessService.Create(ctx, &session.CreateRequest{
		AppName:   "focusd",
		UserID:    "user",
		SessionID: sessionID,
	})
	if err != nil {
		slog.Error("AgentSession: failed to create session", "error", err)
		return fmt.Errorf("failed to create session: %w", err)
	}

	// Run the agent and collect events
	slog.Info("AgentSession: starting agent run")
	var responseText string
	for event, err := range r.Run(ctx, "user", sessionID, userMsg, agent.RunConfig{}) {
		if err != nil {
			slog.Error("AgentSession: error during agent run", "error", err)
			return fmt.Errorf("error during agent run: %w", err)
		}

		// Collect response content from events (event embeds LLMResponse)
		if event.Content != nil {
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					slog.Debug("AgentSession: received content part", "text_length", len(part.Text))
					responseText += part.Text
				}
			}
		}
	}

	// Send the generated content back to the client
	slog.Info("AgentSession: agent run completed", "response_length", len(responseText))
	slog.Info("AgentSession: sending run response to client")
	if err := stream.Send(&brainv1.AgentSessionResponse{
		Message: &brainv1.AgentSessionResponse_RunResponse_{
			RunResponse: &brainv1.AgentSessionResponse_RunResponse{
				Content: responseText,
			},
		},
	}); err != nil {
		slog.Error("AgentSession: failed to send run response", "error", err)
		return fmt.Errorf("failed to send run response: %w", err)
	}

	// Send session end acknowledgment
	slog.Info("AgentSession: sending session end acknowledgment")
	if err := stream.Send(&brainv1.AgentSessionResponse{
		Message: &brainv1.AgentSessionResponse_SessionEndAck_{
			SessionEndAck: &brainv1.AgentSessionResponse_SessionEndAck{
				Acknowledged: true,
			},
		},
	}); err != nil {
		slog.Error("AgentSession: failed to send session end ack", "error", err)
		return fmt.Errorf("failed to send session end ack: %w", err)
	}

	// Close the stream and return
	slog.Info("AgentSession: session completed successfully")
	return nil
}

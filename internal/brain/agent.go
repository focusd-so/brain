package brain

import (
	"context"
	"encoding/json"
	"fmt"
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
	mu         *sync.Mutex
	toolsQueue map[string]chan *brainv1.AgentSessionRequest_ToolCallResponse
}

func (s *ServiceImpl) AgentSession(ctx context.Context, stream *connect.BidiStream[brainv1.AgentSessionRequest, brainv1.AgentSessionResponse]) error {
	a := &AgentSession{
		toolsQueue: make(map[string]chan *brainv1.AgentSessionRequest_ToolCallResponse),
		mu:         &sync.Mutex{},
	}

	message, err := stream.Receive()
	if err != nil {
		slog.Error("AgentSession: failed to receive initial message", "error", err)
		return err
	}

	if message.GetRunRequest() == nil {
		slog.Error("AgentSession: missing run request")
		return fmt.Errorf("missing run request")
	}

	// Try GOOGLE_API_KEY first, then GEMINI_API_KEY, then FOCUSD_GEMINI_API_KEY
	apiKey := os.Getenv("GOOGLE_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("FOCUSD_GEMINI_API_KEY")
	}

	if apiKey == "" {
		slog.Error("AgentSession: no Gemini API key found")
		return fmt.Errorf("no Gemini API key found (tried GOOGLE_API_KEY, GEMINI_API_KEY, FOCUSD_GEMINI_API_KEY)")
	}

	model, err := gemini.NewModel(ctx, "gemini-1.5-flash", &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		slog.Error("AgentSession: failed to create model", "error", err)
		return fmt.Errorf("failed to create model: %w", err)
	}

	subAgents := []agent.Agent{}

	for _, agent := range message.GetRunRequest().GetAgents() {
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
					return fmt.Errorf("failed to unmarshal input schema: %w", err)
				}
				// Only set InputSchema if it has properties or is not just a bare object
				// The Gemini SDK rejects bare {"type":"object"} with no properties
				if len(inputSchema.Properties) > 0 {
					fntoolcfg.InputSchema = &inputSchema
				} else {
					fntoolcfg.InputSchema = &jsonschema.Schema{}
				}
			} else {
				fntoolcfg.InputSchema = &jsonschema.Schema{}
			}
			if t.GetOutputSchema() != "" {
				var outputSchema jsonschema.Schema
				if err := json.Unmarshal([]byte(t.GetOutputSchema()), &outputSchema); err != nil {
					return fmt.Errorf("Failed to unmarshal output schema: %v", err)
				}
				fntoolcfg.OutputSchema = &outputSchema
			}

			fntool, err := functiontool.New(fntoolcfg, func(ctx tool.Context, input map[string]any) (map[string]any, error) {
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

				a.mu.Lock()
				a.toolsQueue[requestID] = make(chan *brainv1.AgentSessionRequest_ToolCallResponse, 1)
				a.mu.Unlock()
				select {
				case response := <-a.toolsQueue[requestID]:
					// Clean up the channel
					a.mu.Lock()
					delete(a.toolsQueue, requestID)
					a.mu.Unlock()

					if response == nil {
						return nil, fmt.Errorf("no tool call response received")
					}

					var output map[string]any
					if err := json.Unmarshal([]byte(response.GetOutput()), &output); err != nil {
						return nil, fmt.Errorf("failed to unmarshal tool call response: %w", err)
					}

					return output, nil
				case <-time.After(3 * time.Minute):
					// Clean up the channel on timeout
					a.mu.Lock()
					delete(a.toolsQueue, requestID)
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

		subAgent, err := llmagent.New(cfg)
		if err != nil {
			return err
		}
		subAgents = append(subAgents, subAgent)
	}

	slog.Info("AgentSession: all subagents created", "count", len(subAgents))

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
					"request_id", toolCallResponse.GetRequestId(), "response", toolCallResponse.GetOutput())

				a.mu.Lock()
				ch, ok := a.toolsQueue[toolCallResponse.GetRequestId()]
				a.mu.Unlock()

				if ok {
					ch <- toolCallResponse
				} else {
					slog.Warn("AgentSession: received response for unknown or timed-out tool call", "request_id", toolCallResponse.GetRequestId())
				}
			case *brainv1.AgentSessionRequest_SessionEnd_:
				slog.Info("AgentSession: session ended", "reason", message.GetSessionEnd().GetReason())
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

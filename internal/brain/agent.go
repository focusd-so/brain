package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sync"

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
	a := &AgentSession{
		toolCalls: make(map[string]chan *brainv1.AgentSessionRequest_ToolCallResponse),
		mu:        &sync.Mutex{},
	}

	message, err := stream.Receive()
	if err != nil {
		return err
	}

	model, err := gemini.NewModel(ctx, "gemini-2.5-pro", &genai.ClientConfig{
		APIKey: os.Getenv("GOOGLE_API_KEY"),
	})
	if err != nil {
		log.Fatalf("Failed to create model: %v", err)
	}

	subAgents := []agent.Agent{}

	// construct subagents
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
					log.Fatalf("Failed to unmarshal input schema: %v", err)
				}
				fntoolcfg.InputSchema = &inputSchema
			}
			if t.GetOutputSchema() != "" {
				var outputSchema jsonschema.Schema
				if err := json.Unmarshal([]byte(t.GetOutputSchema()), &outputSchema); err != nil {
					return fmt.Errorf("Failed to unmarshal output schema: %v", err)
				}
				fntoolcfg.OutputSchema = &outputSchema
			}
			fntool, err := functiontool.New(fntoolcfg, func(ctx tool.Context, input any) (any, error) {
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

				// Wait for tool call response from client
				response := <-a.toolCalls[requestID]
				if response == nil {
					return nil, fmt.Errorf("no tool call response received")
				}

				return response.GetOutput(), nil
			})
			if err != nil {
				log.Fatalf("Failed to create tool: %v", err)
			}

			cfg.Tools = append(cfg.Tools, fntool)
		}

		subAgent, err := llmagent.New(cfg)
		if err != nil {
			return err
		}
		subAgents = append(subAgents, subAgent)
	}

	go func() {
		for {
			message, err := stream.Receive()
			if err != nil {
				break
			}

			switch message.Message.(type) {
			case *brainv1.AgentSessionRequest_ToolCallResponse_:
				toolCallResponse := message.GetToolCallResponse()
				if toolCallResponse == nil {
					continue
				}

				a.toolCalls[toolCallResponse.GetRequestId()] <- toolCallResponse
			case *brainv1.AgentSessionRequest_SessionEnd_:
				slog.Info("session ended", "reason", message.GetSessionEnd().GetReason())
				break
			}
		}
	}()

	rootAgent, err := llmagent.New(llmagent.Config{
		Model:       model,
		Name:        "root_agent",
		Instruction: message.GetRunRequest().GetInstruction(),
		SubAgents:   subAgents,
	})
	if err != nil {
		return err
	}

	r, err := runner.New(runner.Config{
		AppName:        "focusd",
		Agent:          rootAgent,
		SessionService: session.InMemoryService(),
	})
	if err != nil {
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

	// Run the agent and collect events
	var responseText string
	for event, err := range r.Run(ctx, "user", uuid.New().String(), userMsg, agent.RunConfig{}) {
		if err != nil {
			return fmt.Errorf("error during agent run: %w", err)
		}

		// Collect response content from events (event embeds LLMResponse)
		if event.Content != nil {
			for _, part := range event.Content.Parts {
				if part.Text != "" {
					responseText += part.Text
				}
			}
		}
	}

	// Send the generated content back to the client
	if err := stream.Send(&brainv1.AgentSessionResponse{
		Message: &brainv1.AgentSessionResponse_RunResponse_{
			RunResponse: &brainv1.AgentSessionResponse_RunResponse{
				Content: responseText,
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send run response: %w", err)
	}

	// Send session end acknowledgment
	if err := stream.Send(&brainv1.AgentSessionResponse{
		Message: &brainv1.AgentSessionResponse_SessionEndAck_{
			SessionEndAck: &brainv1.AgentSessionResponse_SessionEndAck{
				Acknowledged: true,
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send session end ack: %w", err)
	}

	// Close the stream and return
	return nil
}

// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

// Package main provides the hd-driver-openai AI model driver for the
// Honeydipper automation framework.  It implements the send_to_model RPC
// so that the agent service can call OpenAI chat-completion endpoints.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"

	agentpkg "github.com/honeydipper/honeydipper/v4/pkg/agent"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/mitchellh/mapstructure"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

var driver *dipper.Driver

// engineConfig holds per-engine connection and model settings loaded from
// driver.Options["data.engines.<name>"].
type engineConfig struct {
	Model   string `mapstructure:"model"`
	APIKey  string `mapstructure:"api_key"`
	BaseURL string `mapstructure:"base_url"`
}

func main() {
	flag.Parse()
	driver = dipper.NewDriver(flag.Arg(0), "openai")
	driver.RPCHandlers["send_to_model|interruptible"] = sendToModel
	driver.Reload = func(m *dipper.Message) {}
	driver.Run()
}

// sendToModel is the RPC handler for send_to_model.  It decodes the payload,
// calls the OpenAI chat-completions endpoint, and delivers the response back
// to the agent session via the agentbus:receive channel.
func sendToModel(msg *dipper.Message) {
	msg = dipper.DeserializePayload(msg)

	sessionID := msg.Labels["agent_session_id"]
	defer dipper.SafeExitOnError("[openai] send_to_model", func(r interface{}) {
		if r != nil {
			m := agentbusMessage(sessionID, agentpkg.Message{
				Role:       agentpkg.RoleAgent,
				IsComplete: true,
			})
			m.Labels["status"] = "error"
			m.Labels["reason"] = fmt.Sprintf("%+v", r)
			driver.SendMessage(m)
		}
	})

	engineName := dipper.MustGetMapDataStr(msg.Payload, "engine")

	// Load engine-specific configuration from driver options.
	engRaw, ok := dipper.GetMapData(driver.Options, "data.engines."+engineName)
	if !ok || engRaw == nil {
		dipper.Logger.Panicf("[openai] unknown engine %q session=%s", engineName, sessionID)

		return
	}

	var cfg engineConfig
	dipper.Must(mapstructure.Decode(engRaw, &cfg))

	historyRaw, _ := dipper.GetMapData(msg.Payload, "history")
	var history []agentpkg.Message
	if historyRaw != nil {
		dipper.Must(mapstructure.Decode(historyRaw, &history))
	}

	toolsRaw, _ := dipper.GetMapData(msg.Payload, "tools")
	var tools map[string]agentpkg.Tool
	if toolsRaw != nil {
		dipper.Must(mapstructure.Decode(toolsRaw, &tools))
	}

	modelDataRaw, _ := dipper.GetMapData(msg.Payload, "model_data")
	var modelData map[string]interface{}
	if modelDataRaw != nil {
		_ = mapstructure.Decode(modelDataRaw, &modelData)
	}

	// Build the OpenAI request.
	params := openai.ChatCompletionNewParams{
		Model:    cfg.Model,
		Messages: buildMessages(history),
	}
	if openaiTools := buildTools(tools); len(openaiTools) > 0 {
		params.Tools = openaiTools
	}

	// Per-request overrides from model_data (e.g. temperature, max_tokens).
	reqOpts := make([]option.RequestOption, 0, len(modelData))
	for k, v := range modelData {
		reqOpts = append(reqOpts, option.WithJSONSet(k, v))
	}

	// Obtain a context that is cancelled when the driver shuts down.
	ctx, cancel := driver.GetContext(msg)
	defer cancel()

	client := newOpenAIClient(cfg)

	shouldStream, _ := dipper.GetMapDataBool(msg.Payload, "should_stream")
	if shouldStream {
		chunkSq := 0
		if chunkSqStr := msg.Labels["chunk_sq"]; chunkSqStr != "" {
			chunkSq = dipper.Must(strconv.Atoi(chunkSqStr)).(int)
		}
		reqOpts = append(reqOpts, option.WithJSONSet("stream_options", map[string]interface{}{"include_usage": true}))
		sendToModelStreaming(ctx, client, params, reqOpts, sessionID, chunkSq)

		return
	}

	completion := dipper.Must(client.Chat.Completions.New(ctx, params, reqOpts...)).(*openai.ChatCompletion)

	if len(completion.Choices) == 0 {
		dipper.Logger.Panicf("[openai] send_to_model no choices returned session=%s", sessionID)

		return
	}

	choice := completion.Choices[0]

	var agentMsg agentpkg.Message
	if choice.FinishReason == "tool_calls" {
		agentMsg = buildToolCallMessage(choice.Message.ToolCalls)
	} else {
		agentMsg = agentpkg.Message{
			Role:       agentpkg.RoleAgent,
			Content:    choice.Message.Content,
			IsComplete: true,
		}
	}
	agentMsg.InputTokens = int(completion.Usage.PromptTokens)
	agentMsg.OutputTokens = int(completion.Usage.CompletionTokens)

	driver.SendMessage(agentbusMessage(sessionID, agentMsg))
}

// agentbusMessage wraps an agent message in a dipper transport message for the agentbus receive channel.
func agentbusMessage(sessionID string, msg agentpkg.Message) *dipper.Message {
	return &dipper.Message{
		Channel: "agentbus",
		Subject: "receive",
		Labels: map[string]string{
			"agent_session_id": sessionID,
			"sequence":         sessionID, // ensure messages for the same session are processed in order
		},
		Payload: map[string]interface{}{"message": msg},
	}
}

// sendToModelStreaming calls the OpenAI streaming endpoint, emits each content
// delta as a non-complete agentbus message, and sends a final complete message
// once the stream closes.  Tool-call responses are accumulated and sent as a
// single tool message at the end.
func sendToModelStreaming(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams, reqOpts []option.RequestOption, sessionID string, chunkSq int) {
	streamer := client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
	acc := openai.ChatCompletionAccumulator{}

	for streamer.Next() {
		chunk := streamer.Current()
		acc.AddChunk(chunk)

		// Tool calls are accumulated; skip content handling until the stream ends.
		if _, ok := acc.JustFinishedToolCall(); ok {
			continue
		}

		// Refusal: send the complete accumulated refusal and stop.
		if refusal, ok := acc.JustFinishedRefusal(); ok {
			driver.SendMessage(agentbusMessage(sessionID, agentpkg.Message{
				Role:       agentpkg.RoleAgent,
				Content:    refusal,
				IsComplete: true,
			}))

			return
		}

		// Send each content delta immediately as a non-complete chunk.
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			driver.SendMessage(agentbusMessage(sessionID, agentpkg.Message{
				Role:       agentpkg.RoleAgent,
				Content:    chunk.Choices[0].Delta.Content,
				IsComplete: false,
			}))
		}
	}

	dipper.Must(streamer.Err())

	if len(acc.Choices) == 0 {
		dipper.Logger.Panicf("[openai] streaming no choices returned session=%s", sessionID)

		return
	}

	// Send the terminal message based on finish reason.
	choice := acc.Choices[0]

	if choice.FinishReason == "tool_calls" {
		tcMsg := buildToolCallMessage(choice.Message.ToolCalls)
		tcMsg.InputTokens = int(acc.Usage.PromptTokens)
		tcMsg.OutputTokens = int(acc.Usage.CompletionTokens)
		driver.SendMessage(agentbusMessage(sessionID, tcMsg))
	} else {
		driver.SendMessage(agentbusMessage(sessionID, agentpkg.Message{
			Role:         agentpkg.RoleAgent,
			IsComplete:   true,
			InputTokens:  int(acc.Usage.PromptTokens),
			OutputTokens: int(acc.Usage.CompletionTokens),
		}))
	}
}

// newOpenAIClient creates an OpenAI client from the per-engine configuration.
func newOpenAIClient(cfg engineConfig) *openai.Client {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}

	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	c := openai.NewClient(opts...)

	return &c
}

// buildMessages converts the agent conversation history into the slice of
// OpenAI message params expected by ChatCompletionNewParams.Messages.
func buildMessages(history []agentpkg.Message) []openai.ChatCompletionMessageParamUnion {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(history))
	var lastToolCallIDs []string

	for histIdx, msg := range history {
		switch msg.Role {
		case agentpkg.RoleSystem:
			msgs = append(msgs, openai.SystemMessage(msg.Content))

		case agentpkg.RoleUser:
			msgs = append(msgs, openai.UserMessage(msg.Content))

		case agentpkg.RoleAgent:
			msgs = append(msgs, openai.AssistantMessage(msg.Content))

		case agentpkg.RoleTool:
			// The model previously requested tool calls; reconstruct the
			// assistant message with ToolCalls so OpenAI sees the full turn.
			ids := make([]string, len(msg.ToolCalls))
			toolCalls := make([]openai.ChatCompletionMessageToolCallUnionParam, len(msg.ToolCalls))

			for i, tc := range msg.ToolCalls {
				id := fmt.Sprintf("call_%d_%d", histIdx, i)
				ids[i] = id

				argBytes, _ := json.Marshal(tc.Params)
				toolCalls[i] = openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: id,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.FuncName,
							Arguments: string(argBytes),
						},
					},
				}
			}

			msgs = append(msgs, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &openai.ChatCompletionAssistantMessageParam{
					ToolCalls: toolCalls,
				},
			})
			lastToolCallIDs = ids

		case agentpkg.RoleToolResult:
			// One ToolMessage per result, matched to the prior tool call IDs.
			for i, result := range msg.ToolResult {
				id := ""
				if i < len(lastToolCallIDs) {
					id = lastToolCallIDs[i]
				}

				resultBytes, _ := json.Marshal(result)
				msgs = append(msgs, openai.ToolMessage(string(resultBytes), id))
			}
		}
	}

	return msgs
}

// buildTools converts the agent tool map into the OpenAI tool params slice.
func buildTools(tools map[string]agentpkg.Tool) []openai.ChatCompletionToolUnionParam {
	if len(tools) == 0 {
		return nil
	}

	result := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))

	for _, tool := range tools {
		properties := make(map[string]interface{}, len(tool.Params))
		required := make([]string, 0, len(tool.Params))

		for paramName, paramDef := range tool.Params {
			def, ok := paramDef.(map[string]interface{})
			if !ok {
				continue
			}

			prop := map[string]interface{}{"type": "string"}
			if t, ok := def["type"].(string); ok && t != "" {
				prop["type"] = t
			}

			if d, ok := def["description"]; ok {
				prop["description"] = d
			}

			properties[paramName] = prop
			required = append(required, paramName)
		}

		parameters := openai.FunctionParameters{
			"type":       "object",
			"properties": properties,
		}
		if len(required) > 0 {
			parameters["required"] = required
		}

		result = append(result, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: openai.FunctionDefinitionParam{
					Name:        tool.Name,
					Description: openai.String(tool.Description),
					Parameters:  parameters,
				},
			},
		})
	}

	return result
}

// buildToolCallMessage converts OpenAI tool-call response entries into the
// agent message format consumed by the agent session.
func buildToolCallMessage(toolCalls []openai.ChatCompletionMessageToolCallUnion) agentpkg.Message {
	calls := make([]agentpkg.ToolCall, 0, len(toolCalls))

	for _, tc := range toolCalls {
		fn := tc.Function

		var params map[string]interface{}
		_ = json.Unmarshal([]byte(fn.Arguments), &params)

		calls = append(calls, agentpkg.ToolCall{
			FuncName: fn.Name,
			Params:   params,
		})
	}

	return agentpkg.Message{
		Role:      agentpkg.RoleTool,
		ToolCalls: calls,
	}
}

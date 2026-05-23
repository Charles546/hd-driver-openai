// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	agentpkg "github.com/honeydipper/honeydipper/v4/pkg/agent"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if dipper.Logger == nil {
		f, _ := os.Create("test.log")
		defer f.Close() //nolint:errcheck
		dipper.GetLogger("test service", "DEBUG", f, f)
	}
	os.Exit(m.Run())
}

// ─── buildMessages ────────────────────────────────────────────────────────────

func TestBuildMessages_Empty(t *testing.T) {
	assert.Empty(t, buildMessages(nil))
	assert.Empty(t, buildMessages([]agentpkg.Message{}))
}

func TestBuildMessages_SystemAndUser(t *testing.T) {
	history := []agentpkg.Message{
		{Role: agentpkg.RoleSystem, Content: "You are helpful."},
		{Role: agentpkg.RoleUser, Content: "Hello!"},
	}
	msgs := buildMessages(history)
	require.Len(t, msgs, 2)
	assert.NotNil(t, msgs[0].OfSystem)
	assert.NotNil(t, msgs[1].OfUser)
}

func TestBuildMessages_AgentMessage(t *testing.T) {
	history := []agentpkg.Message{
		{Role: agentpkg.RoleAgent, Content: "Hi there!"},
	}
	msgs := buildMessages(history)
	require.Len(t, msgs, 1)
	assert.NotNil(t, msgs[0].OfAssistant)
	assert.Nil(t, msgs[0].OfAssistant.ToolCalls)
}

func TestBuildMessages_ToolRoundTrip(t *testing.T) {
	history := []agentpkg.Message{
		{
			Role: agentpkg.RoleTool,
			ToolCalls: []agentpkg.ToolCall{
				{FuncName: "my_func", Params: map[string]interface{}{"arg": "val"}},
			},
		},
		{
			Role:       agentpkg.RoleToolResult,
			ToolResult: []map[string]interface{}{{"result": "ok"}},
		},
	}
	msgs := buildMessages(history)
	// assistant message with tool_calls + 1 tool result
	require.Len(t, msgs, 2)
	require.NotNil(t, msgs[0].OfAssistant)
	require.Len(t, msgs[0].OfAssistant.ToolCalls, 1)
	require.NotNil(t, msgs[0].OfAssistant.ToolCalls[0].OfFunction)
	assert.Equal(t, "call_0_0", msgs[0].OfAssistant.ToolCalls[0].OfFunction.ID)
	assert.Equal(t, "my_func", msgs[0].OfAssistant.ToolCalls[0].OfFunction.Function.Name)
	assert.NotNil(t, msgs[1].OfTool)
}

func TestBuildMessages_MultipleToolResults(t *testing.T) {
	history := []agentpkg.Message{
		{
			Role: agentpkg.RoleTool,
			ToolCalls: []agentpkg.ToolCall{
				{FuncName: "fn1", Params: map[string]interface{}{}},
				{FuncName: "fn2", Params: map[string]interface{}{}},
			},
		},
		{
			Role: agentpkg.RoleToolResult,
			ToolResult: []map[string]interface{}{
				{"out": "a"},
				{"out": "b"},
			},
		},
	}
	msgs := buildMessages(history)
	// 1 assistant with 2 tool calls + 2 tool result messages
	require.Len(t, msgs, 3)
	assert.NotNil(t, msgs[0].OfAssistant)
	assert.Len(t, msgs[0].OfAssistant.ToolCalls, 2)
	assert.NotNil(t, msgs[1].OfTool)
	assert.NotNil(t, msgs[2].OfTool)
}

// ─── buildTools ───────────────────────────────────────────────────────────────

func TestBuildTools_Empty(t *testing.T) {
	assert.Nil(t, buildTools(nil))
	assert.Nil(t, buildTools(map[string]agentpkg.Tool{}))
}

func TestBuildTools_WithParam(t *testing.T) {
	tools := map[string]agentpkg.Tool{
		"search": {
			Name:        "search",
			Description: "Search the web",
			Params: map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "search query",
				},
			},
		},
	}
	result := buildTools(tools)
	require.Len(t, result, 1)
	require.NotNil(t, result[0].OfFunction)
	assert.Equal(t, "search", result[0].OfFunction.Function.Name)
	params := result[0].OfFunction.Function.Parameters
	properties, ok := params["properties"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, properties, "query")
	required, ok := params["required"].([]string)
	require.True(t, ok)
	assert.Contains(t, required, "query")
}

func TestBuildTools_SkipsInvalidParam(t *testing.T) {
	tools := map[string]agentpkg.Tool{
		"tool": {
			Name: "tool",
			Params: map[string]interface{}{
				"good": map[string]interface{}{"type": "string"},
				"bad":  "not-a-map",
			},
		},
	}
	result := buildTools(tools)
	require.Len(t, result, 1)
	params := result[0].OfFunction.Function.Parameters
	properties := params["properties"].(map[string]interface{})
	assert.Contains(t, properties, "good")
	assert.NotContains(t, properties, "bad")
}

// ─── buildToolCallMessage ──────────────────────────────────────────────────────

func TestBuildToolCallMessage_Single(t *testing.T) {
	toolCalls := []openai.ChatCompletionMessageToolCallUnion{
		{
			Function: openai.ChatCompletionMessageFunctionToolCallFunction{
				Name:      "do_thing",
				Arguments: `{"x": 1}`,
			},
		},
	}
	msg := buildToolCallMessage(toolCalls)
	assert.Equal(t, agentpkg.RoleTool, msg.Role)
	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "do_thing", msg.ToolCalls[0].FuncName)
	assert.Equal(t, float64(1), msg.ToolCalls[0].Params["x"])
}

func TestBuildToolCallMessage_Multiple(t *testing.T) {
	toolCalls := []openai.ChatCompletionMessageToolCallUnion{
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "a", Arguments: `{"k":"v1"}`}},
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "b", Arguments: `{"k":"v2"}`}},
	}
	msg := buildToolCallMessage(toolCalls)
	require.Len(t, msg.ToolCalls, 2)
	assert.Equal(t, "a", msg.ToolCalls[0].FuncName)
	assert.Equal(t, "b", msg.ToolCalls[1].FuncName)
}

func TestBuildToolCallMessage_InvalidJSON(t *testing.T) {
	toolCalls := []openai.ChatCompletionMessageToolCallUnion{
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "fn", Arguments: `not-json`}},
	}
	msg := buildToolCallMessage(toolCalls)
	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "fn", msg.ToolCalls[0].FuncName)
	assert.Nil(t, msg.ToolCalls[0].Params)
}

// ─── sendToModel integration ──────────────────────────────────────────────────

// mockOpenAIServer returns an httptest.Server that responds with a canned OpenAI
// chat completion JSON body.
func mockOpenAIServer(body map[string]interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func openAITextBody(content string) map[string]interface{} {
	return map[string]interface{}{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1234567890, "model": "gpt-4o",
		"choices": []map[string]interface{}{
			{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": content}, "finish_reason": "stop"},
		},
		"usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

func openAIToolCallBody(toolName, argsJSON string) map[string]interface{} {
	return map[string]interface{}{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1234567890, "model": "gpt-4o",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{
						{"id": "call_abc", "type": "function", "function": map[string]interface{}{"name": toolName, "arguments": argsJSON}},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
}

func setupDriverWithServer(ts *httptest.Server) (io.Reader, *io.PipeWriter) {
	outReader, outWriter := io.Pipe()
	driver = &dipper.Driver{
		Options: map[string]interface{}{
			"data": map[string]interface{}{
				"engines": map[string]interface{}{
					"test-engine": map[string]interface{}{
						"model":    "gpt-4o",
						"api_key":  "test-key",
						"base_url": ts.URL + "/",
					},
				},
			},
		},
		Out: outWriter,
	}

	return outReader, outWriter
}

func testMessage(engineName string) *dipper.Message {
	return &dipper.Message{
		Channel: "rpc",
		Subject: "send_to_model",
		Labels:  map[string]string{"agent_session_id": "sess-test"},
		Payload: map[string]interface{}{
			"engine":  engineName,
			"history": []interface{}{},
		},
	}
}

func TestSendToModel_TextResponse(t *testing.T) {
	ts := mockOpenAIServer(openAITextBody("Hello, test!"))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts)
	defer outWriter.Close() //nolint:errcheck

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	sendToModel(testMessage("test-engine"))

	result := <-done
	require.NotNil(t, result)
	assert.Equal(t, "agentbus", result.Channel)
	assert.Equal(t, "receive", result.Subject)
	assert.Equal(t, "sess-test", result.Labels["agent_session_id"])

	payloadMap, ok := result.Payload.(map[string]interface{})
	require.True(t, ok)
	msgMap, ok := payloadMap["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, agentpkg.RoleAgent, msgMap["Role"])
	assert.Equal(t, "Hello, test!", msgMap["content"])
	assert.True(t, msgMap["is_complete"].(bool))
}

func TestSendToModel_ToolCallResponse(t *testing.T) {
	ts := mockOpenAIServer(openAIToolCallBody("search_web", `{"query":"golang testing"}`))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts)
	defer outWriter.Close() //nolint:errcheck

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	sendToModel(testMessage("test-engine"))

	result := <-done
	require.NotNil(t, result)

	payloadMap, ok := result.Payload.(map[string]interface{})
	require.True(t, ok)
	msgMap, ok := payloadMap["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, agentpkg.RoleTool, msgMap["Role"])

	rawCalls, ok := msgMap["ToolCalls"].([]interface{})
	require.True(t, ok)
	require.Len(t, rawCalls, 1)
	call := rawCalls[0].(map[string]interface{})
	assert.Equal(t, "search_web", call["FuncName"])
}

func TestSendToModel_UnknownEngine(t *testing.T) {
	driver = &dipper.Driver{
		Options: map[string]interface{}{
			"data": map[string]interface{}{
				"engines": map[string]interface{}{},
			},
		},
		Out: io.Discard,
	}
	msg := testMessage("nonexistent")
	assert.NotPanics(t, func() { sendToModel(msg) })
}

// ─── streaming sendToModel ─────────────────────────────────────────────────────

// mockSSEServer returns an httptest.Server that responds with Server-Sent Events.
// Each element of chunks is written as a separate SSE data line; a [DONE] sentinel
// is appended automatically.
func mockSSEServer(chunks []map[string]interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		for _, chunk := range chunks {
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b) //nolint:errcheck
		}

		fmt.Fprint(w, "data: [DONE]\n\n") //nolint:errcheck

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

func streamingChunk(id, content, finishReason string) map[string]interface{} {
	delta := map[string]interface{}{"role": "assistant", "content": content}
	choice := map[string]interface{}{"index": 0, "delta": delta}

	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}

	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "created": 1234567890, "model": "gpt-4o",
		"choices": []interface{}{choice},
	}
}

func streamingToolCallChunk(id, toolID, toolName, args, finishReason string) map[string]interface{} {
	toolCall := map[string]interface{}{
		"index": 0, "id": toolID, "type": "function",
		"function": map[string]interface{}{"name": toolName, "arguments": args},
	}
	delta := map[string]interface{}{"role": "assistant", "content": nil, "tool_calls": []interface{}{toolCall}}
	choice := map[string]interface{}{"index": 0, "delta": delta}

	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}

	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "created": 1234567890, "model": "gpt-4o",
		"choices": []interface{}{choice},
	}
}

func setupStreamingDriverWithServer(ts *httptest.Server) (io.Reader, *io.PipeWriter) {
	outReader, outWriter := io.Pipe()
	driver = &dipper.Driver{
		Options: map[string]interface{}{
			"data": map[string]interface{}{
				"engines": map[string]interface{}{
					"stream-engine": map[string]interface{}{
						"model":    "gpt-4o",
						"api_key":  "test-key",
						"base_url": ts.URL + "/",
					},
				},
			},
		},
		Out: outWriter,
	}

	return outReader, outWriter
}

func TestSendToModel_StreamingTextResponse(t *testing.T) {
	chunks := []map[string]interface{}{
		streamingChunk("id1", "Hello", ""),
		streamingChunk("id1", ", world", ""),
		streamingChunk("id1", "", "stop"),
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts)
	defer outWriter.Close() //nolint:errcheck

	// Collect all messages until the writer is closed.  FetchMessage panics on EOF
	// so we recover to detect the end of stream.
	var msgs []*dipper.Message

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			var m *dipper.Message
			var panicked bool

			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()

				m = dipper.FetchMessage(outReader)
			}()

			if panicked || m == nil {
				return
			}

			msgs = append(msgs, m)
		}
	}()

	sendToModel(&dipper.Message{
		Channel: "rpc",
		Subject: "send_to_model",
		Labels:  map[string]string{"agent_session_id": "sess-stream"},
		Payload: map[string]interface{}{
			"engine":        "stream-engine",
			"history":       []interface{}{},
			"should_stream": true,
		},
	})

	outWriter.Close() //nolint:errcheck
	<-done

	require.GreaterOrEqual(t, len(msgs), 2, "expected at least one chunk and one final message")

	// Last message must be complete.
	last := msgs[len(msgs)-1]
	payloadMap := last.Payload.(map[string]interface{})
	msgMap := payloadMap["message"].(map[string]interface{})
	assert.True(t, msgMap["is_complete"].(bool), "last message should be complete")
	assert.Equal(t, agentpkg.RoleAgent, msgMap["Role"])

	// Intermediate messages must not be marked complete.
	for _, m := range msgs[:len(msgs)-1] {
		p := m.Payload.(map[string]interface{})
		mm := p["message"].(map[string]interface{})
		assert.False(t, mm["is_complete"].(bool), "non-final chunk should not be complete")
	}
}

func TestSendToModel_StreamingToolCallResponse(t *testing.T) {
	chunks := []map[string]interface{}{
		streamingToolCallChunk("id2", "call_xyz", "search_web", `{"query":"go testing"}`, ""),
		streamingToolCallChunk("id2", "", "", "", "tool_calls"),
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts)
	defer outWriter.Close() //nolint:errcheck

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	sendToModel(&dipper.Message{
		Channel: "rpc",
		Subject: "send_to_model",
		Labels:  map[string]string{"agent_session_id": "sess-tool-stream"},
		Payload: map[string]interface{}{
			"engine":        "stream-engine",
			"history":       []interface{}{},
			"should_stream": true,
		},
	})

	result := <-done
	require.NotNil(t, result)

	payloadMap, ok := result.Payload.(map[string]interface{})
	require.True(t, ok)
	msgMap, ok := payloadMap["message"].(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, agentpkg.RoleTool, msgMap["Role"])

	rawCalls, ok := msgMap["ToolCalls"].([]interface{})
	require.True(t, ok)
	require.Len(t, rawCalls, 1)

	call := rawCalls[0].(map[string]interface{})
	assert.Equal(t, "search_web", call["FuncName"])
}

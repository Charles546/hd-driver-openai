// Copyright 2026 Chun Huang (Charles).

// This Source Code Form is dual-licensed.
// By default, this file is licensed under the GNU Affero General Public License v3.0.
// If you have a separate written commercial agreement, you may use this file under those terms instead.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	agentpkg "github.com/honeydipper/honeydipper/v4/pkg/agent"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if dipper.Logger == nil {
		f, _ := os.Create("test.log")
		defer f.Close()
		dipper.GetLogger("test service", "DEBUG", f, f)
	}
	os.Exit(m.Run())
}

// defaultEngineConfig is the minimal engine options used by most tests.  It
// sets retry_max_attempts to 1 so that retryable-error tests don't hang.
var defaultEngineConfig = map[string]interface{}{
	"model":              "gpt-4o",
	"api_key":            "test-key",
	"retry_max_attempts": 1,
}

// ─── extractAPIErrorInfo ───────────────────────────────────────────────────────

func TestExtractAPIErrorInfo_NoError(t *testing.T) {
	t.Parallel()

	raw := `{"id":"chatcmpl-abc","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`
	info := extractAPIErrorInfo(raw)
	assert.Nil(t, info)
}

func TestExtractAPIErrorInfo_RateLimit(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded.","type":"requests"}}`
	info := extractAPIErrorInfo(raw)
	require.NotNil(t, info)
	assert.Equal(t, "rate_limit_exceeded", info.Code)
	assert.Equal(t, "Rate limit exceeded.", info.Message)
	assert.Equal(t, "requests", info.Type)
	assert.True(t, info.Retryable)
	assert.Equal(t, time.Duration(0), info.RetryAfter)
}

func TestExtractAPIErrorInfo_RateLimitWithRetryAfter(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"rate_limit_exceeded","message":"Rate limit","type":"requests","retry_after":20}}`
	info := extractAPIErrorInfo(raw)
	require.NotNil(t, info)
	assert.True(t, info.Retryable)
	assert.Equal(t, 20*time.Second, info.RetryAfter)
}

func TestExtractAPIErrorInfo_FractionalRetryAfter(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"rate_limit_exceeded","message":"Rate limit","type":"requests","retry_after":2.5}}`
	info := extractAPIErrorInfo(raw)
	require.NotNil(t, info)
	assert.True(t, info.Retryable)
	assert.Equal(t, 2500*time.Millisecond, info.RetryAfter)
}

func TestExtractAPIErrorInfo_InsufficientQuota(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"insufficient_quota","message":"Quota exceeded.","type":"requests"}}`
	info := extractAPIErrorInfo(raw)
	require.NotNil(t, info)
	assert.True(t, info.Retryable)
}

func TestExtractAPIErrorInfo_ServerError(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"internal_error","message":"Server error.","type":"server_error"}}`
	info := extractAPIErrorInfo(raw)
	require.NotNil(t, info)
	assert.True(t, info.Retryable)
}

func TestExtractAPIErrorInfo_InvalidRequest(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"invalid_api_key","message":"Invalid API key.","type":"invalid_request_error"}}`
	info := extractAPIErrorInfo(raw)
	require.NotNil(t, info)
	assert.False(t, info.Retryable)
}

func TestExtractAPIErrorInfo_InvalidJSON(t *testing.T) {
	t.Parallel()

	info := extractAPIErrorInfo(`not-json`)
	assert.Nil(t, info)
}

func TestExtractAPIErrorInfo_EmptyJSON(t *testing.T) {
	t.Parallel()

	info := extractAPIErrorInfo(`{}`)
	assert.Nil(t, info)
}

// ─── extractAPIError ──────────────────────────────────────────────────────────

func TestExtractAPIError_NoError(t *testing.T) {
	t.Parallel()

	raw := `{"id":"chatcmpl-abc","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`
	code, msg, errType, retryable := extractAPIError(raw)
	assert.Empty(t, code)
	assert.Empty(t, msg)
	assert.Empty(t, errType)
	assert.False(t, retryable)
}

func TestExtractAPIError_RateLimit(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded, retry later.","type":"requests"}}`
	code, msg, errType, retryable := extractAPIError(raw)
	assert.Equal(t, "rate_limit_exceeded", code)
	assert.Equal(t, "Rate limit exceeded, retry later.", msg)
	assert.Equal(t, "requests", errType)
	assert.True(t, retryable)
}

func TestExtractAPIError_InsufficientQuota(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"insufficient_quota","message":"You have exceeded your quota.","type":"requests"}}`
	code, _, _, retryable := extractAPIError(raw)
	assert.Equal(t, "insufficient_quota", code)
	assert.True(t, retryable)
}

func TestExtractAPIError_ServerError(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"internal_error","message":"The server encountered an error.","type":"server_error"}}`
	code, msg, errType, retryable := extractAPIError(raw)
	assert.Equal(t, "internal_error", code)
	assert.Equal(t, "server_error", errType)
	assert.True(t, retryable)
	_ = msg
}

func TestExtractAPIError_InvalidRequest(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"invalid_api_key","message":"You didn't provide an API key.","type":"invalid_request_error"}}`
	code, msg, errType, retryable := extractAPIError(raw)
	assert.Equal(t, "invalid_api_key", code)
	assert.Equal(t, "invalid_request_error", errType)
	assert.False(t, retryable)
	_ = msg
}

func TestExtractAPIError_BadRequest(t *testing.T) {
	t.Parallel()

	raw := `{"error":{"code":"bad_request","message":"Invalid parameter.","type":"invalid_request_error"}}`
	code, msg, errType, retryable := extractAPIError(raw)
	assert.Equal(t, "bad_request", code)
	assert.Equal(t, "invalid_request_error", errType)
	assert.False(t, retryable)
	_ = msg
}

func TestExtractAPIError_InvalidJSON(t *testing.T) {
	t.Parallel()

	code, msg, errType, retryable := extractAPIError(`not-json`)
	assert.Empty(t, code)
	assert.Empty(t, msg)
	assert.Empty(t, errType)
	assert.False(t, retryable)
}

func TestExtractAPIError_EmptyJSON(t *testing.T) {
	t.Parallel()

	code, msg, errType, retryable := extractAPIError(`{}`)
	assert.Empty(t, code)
	assert.Empty(t, msg)
	assert.Empty(t, errType)
	assert.False(t, retryable)
}

// ─── classifyErrorTypeLabel ──────────────────────────────────────────────────

func TestClassifyErrorTypeLabel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "unknown", classifyErrorTypeLabel(nil))
	assert.Equal(t, "rate_limit", classifyErrorTypeLabel(&apiErrorInfo{Code: "rate_limit_exceeded"}))
	assert.Equal(t, "insufficient_quota", classifyErrorTypeLabel(&apiErrorInfo{Code: "insufficient_quota"}))
	assert.Equal(t, "server_error", classifyErrorTypeLabel(&apiErrorInfo{Type: "server_error"}))
	assert.Equal(t, "server_error", classifyErrorTypeLabel(&apiErrorInfo{Type: "server_error_with_details"}))
	assert.Equal(t, "non_retryable", classifyErrorTypeLabel(&apiErrorInfo{Code: "invalid_api_key", Type: "invalid_request_error"}))
}

// ─── classifySDKError ────────────────────────────────────────────────────────

func TestClassifySDKError_5xxRetryable(t *testing.T) {
	t.Parallel()

	apiErr := &openai.Error{
		StatusCode: http.StatusInternalServerError,
		Code:       "internal_error",
		Type:       "server_error",
		Message:    "Internal server error",
	}

	shouldRetry, err := classifySDKError(apiErr)
	assert.True(t, shouldRetry)
	assert.ErrorIs(t, err, errRetryable)

	var retryAfter *retryAfterError
	assert.True(t, errors.As(err, &retryAfter))
	assert.Contains(t, err.Error(), "server error (HTTP 500)")
}

func TestClassifySDKError_5xxRetryableServiceUnavailable(t *testing.T) {
	t.Parallel()

	apiErr := &openai.Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "service_unavailable",
		Type:       "server_error",
		Message:    "Service unavailable",
	}

	shouldRetry, err := classifySDKError(apiErr)
	assert.True(t, shouldRetry)
	assert.Contains(t, err.Error(), "server error (HTTP 503)")
}

func TestClassifySDKError_4xxNonRetryable(t *testing.T) {
	t.Parallel()

	apiErr := &openai.Error{
		StatusCode: http.StatusUnauthorized,
		Code:       "invalid_api_key",
		Type:       "invalid_request_error",
		Message:    "Invalid API key",
	}

	shouldRetry, err := classifySDKError(apiErr)
	assert.False(t, shouldRetry)
	assert.Contains(t, err.Error(), "HTTP 401")
}

func TestClassifySDKError_BadRequestNonRetryable(t *testing.T) {
	t.Parallel()

	apiErr := &openai.Error{
		StatusCode: http.StatusBadRequest,
		Code:       "bad_request",
		Type:       "invalid_request_error",
		Message:    "Bad request",
	}

	shouldRetry, err := classifySDKError(apiErr)
	assert.False(t, shouldRetry)
	assert.Contains(t, err.Error(), "HTTP 400")
}

func TestClassifySDKError_EOFRetryable(t *testing.T) {
	t.Parallel()

	shouldRetry, err := classifySDKError(io.EOF)
	assert.True(t, shouldRetry)
	assert.ErrorIs(t, err, errRetryable)
	assert.Contains(t, err.Error(), "empty response body")
}

func TestClassifySDKError_UnexpectedEOFRetryable(t *testing.T) {
	t.Parallel()

	shouldRetry, err := classifySDKError(io.ErrUnexpectedEOF)
	assert.True(t, shouldRetry)
	assert.ErrorIs(t, err, errRetryable)
	assert.Contains(t, err.Error(), "empty response body")
}

func TestClassifySDKError_JSONSyntaxErrorRetryable(t *testing.T) {
	t.Parallel()

	shouldRetry, err := classifySDKError(&json.SyntaxError{})
	assert.True(t, shouldRetry)
	assert.ErrorIs(t, err, errRetryable)
	assert.Contains(t, err.Error(), "malformed JSON response")
}

func TestClassifySDKError_NetworkErrorNonRetryable(t *testing.T) {
	t.Parallel()

	shouldRetry, err := classifySDKError(errors.New("connection refused"))
	assert.False(t, shouldRetry)
	assert.Contains(t, err.Error(), "connection refused")
}

// ─── parseDuration ────────────────────────────────────────────────────────────

func TestParseDuration(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1*time.Second, parseDuration("", 1*time.Second))
	assert.Equal(t, 1*time.Second, parseDuration("1s", 5*time.Second))
	assert.Equal(t, 500*time.Millisecond, parseDuration("500ms", 1*time.Second))
	assert.Equal(t, 2*time.Minute, parseDuration("2m", 1*time.Second))
	// Invalid string returns default.
	assert.Equal(t, 1*time.Second, parseDuration("not-a-duration", 1*time.Second))
}

// ─── calculateBackoff ─────────────────────────────────────────────────────────

func TestCalculateBackoff_FirstAttempt(t *testing.T) {
	t.Parallel()

	// Fixed seed for reproducibility.
	delay := calculateBackoff(1, 1*time.Second, 30*time.Second, nil)
	assert.GreaterOrEqual(t, delay, 1*time.Second)
	assert.LessOrEqual(t, delay, 1250*time.Millisecond) // 1s + 25% jitter
}

func TestCalculateBackoff_SecondAttempt(t *testing.T) {
	t.Parallel()

	delay := calculateBackoff(2, 1*time.Second, 30*time.Second, nil)
	assert.GreaterOrEqual(t, delay, 2*time.Second)
	assert.LessOrEqual(t, delay, 2500*time.Millisecond) // 2s + 25% jitter
}

func TestCalculateBackoff_CapsAtMax(t *testing.T) {
	t.Parallel()

	delay := calculateBackoff(10, 1*time.Second, 5*time.Second, nil)
	assert.GreaterOrEqual(t, delay, 5*time.Second)
	assert.LessOrEqual(t, delay, 6250*time.Millisecond) // 5s + 25% jitter
}

func TestCalculateBackoff_RetryAfter(t *testing.T) {
	t.Parallel()

	info := &apiErrorInfo{RetryAfter: 3 * time.Second}
	// With initial delay 1s, attempt 2 would give 2s exponential, but
	// Retry-After 3s should override.
	delay := calculateBackoff(2, 1*time.Second, 30*time.Second, info)
	assert.GreaterOrEqual(t, delay, 3*time.Second)
}

func TestCalculateBackoff_RetryAfterShorter(t *testing.T) {
	t.Parallel()

	info := &apiErrorInfo{RetryAfter: 1 * time.Second}
	// Attempt 4 would give 8s exponential, so Retry-After 1s shouldn't override.
	delay := calculateBackoff(4, 1*time.Second, 30*time.Second, info)
	assert.GreaterOrEqual(t, delay, 8*time.Second)
}

// ─── retryWithBackoff ─────────────────────────────────────────────────────────

func TestRetryWithBackoff_FirstAttemptSucceeds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	attempts := 0

	err := retryWithBackoff(ctx, 3, 10*time.Millisecond, 100*time.Millisecond, func() (bool, error) {
		attempts++

		return false, nil
	})

	assert.NoError(t, err)
	assert.Equal(t, 1, attempts)
}

func TestRetryWithBackoff_SecondAttemptSucceeds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	attempts := 0

	err := retryWithBackoff(ctx, 3, 10*time.Millisecond, 100*time.Millisecond, func() (bool, error) {
		attempts++
		if attempts == 1 {
			return true, fmt.Errorf("transient error")
		}

		return false, nil
	})

	assert.NoError(t, err)
	assert.Equal(t, 2, attempts)
}

func TestRetryWithBackoff_AllAttemptsExhausted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	attempts := 0

	err := retryWithBackoff(ctx, 3, 10*time.Millisecond, 100*time.Millisecond, func() (bool, error) {
		attempts++

		return true, fmt.Errorf("persistent error")
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 3 retry attempts exhausted")
	assert.Equal(t, 3, attempts)
}

func TestRetryWithBackoff_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	// Start the retry loop in a goroutine because it will block on the backoff.
	errCh := make(chan error, 1)

	go func() {
		errCh <- retryWithBackoff(ctx, 3, 5*time.Second, 30*time.Second, func() (bool, error) {
			attempts++

			return true, fmt.Errorf("transient error")
		})
	}()

	// Wait for the first attempt to complete.
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-errCh
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts)
}

// ─── buildMessages ────────────────────────────────────────────────────────────

func TestBuildMessages_Empty(t *testing.T) {
	assert.Empty(t, buildMessages(nil, ""))
	assert.Empty(t, buildMessages([]agentpkg.Message{}, ""))
}

func TestBuildMessages_SystemAndUser(t *testing.T) {
	history := []agentpkg.Message{
		{Role: agentpkg.RoleSystem, Content: "You are helpful."},
		{Role: agentpkg.RoleUser, Content: "Hello!"},
	}
	msgs := buildMessages(history, "")
	require.Len(t, msgs, 2)
	assert.NotNil(t, msgs[0].OfSystem)
	assert.NotNil(t, msgs[1].OfUser)
}

func TestBuildMessages_AgentMessage(t *testing.T) {
	history := []agentpkg.Message{
		{Role: agentpkg.RoleAgent, Content: "Hi there!"},
	}
	msgs := buildMessages(history, "")
	require.Len(t, msgs, 1)
	assert.NotNil(t, msgs[0].OfAssistant)
	assert.Nil(t, msgs[0].OfAssistant.ToolCalls)
}

func TestBuildMessages_ToolRoundTrip(t *testing.T) {
	// Under the new protocol, tool calls are stored in RoleAgent messages.
	history := []agentpkg.Message{
		{
			Role:       agentpkg.RoleAgent,
			IsComplete: true,
			ToolCalls: []agentpkg.ToolCall{
				{FuncName: "my_func", Params: map[string]interface{}{"arg": "val"}},
			},
		},
		{
			Role:       agentpkg.RoleToolResult,
			ToolResult: []map[string]interface{}{{"result": "ok"}},
		},
	}
	msgs := buildMessages(history, "")
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
	// Under the new protocol, tool calls are stored in RoleAgent messages.
	history := []agentpkg.Message{
		{
			Role:       agentpkg.RoleAgent,
			IsComplete: true,
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
	msgs := buildMessages(history, "")
	// 1 assistant with 2 tool calls + 2 tool result messages
	require.Len(t, msgs, 3)
	assert.NotNil(t, msgs[0].OfAssistant)
	assert.Len(t, msgs[0].OfAssistant.ToolCalls, 2)
	assert.NotNil(t, msgs[1].OfTool)
	assert.NotNil(t, msgs[2].OfTool)
}

func TestBuildMessages_ReasoningInjection(t *testing.T) {
	history := []agentpkg.Message{
		{Role: agentpkg.RoleAgent, Content: "I think first.", Thoughts: "internal reasoning"},
	}
	// Without injection field: extra fields should not be set.
	msgs := buildMessages(history, "")
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].OfAssistant)
	assert.Empty(t, msgs[0].OfAssistant.ExtraFields())

	// With injection field: Thoughts should appear in extra fields.
	msgs = buildMessages(history, "reasoning")
	require.Len(t, msgs, 1)
	require.NotNil(t, msgs[0].OfAssistant)
	assert.Contains(t, msgs[0].OfAssistant.ExtraFields(), "reasoning")
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

// ─── buildToolCalls ──────────────────────────────────────────────────────────

func TestBuildToolCalls_Single(t *testing.T) {
	toolCalls := []openai.ChatCompletionMessageToolCallUnion{
		{
			Function: openai.ChatCompletionMessageFunctionToolCallFunction{
				Name:      "do_thing",
				Arguments: `{"x": 1}`,
			},
		},
	}
	calls := buildToolCalls(toolCalls)
	require.Len(t, calls, 1)
	assert.Equal(t, "do_thing", calls[0].FuncName)
	assert.Equal(t, float64(1), calls[0].Params["x"])
}

func TestBuildToolCalls_Multiple(t *testing.T) {
	toolCalls := []openai.ChatCompletionMessageToolCallUnion{
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "a", Arguments: `{"k":"v1"}`}},
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "b", Arguments: `{"k":"v2"}`}},
	}
	calls := buildToolCalls(toolCalls)
	require.Len(t, calls, 2)
	assert.Equal(t, "a", calls[0].FuncName)
	assert.Equal(t, "b", calls[1].FuncName)
}

func TestBuildToolCalls_InvalidJSON(t *testing.T) {
	toolCalls := []openai.ChatCompletionMessageToolCallUnion{
		{Function: openai.ChatCompletionMessageFunctionToolCallFunction{Name: "fn", Arguments: `not-json`}},
	}
	calls := buildToolCalls(toolCalls)
	require.Len(t, calls, 1)
	assert.Equal(t, "fn", calls[0].FuncName)
	assert.Nil(t, calls[0].Params)
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

// mockOpenAIServerWithCode returns an httptest.Server that responds with a
// specific HTTP status code and body.
func mockOpenAIServerWithCode(statusCode int, body interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

// mockEmptyBodyServer returns an httptest.Server that responds with HTTP 200
// and an empty body.
func mockEmptyBodyServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write nothing — empty body.
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

// openAIErrorBody returns a mock HTTP 200 response body containing an OpenAI
// API error (e.g. rate limit, auth failure) instead of valid completion data.
func openAIErrorBody(errCode, errMsg, errType string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{
			"code":    errCode,
			"message": errMsg,
			"type":    errType,
		},
	}
}

func setupDriverWithServer(ts *httptest.Server, extraEngineOpts map[string]interface{}) (io.Reader, *io.PipeWriter) {
	outReader, outWriter := io.Pipe()
	engineOpts := make(map[string]interface{})
	for k, v := range defaultEngineConfig {
		engineOpts[k] = v
	}
	for k, v := range extraEngineOpts {
		engineOpts[k] = v
	}
	engineOpts["base_url"] = ts.URL + "/"

	driver = &dipper.Driver{
		Options: map[string]interface{}{
			"data": map[string]interface{}{
				"engines": map[string]interface{}{
					"test-engine": engineOpts,
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

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

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

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	sendToModel(testMessage("test-engine"))

	result := <-done
	require.NotNil(t, result)

	payloadMap, ok := result.Payload.(map[string]interface{})
	require.True(t, ok)
	msgMap, ok := payloadMap["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, agentpkg.RoleAgent, msgMap["Role"])
	assert.True(t, msgMap["is_complete"].(bool))

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

// ─── sendToModel API error handling ──────────────────────────────────────────

func TestSendToModel_RateLimitError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Simulate HTTP 200 with a rate-limit error body (no choices).
	ts := mockOpenAIServer(openAIErrorBody("rate_limit_exceeded", "Rate limit exceeded.", "requests"))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// With retry_max_attempts=1, the retry loop exhausts and sends an error message.
	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

func TestSendToModel_InvalidRequestError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Simulate HTTP 200 with a non-retryable error body.
	ts := mockOpenAIServer(openAIErrorBody("invalid_api_key", "Invalid API key.", "invalid_request_error"))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// This should panic (caught by defer) and send an error message to the session.
	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "sess-test", result.Labels["agent_session_id"])
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "invalid_api_key")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

func TestSendToModel_ServerError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Simulate HTTP 200 with a server error body.
	ts := mockOpenAIServer(openAIErrorBody("internal_error", "Server error.", "server_error"))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// Server errors are retryable; with retry_max_attempts=1, retry exhausted sends error.
	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── sendToModel empty / blank response handling ────────────────────────────

func TestSendToModel_EmptyChoicesWithoutError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// HTTP 200 with empty choices and no error field.
	body := map[string]interface{}{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1234567890, "model": "gpt-4o",
		"choices": []interface{}{},
		"usage":   map[string]interface{}{},
	}
	ts := mockOpenAIServer(body)
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// This should send an error to the session (no panic).
	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "no choices")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

func TestSendToModel_EmptyResponseBody(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// HTTP 200 with completely empty body (no content at all).
	ts := mockEmptyBodyServer()
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "1ms",
		"retry_max_delay":     "5ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// Empty body is retryable; all retries exhausted sends error to session.
	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── sendToModel SDK HTTP error handling ────────────────────────────────────

func TestSendToModel_HTTPServerErrorRetryThenSucceed(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Server responds with HTTP 500 on first call, then 200 on retry.
	var callCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(callCount.Add(1))
		w.Header().Set("Content-Type", "application/json")

		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "internal_error",
					"message": "Server error",
					"type":    "server_error",
				},
			})
		} else {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(openAITextBody("Success after 500!"))
		}
	}))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "1ms",
		"retry_max_delay":     "5ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		payloadMap := result.Payload.(map[string]interface{})
		msgMap := payloadMap["message"].(map[string]interface{})
		assert.Equal(t, "Success after 500!", msgMap["content"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for success after HTTP 500 retry")
	}
}

func TestSendToModel_HTTPClientErrorNonRetryable(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Server responds with HTTP 401 (auth error).
	ts := mockOpenAIServerWithCode(http.StatusUnauthorized, map[string]interface{}{
		"error": map[string]interface{}{
			"code":    "invalid_api_key",
			"message": "Invalid API key",
			"type":    "invalid_request_error",
		},
	})
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "HTTP 401")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── sendToModel context cancellation during retry ──────────────────────────

// ─── sendToModel retry integration ────────────────────────────────────────────

// counterServer returns an httptest.Server that alternates responses based on
// a request counter.  The fn receives the current count (1-based) and returns
// the response body for that request.
func counterServer(fn func(n int) map[string]interface{}) *httptest.Server {
	var counter atomic.Int32

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(counter.Add(1))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fn(n))
	}))
}

func TestSendToModel_RetryThenSucceed(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	ts := counterServer(func(n int) map[string]interface{} {
		if n == 1 {
			return openAIErrorBody("rate_limit_exceeded", "Rate limit.", "requests")
		}

		return openAITextBody("Success on retry!")
	})
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "10ms",
		"retry_max_delay":     "100ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		payloadMap, ok := result.Payload.(map[string]interface{})
		require.True(t, ok)
		msgMap, ok := payloadMap["message"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "Success on retry!", msgMap["content"])
		assert.True(t, msgMap["is_complete"].(bool))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for successful response after retry")
	}
}

func TestSendToModel_RetryAllExhausted(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Server always returns rate limit error.
	ts := mockOpenAIServer(openAIErrorBody("rate_limit_exceeded", "Always rate limited.", "requests"))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "5ms",
		"retry_max_delay":     "50ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// All retries exhausted — error message sent to session.
	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── sendToModel non-retryable doesn't retry ────────────────────────────────

func TestSendToModel_NonRetryableErrorNoRetry(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Count how many times the server is called.
	var callCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIErrorBody("invalid_api_key", "Invalid API key.", "invalid_request_error"))
	}))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "10ms",
		"retry_max_delay":     "100ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	assert.NotPanics(t, func() { sendToModel(testMessage("test-engine")) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "error", result.Labels["status"])
		// Should have only been called once (no retry).
		assert.Equal(t, int32(1), callCount.Load())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
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
			fmt.Fprintf(w, "data: %s\n\n", b)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

func streamingChunk(content, finishReason string) map[string]interface{} {
	delta := map[string]interface{}{"role": "assistant", "content": content}
	choice := map[string]interface{}{"index": 0, "delta": delta}

	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}

	return map[string]interface{}{
		"id": "id1", "object": "chat.completion.chunk", "created": 1234567890, "model": "gpt-4o",
		"choices": []interface{}{choice},
	}
}

func streamingToolCallChunk(toolID, toolName, args, finishReason string) map[string]interface{} {
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
		"id": "id2", "object": "chat.completion.chunk", "created": 1234567890, "model": "gpt-4o",
		"choices": []interface{}{choice},
	}
}

func setupStreamingDriverWithServer(ts *httptest.Server, extraEngineOpts map[string]interface{}) (io.Reader, *io.PipeWriter) {
	outReader, outWriter := io.Pipe()
	engineOpts := make(map[string]interface{})
	for k, v := range defaultEngineConfig {
		engineOpts[k] = v
	}
	for k, v := range extraEngineOpts {
		engineOpts[k] = v
	}
	engineOpts["base_url"] = ts.URL + "/"

	driver = &dipper.Driver{
		Options: map[string]interface{}{
			"data": map[string]interface{}{
				"engines": map[string]interface{}{
					"stream-engine": engineOpts,
				},
			},
		},
		Out: outWriter,
	}

	return outReader, outWriter
}

func streamingMessage() *dipper.Message {
	return &dipper.Message{
		Channel: "rpc",
		Subject: "send_to_model",
		Labels:  map[string]string{"agent_session_id": "sess-stream"},
		Payload: map[string]interface{}{
			"engine":        "stream-engine",
			"history":       []interface{}{},
			"should_stream": true,
		},
	}
}

func TestSendToModel_StreamingTextResponse(t *testing.T) {
	chunks := []map[string]interface{}{
		streamingChunk("Hello", ""),
		streamingChunk(", world", ""),
		streamingChunk("", "stop"),
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, nil)
	defer outWriter.Close()

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

	sendToModel(streamingMessage())

	outWriter.Close()
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
		streamingToolCallChunk("call_xyz", "search_web", `{"query":"go testing"}`, ""),
		streamingToolCallChunk("", "", "", "tool_calls"),
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	sendToModel(streamingMessage())

	result := <-done
	require.NotNil(t, result)

	payloadMap, ok := result.Payload.(map[string]interface{})
	require.True(t, ok)
	msgMap, ok := payloadMap["message"].(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, agentpkg.RoleAgent, msgMap["Role"])
	assert.True(t, msgMap["is_complete"].(bool))

	rawCalls, ok := msgMap["ToolCalls"].([]interface{})
	require.True(t, ok)
	require.Len(t, rawCalls, 1)

	call := rawCalls[0].(map[string]interface{})
	assert.Equal(t, "search_web", call["FuncName"])
}

// ─── sendToModel streaming API error handling ────────────────────────────────

// streamingErrorChunk returns an SSE data chunk containing an OpenAI API error
// object.  This simulates a streaming endpoint returning an error event inline.
func streamingErrorChunk(errCode, errMsg, errType string) map[string]interface{} {
	return map[string]interface{}{
		"error": map[string]interface{}{
			"code":    errCode,
			"message": errMsg,
			"type":    errType,
		},
	}
}

func TestSendToModel_StreamingRateLimitError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Simulate a stream that returns a rate-limit error as the first event.
	chunks := []map[string]interface{}{
		streamingErrorChunk("rate_limit_exceeded", "Rate limit exceeded.", "requests"),
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// Retryable: with retry_max_attempts=1, retry exhausted sends error via Panicf.
	assert.NotPanics(t, func() {
		sendToModel(streamingMessage())
	})

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

func TestSendToModel_StreamingInvalidAuthError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Simulate a stream that returns an auth error.
	chunks := []map[string]interface{}{
		streamingErrorChunk("invalid_api_key", "Invalid API key.", "invalid_request_error"),
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// Non-retryable: should send an error message to the session via deferred recovery.
	assert.NotPanics(t, func() {
		sendToModel(streamingMessage())
	})

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "invalid_api_key")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── sendToModel streaming with SSE error events ────────────────────────────

func TestSendToModel_StreamingEmptyChoicesNoError(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// Stream that returns no choices and no error in the accumulated result.
	chunks := []map[string]interface{}{
		// A chunk with no choices (just usage data)
		{
			"id": "id1", "object": "chat.completion.chunk", "created": 1234567890, "model": "gpt-4o",
			"choices": []interface{}{},
		},
	}
	ts := mockSSEServer(chunks)
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	assert.NotPanics(t, func() { sendToModel(streamingMessage()) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "empty_response")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

func TestSendToModel_StreamingSSEErrorEvent(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// This test simulates an SSE error event (not a data: line with error JSON)
	// by sending an event: error line with error data.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Send an error event with rate limit error data.
		errBody, _ := json.Marshal(openAIErrorBody("rate_limit_exceeded", "Rate limit.", "requests"))
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", errBody)
		fmt.Fprint(w, "data: [DONE]\n\n")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, nil)
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// With retry_max_attempts=1, retry exhausted sends error via Panicf.
	assert.NotPanics(t, func() { sendToModel(streamingMessage()) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── sendToModel streaming retry integration ─────────────────────────────────

func TestSendToModel_StreamingRetryThenSucceed(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// A counter server that returns an error on the first connection and a
	// successful stream on subsequent connections.
	var connectCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(connectCount.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		if n == 1 {
			// First connection: return a rate-limit error.
			errBody, _ := json.Marshal(openAIErrorBody("rate_limit_exceeded", "Rate limit.", "requests"))
			fmt.Fprintf(w, "data: %s\n\n", errBody)
		} else {
			// Subsequent connections: return a successful streaming response.
			chunk1, _ := json.Marshal(streamingChunk("Hello after retry!", ""))
			fmt.Fprintf(w, "data: %s\n\n", chunk1)
			chunk2, _ := json.Marshal(streamingChunk("", "stop"))
			fmt.Fprintf(w, "data: %s\n\n", chunk2)
		}

		fmt.Fprint(w, "data: [DONE]\n\n")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "10ms",
		"retry_max_delay":     "100ms",
	})
	defer outWriter.Close()

	// Collect all messages.
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

	assert.NotPanics(t, func() { sendToModel(streamingMessage()) })
	outWriter.Close()
	<-done

	require.GreaterOrEqual(t, len(msgs), 1, "expected at least one message after streaming retry")

	last := msgs[len(msgs)-1]
	payloadMap := last.Payload.(map[string]interface{})
	msgMap := payloadMap["message"].(map[string]interface{})
	assert.True(t, msgMap["is_complete"].(bool))
	assert.Contains(t, msgMap["content"], "Hello after retry!")
}

func TestSendToModel_StreamingMaxRetriesExhausted(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	// All connections return rate limit error.
	var connectCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		errBody, _ := json.Marshal(openAIErrorBody("rate_limit_exceeded", "Rate limit.", "requests"))
		fmt.Fprintf(w, "data: %s\n\n", errBody)
		fmt.Fprint(w, "data: [DONE]\n\n")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "1ms",
		"retry_max_delay":     "5ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	// All retries exhausted — error sent via Panicf deferred recovery.
	assert.NotPanics(t, func() { sendToModel(streamingMessage()) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "agentbus", result.Channel)
		assert.Equal(t, "receive", result.Subject)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "retry exhausted")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error message")
	}
	assert.Equal(t, int32(3), connectCount.Load(), "should have attempted 3 times")
}

func TestSendToModel_StreamingNonRetryableNoRetry(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	var connectCount atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connectCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		errBody, _ := json.Marshal(openAIErrorBody("invalid_api_key", "Invalid API key.", "invalid_request_error"))
		fmt.Fprintf(w, "data: %s\n\n", errBody)
		fmt.Fprint(w, "data: [DONE]\n\n")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	outReader, outWriter := setupStreamingDriverWithServer(ts, map[string]interface{}{
		"retry_max_attempts":  3,
		"retry_initial_delay": "10ms",
		"retry_max_delay":     "100ms",
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	assert.NotPanics(t, func() { sendToModel(streamingMessage()) })

	select {
	case result := <-done:
		require.NotNil(t, result)
		assert.Equal(t, "error", result.Labels["status"])
		assert.Contains(t, result.Labels["reason"], "invalid_api_key")
		// Should have only been called once (no retry).
		assert.Equal(t, int32(1), connectCount.Load())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error message")
	}
}

// ─── newOpenAIClient / custom headers ───────────────────────────────────────

func TestNewOpenAIClient_WithCustomHeaders(t *testing.T) {
	// Create a mock server that records the headers it receives.
	var capturedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer ts.Close()

	cfg := engineConfig{
		Model:   "gpt-4o",
		APIKey:  "test-key-123",
		BaseURL: ts.URL + "/",
		Headers: map[string]string{
			"X-Custom-Header":  "custom-value",
			"X-Another-Header": "another-value",
		},
	}

	client := newOpenAIClient(cfg)
	require.NotNil(t, client)

	// Make a simple chat completion request to trigger the HTTP call.
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
	})
	require.NoError(t, err)

	// Verify custom headers were sent.
	assert.Equal(t, "custom-value", capturedHeaders.Get("X-Custom-Header"), "custom header X-Custom-Header should be present")
	assert.Equal(t, "another-value", capturedHeaders.Get("X-Another-Header"), "custom header X-Another-Header should be present")
}

func TestNewOpenAIClient_NilHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer ts.Close()

	cfg := engineConfig{
		Model:   "gpt-4o",
		APIKey:  "test-key-456",
		BaseURL: ts.URL + "/",
		// Headers is nil — should not cause any errors.
	}

	client := newOpenAIClient(cfg)
	require.NotNil(t, client)

	// Making a request with nil headers should succeed without panics or errors.
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
	})
	require.NoError(t, err)
}

func TestNewOpenAIClient_EmptyHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer ts.Close()

	cfg := engineConfig{
		Model:   "gpt-4o",
		APIKey:  "test-key-789",
		BaseURL: ts.URL + "/",
		Headers: map[string]string{}, // empty map — should work fine.
	}

	client := newOpenAIClient(cfg)
	require.NotNil(t, client)

	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
	})
	require.NoError(t, err)
}

func TestNewOpenAIClient_CustomHeadersDontConflictWithAuthorization(t *testing.T) {
	var capturedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer ts.Close()

	cfg := engineConfig{
		Model:   "gpt-4o",
		APIKey:  "sk-proj-test-key-12345",
		BaseURL: ts.URL + "/",
		Headers: map[string]string{
			"X-Debug-Mode": "true",
			"X-Request-ID": "req-abc-123",
		},
	}

	client := newOpenAIClient(cfg)
	require.NotNil(t, client)

	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-4o",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")},
	})
	require.NoError(t, err)

	// Verify Authorization header is still present and correct.
	authHeader := capturedHeaders.Get("Authorization")
	assert.Contains(t, authHeader, "Bearer sk-proj-test-key-12345",
		"Authorization header should contain the API key")

	// Verify custom headers are also present alongside Authorization.
	assert.Equal(t, "true", capturedHeaders.Get("X-Debug-Mode"))
	assert.Equal(t, "req-abc-123", capturedHeaders.Get("X-Request-ID"))
}

// ─── sendToModel integration with custom headers ────────────────────────────

func TestSendToModel_WithCustomHeaders(t *testing.T) {
	// Note: NOT t.Parallel() because tests share the global driver variable.

	var capturedHeaders http.Header

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAITextBody("Hello with custom headers!"))
	}))
	defer ts.Close()

	outReader, outWriter := setupDriverWithServer(ts, map[string]interface{}{
		"headers": map[string]interface{}{
			"X-Custom-Header": "custom-value",
			"X-Request-ID":    "req-abc-123",
		},
	})
	defer outWriter.Close()

	done := make(chan *dipper.Message, 1)
	go func() { done <- dipper.FetchMessage(outReader) }()

	sendToModel(testMessage("test-engine"))

	result := <-done
	require.NotNil(t, result)

	// Verify the response was successful.
	payloadMap, ok := result.Payload.(map[string]interface{})
	require.True(t, ok)
	msgMap, ok := payloadMap["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "Hello with custom headers!", msgMap["content"])

	// Verify custom headers were sent with the request.
	assert.Equal(t, "custom-value", capturedHeaders.Get("X-Custom-Header"))
	assert.Equal(t, "req-abc-123", capturedHeaders.Get("X-Request-ID"))

	// Verify Authorization header is still intact.
	authHeader := capturedHeaders.Get("Authorization")
	assert.Contains(t, authHeader, "Bearer test-key")
}

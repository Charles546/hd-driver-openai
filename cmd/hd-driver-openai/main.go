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
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"

	agentpkg "github.com/honeydipper/honeydipper/v4/pkg/agent"
	"github.com/honeydipper/honeydipper/v4/pkg/dipper"
	"github.com/mitchellh/mapstructure"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/tidwall/gjson"
)

var driver *dipper.Driver

// engineConfig holds per-engine connection and model settings loaded from
// driver.Options["data.engines.<name>"].
type engineConfig struct {
	Model   string            `mapstructure:"model"`
	APIKey  string            `mapstructure:"api_key"`
	BaseURL string            `mapstructure:"base_url"`
	Headers map[string]string `mapstructure:"headers"`

	// Retry configuration for transient API errors (e.g. 429 rate limit).
	RetryMaxAttempts  int    `mapstructure:"retry_max_attempts"`
	RetryInitialDelay string `mapstructure:"retry_initial_delay"`
	RetryMaxDelay     string `mapstructure:"retry_max_delay"`
}

// apiErrorInfo holds structured information about an API error detected in a
// response body (typically returned as HTTP 200 with an "error" JSON object).
type apiErrorInfo struct {
	Code       string
	Message    string
	Type       string
	Retryable  bool
	RetryAfter time.Duration // suggested delay from the response (0 if not set)
}

// errRetryable is a static sentinel that wraps a retryable transient error.
var errRetryable = errors.New("retryable API error")

// errNonRetryable is a static sentinel for non-retryable API errors.
var errNonRetryable = errors.New("non-retryable API error")

// retryAfterError wraps a retryable error with an optional Retry-After hint.
type retryAfterError struct {
	Duration time.Duration
	Err      error
}

func (e *retryAfterError) Error() string { return e.Err.Error() }

func (e *retryAfterError) Unwrap() error { return e.Err }

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
//
//nolint:funlen // core orchestration function; splitting would reduce clarity.
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

	// Build the OpenAI request.
	params := openai.ChatCompletionNewParams{}
	modelDataRaw, _ := dipper.GetMapData(msg.Payload, "model_data")
	if modelDataRaw != nil {
		jstring := dipper.Must(json.Marshal(modelDataRaw)).([]byte)
		dipper.Must(json.Unmarshal(jstring, &params))
		dipper.Logger.Infof("[openai] send_to_model using model_data params session=%s: %+v", sessionID, params)
	}

	reasoningExtraction, _ := dipper.GetMapDataStr(msg.Payload, "agent_settings.reasoning.extraction_field")
	reasoningInjection, _ := dipper.GetMapDataStr(msg.Payload, "agent_settings.reasoning.injection_field")

	params.Model = cfg.Model
	params.Messages = buildMessages(history, reasoningInjection)
	if openaiTools := buildTools(tools); len(openaiTools) > 0 {
		params.Tools = openaiTools
	}

	if extraFields, _ := dipper.GetMapData(msg.Payload, "model_data.extra_fields"); extraFields != nil {
		if m, ok := extraFields.(map[string]interface{}); ok {
			params.SetExtraFields(m)
		}
	}

	// Per-request options
	reqOpts := []option.RequestOption{}

	// Obtain a context that is cancelled when the driver shuts down.
	ctx, cancel := driver.GetContext(msg)
	defer cancel()

	client := newOpenAIClient(cfg)

	// Parse retry configuration with sensible defaults.
	retryMaxAttempts := cfg.RetryMaxAttempts
	if retryMaxAttempts <= 0 {
		retryMaxAttempts = 3
	}

	retryInitialDelay := parseDuration(cfg.RetryInitialDelay, 1*time.Second)
	retryMaxDelay := parseDuration(cfg.RetryMaxDelay, 30*time.Second)

	shouldStream, _ := dipper.GetMapDataBool(msg.Payload, "should_stream")
	if shouldStream {
		reqOpts = append(reqOpts, option.WithJSONSet("stream_options", map[string]interface{}{"include_usage": true}))
		sendToModelStreaming(ctx, client, params, reqOpts, sessionID, reasoningExtraction,
			retryMaxAttempts, retryInitialDelay, retryMaxDelay)

		return
	}

	// Non-streaming: retry loop for retryable API errors using exponential backoff.
	var lastErrInfo *apiErrorInfo

	err := retryWithBackoff(ctx, retryMaxAttempts, retryInitialDelay, retryMaxDelay, func() (bool, error) {
		completion, err := client.Chat.Completions.New(ctx, params, reqOpts...)
		if err != nil {
			// SDK-level error (network, timeout, or HTTP status >=400).
			return classifySDKError(err)
		}

		lastErrInfo = extractAPIErrorInfo(completion.RawJSON())
		if lastErrInfo == nil {
			// No API-level error detected.  Check for empty choices as a safety net.
			if len(completion.Choices) == 0 {
				dipper.Logger.Warningf("[openai] no choices in response session=%s: raw=%s",
					sessionID, completion.RawJSON())

				// Return as retryable so the retry loop can retry the request.
				return true, fmt.Errorf("%w: response contains no choices", errRetryable)
			}

			// Check for empty response (stop/length with no content and no tool calls).
			if isResponseEmpty(completion) {
				dipper.Logger.Warningf("[openai] empty response session=%s finish_reason=%s raw=%s",
					sessionID, completion.Choices[0].FinishReason, completion.RawJSON())

				return true, &retryAfterError{
					Err: fmt.Errorf("%w: empty response (finish_reason=%s)",
						errRetryable, completion.Choices[0].FinishReason),
				}
			}

			// Success.
			handleIncomingMessage(completion, sessionID, reasoningExtraction)

			return false, nil
		}

		// Log the raw error body at debug level for troubleshooting.
		dipper.Logger.Debugf("[openai] raw error body session=%s: code=%s type=%s raw=%s",
			sessionID, lastErrInfo.Code, lastErrInfo.Type, completion.RawJSON())

		errTypeLabel := classifyErrorTypeLabel(lastErrInfo)
		dipper.Logger.Warningf("[openai] API error (%s) session=%s: code=%s type=%s message=%s",
			errTypeLabel, sessionID, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)

		if !lastErrInfo.Retryable {
			return false, fmt.Errorf("%w: code=%s type=%s message=%s",
				errNonRetryable, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)
		}

		// Retryable: return a retryAfterError with the optional Retry-After hint.
		return true, &retryAfterError{
			Duration: lastErrInfo.RetryAfter,
			Err:      fmt.Errorf("%w: code=%s type=%s message=%s", errRetryable, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message),
		}
	})
	if err != nil {
		// Determine if the final error is retryable (exhausted) or non-retryable.
		var retryErr *retryAfterError

		switch {
		case errors.As(err, &retryErr):
			// All retries exhausted — send error to the session via deferred recovery.
			dipper.Logger.Panicf("[openai] retry exhausted session=%s: %v", sessionID, err)

		case errors.Is(err, errNonRetryable):
			// Non-retryable: let the deferred recovery send an error to the session.
			dipper.Logger.Infof("[openai] non-retryable error session=%s: %v", sessionID, err)
			dipper.Logger.Panicf("[openai] non-retryable API error session=%s: %v", sessionID, err)

		default:
			// Other errors (e.g. SDK errors that weren't classified): let deferred recovery handle.
			dipper.Logger.Panicf("[openai] request error session=%s: %v", sessionID, err)
		}
	}
}

// classifySDKError inspects an SDK-level error and decides whether to retry.
// HTTP 5xx errors are retryable; HTTP 4xx, network/timeout, and other errors
// are not.  Empty/blank response bodies (EOF when parsing JSON) are also
// treated as retryable.
func classifySDKError(err error) (bool, error) {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode >= http.StatusInternalServerError || apiErr.StatusCode == http.StatusTooManyRequests {
			// HTTP 5xx — retryable server error.
			return true, &retryAfterError{
				Err: fmt.Errorf("%w: server error (HTTP %d) code=%s type=%s message=%s",
					errRetryable, apiErr.StatusCode, apiErr.Code, apiErr.Type, apiErr.Message),
			}
		}

		// HTTP 4xx — non-retryable client error.
		return false, fmt.Errorf("API call failed with HTTP %d (type=%s code=%s): %w",
			apiErr.StatusCode, apiErr.Type, apiErr.Code, err)
	}

	// Check for empty/blank response body (EOF when parsing JSON).
	// Some API providers return HTTP 200 with an empty body instead of a proper
	// JSON response, which causes the SDK to fail with an EOF or JSON parse error.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true, &retryAfterError{
			Err: fmt.Errorf("%w: empty response body (EOF)", errRetryable),
		}
	}

	// Check for JSON syntax errors which may indicate a blank/truncated body.
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true, &retryAfterError{
			Err: fmt.Errorf("%w: malformed JSON response: %w", errRetryable, syntaxErr),
		}
	}

	// Non-API errors (network, timeout, etc.) — treat as non-retryable.
	return false, fmt.Errorf("API call failed: %w", err)
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

// extractAPIErrorInfo inspects a raw JSON response body for an OpenAI API error
// object and returns structured error information.  Returns nil when no error
// is present.
//
// Retryable errors include those with code "rate_limit_exceeded" or
// "insufficient_quota", and server-error types (5xx).  All other error codes
// (e.g. 400, 401, 403) are treated as non-retryable.
func extractAPIErrorInfo(rawJSON string) *apiErrorInfo {
	if !gjson.Valid(rawJSON) {
		return nil
	}

	errVal := gjson.Get(rawJSON, "error")
	if !errVal.Exists() {
		return nil
	}

	info := &apiErrorInfo{
		Code:    gjson.Get(rawJSON, "error.code").String(),
		Message: gjson.Get(rawJSON, "error.message").String(),
		Type:    gjson.Get(rawJSON, "error.type").String(),
	}

	// Extract optional retry_after from the error body (seconds, may be fractional).
	retryAfter := gjson.Get(rawJSON, "error.retry_after")
	if retryAfter.Exists() {
		info.RetryAfter = time.Duration(retryAfter.Float() * float64(time.Second))
	}

	// Rate-limit errors are retryable and workaround some openrouter provider return errors.
	if info.Code == "rate_limit_exceeded" || info.Code == "insufficient_quota" || info.Message == "Provider returned error" {
		info.Retryable = true

		return info
	}

	// Server-side error types (5xx-equivalent) are retryable.
	if strings.HasPrefix(info.Type, "server_error") {
		info.Retryable = true

		return info
	}

	// All other errors (e.g. invalid_request_error, 400, 401, 403) are not retryable.
	return info
}

// extractAPIError inspects a raw JSON response body for an OpenAI API error
// object.  It returns the error code, message, type, and whether the error is
// considered retryable.  When no error is found, all return values are zero-valued.
//
// Deprecated: Use extractAPIErrorInfo which returns structured error info
// including the optional retry_after duration.
func extractAPIError(rawJSON string) (code, message, errType string, retryable bool) {
	info := extractAPIErrorInfo(rawJSON)
	if info == nil {
		return "", "", "", false
	}

	return info.Code, info.Message, info.Type, info.Retryable
}

// sendToModelStreaming calls the OpenAI streaming endpoint, emits each content
// delta as a non-complete agentbus message, and sends a final complete message
// once the stream closes.  Tool-call responses are accumulated and sent as a
// single tool message at the end.
//
// If a retryable API error is detected before any content has been streamed,
// the function will retry the entire streaming request with exponential backoff.
func sendToModelStreaming(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams,
	reqOpts []option.RequestOption, sessionID string, reasoningExtraction string,
	retryMaxAttempts int, retryInitialDelay, retryMaxDelay time.Duration,
) {
	var lastErrInfo *apiErrorInfo
	contentDelivered := false

	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 {
			// Only retry if no content was delivered yet.
			if contentDelivered {
				dipper.Logger.Panicf("[openai] streaming API error after content delivered session=%s: code=%s type=%s message=%s",
					sessionID, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)

				return
			}

			delay := calculateBackoff(attempt, retryInitialDelay, retryMaxDelay, lastErrInfo)

			dipper.Logger.Infof("[openai] streaming retry attempt=%d/%d delay=%v session=%s",
				attempt+1, retryMaxAttempts, delay, sessionID)

			select {
			case <-ctx.Done():
				dipper.Logger.Panicf("[openai] context cancelled during streaming retry backoff session=%s err=%v",
					sessionID, ctx.Err())

				return

			case <-time.After(delay):
			}
		}

		contentDelivered, lastErrInfo = runStream(ctx, client, params, reqOpts, sessionID, reasoningExtraction)
		if lastErrInfo == nil {
			return // success
		}

		errTypeLabel := classifyErrorTypeLabel(lastErrInfo)
		dipper.Logger.Warningf("[openai] streaming API error (%s) attempt=%d/%d session=%s: code=%s type=%s message=%s",
			errTypeLabel, attempt+1, retryMaxAttempts, sessionID, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)

		if !lastErrInfo.Retryable || contentDelivered {
			// Non-retryable error, or content was already delivered.
			if !lastErrInfo.Retryable {
				// Non-retryable: let the deferred recovery send an error to the session.
				dipper.Logger.Infof("[openai] non-retryable streaming error session=%s: code=%s type=%s message=%s",
					sessionID, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)
				dipper.Logger.Panicf("[openai] non-retryable streaming API error session=%s: code=%s type=%s message=%s",
					sessionID, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)
			}

			return
		}

		// Retryable error before any content — loop continues.
	}

	// All retries exhausted — send error to the session via deferred recovery.
	dipper.Logger.Panicf("[openai] streaming retry exhausted (max=%d) session=%s: code=%s type=%s message=%s",
		retryMaxAttempts, sessionID, lastErrInfo.Code, lastErrInfo.Type, lastErrInfo.Message)
}

// classifyErrorTypeLabel returns a human-readable label for the error type.
func classifyErrorTypeLabel(info *apiErrorInfo) string {
	if info == nil {
		return "unknown"
	}

	switch {
	case info.Code == "rate_limit_exceeded":
		return "rate_limit"
	case info.Code == "insufficient_quota":
		return "insufficient_quota"
	case strings.HasPrefix(info.Type, "server_error"):
		return "server_error"
	default:
		return "non_retryable"
	}
}

// runStream performs a single streaming request and returns whether any content
// was delivered and any API error info detected.  Returns nil error info on
// success.
//
//nolint:nestif,funlen // nesting and length are inherent to stream error handling logic
func runStream(ctx context.Context, client *openai.Client, params openai.ChatCompletionNewParams,
	reqOpts []option.RequestOption, sessionID string, reasoningExtraction string,
) (contentDelivered bool, errInfo *apiErrorInfo) {
	streamer := client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
	acc := openai.ChatCompletionAccumulator{}
	delivered := false

	for streamer.Next() {
		chunk := streamer.Current()
		acc.AddChunk(chunk)

		// Check for API error in the chunk data before proceeding.
		if errInfo := extractAPIErrorInfo(chunk.RawJSON()); errInfo != nil {
			return delivered, errInfo
		}

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
				IsChunk:    false, // Refusal is a complete message, not a chunk
			}))

			return delivered, nil
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		msg := agentpkg.Message{
			Role:       agentpkg.RoleAgent,
			Content:    choice.Delta.Content,
			IsComplete: false,
			IsChunk:    true, // Each streaming delta is a chunk
		}

		if len(choice.Delta.JSON.ExtraFields) > 0 && reasoningExtraction != "" {
			if think, ok := choice.Delta.JSON.ExtraFields[reasoningExtraction]; ok {
				var reasoning string
				dipper.Must(json.Unmarshal([]byte(think.Raw()), &reasoning))
				msg.Thoughts = reasoning
			}
		}

		if len(msg.Thoughts) > 0 || len(msg.Content) > 0 {
			delivered = true
			driver.SendMessage(agentbusMessage(sessionID, msg))
		}
	}

	// Check for stream-level errors (including API errors embedded in SSE events).
	if err := streamer.Err(); err != nil {
		var streamErr *ssestream.StreamError
		if !errors.As(err, &streamErr) {
			// Non-SSE stream error — treat as non-retryable API failure.
			dipper.Logger.Debugf("[openai] stream non-SSE error session=%s: %v", sessionID, err)
			if delivered {
				dipper.Must(err)

				return delivered, nil
			}

			return delivered, &apiErrorInfo{
				Code:      "stream_error",
				Message:   err.Error(),
				Type:      "stream_error",
				Retryable: false,
			}
		}

		rawEventData := string(streamErr.Event.Data)
		dipper.Logger.Debugf("[openai] SSE stream error event session=%s: type=%s data=%s",
			sessionID, streamErr.Event.Type, rawEventData)

		info := extractAPIErrorInfo(rawEventData)
		if info != nil {
			return delivered, info
		}

		// Non-API stream error — treat as non-retryable.
		if !delivered {
			return delivered, &apiErrorInfo{
				Code:      "stream_error",
				Message:   err.Error(),
				Type:      "stream_error",
				Retryable: false,
			}
		}

		dipper.Must(err)

		return delivered, nil
	}

	if len(acc.Choices) == 0 {
		rawJSON := acc.RawJSON()

		info := extractAPIErrorInfo(rawJSON)
		if info != nil {
			return delivered, info
		}

		// No choices and no error — log raw JSON and retry if no content was delivered.
		dipper.Logger.Warningf("[openai] streaming no choices returned session=%s raw=%s", sessionID, rawJSON)

		if !delivered {
			return delivered, &apiErrorInfo{
				Code:      "empty_response",
				Message:   "streaming response contains no choices and no error",
				Type:      "empty_response",
				Retryable: true,
			}
		}

		return delivered, nil
	}

	// Check for empty response (stop/length with no content and no tool calls).
	if isResponseEmpty(&acc.ChatCompletion) {
		if !delivered {
			dipper.Logger.Warningf("[openai] streaming empty response session=%s finish_reason=%s raw=%s",
				sessionID, acc.Choices[0].FinishReason, acc.RawJSON())

			return delivered, &apiErrorInfo{
				Code:      "empty_response",
				Message:   "model returned complete response with no content",
				Type:      "empty_response",
				Retryable: true,
			}
		}

		// Content was already delivered before final empty response.
		// Still send the complete message to avoid hanging the session.
		handleIncomingMessage(&acc.ChatCompletion, sessionID, reasoningExtraction)

		return delivered, nil
	}

	handleIncomingMessage(&acc.ChatCompletion, sessionID, reasoningExtraction)

	return delivered, nil
}

// isResponseEmpty checks whether a ChatCompletion response is effectively
// empty — the model finished with a stop/length reason but produced no
// content and no tool calls. Such responses cause the conversation to hang
// because the session receives a "complete" message with nothing actionable.
func isResponseEmpty(msg *openai.ChatCompletion) bool {
	if len(msg.Choices) == 0 {
		return true
	}
	choice := msg.Choices[0]
	hasContent := len(choice.Message.Content) > 0
	hasToolCalls := len(choice.Message.ToolCalls) > 0
	isFinished := choice.FinishReason == "stop" || choice.FinishReason == "length"

	return isFinished && !hasContent && !hasToolCalls
}

// handleIncomingMessage processes a single OpenAI message from the streaming endpoint,
// sending appropriate agent messages for content, tool calls, and reasoning texts.
func handleIncomingMessage(msg *openai.ChatCompletion, sessionID string, reasoningExtraction string) {
	choice := msg.Choices[0]
	cm := choice.Message

	agentMsg := agentpkg.Message{
		Role:       agentpkg.RoleAgent,
		IsComplete: choice.FinishReason == "stop" || choice.FinishReason == "tool_calls" || len(cm.ToolCalls) > 0,
		IsChunk:    false, // Non-streaming / final completion is not a chunk
		Content:    cm.Content,
	}

	if fields := cm.JSON.ExtraFields; fields != nil && reasoningExtraction != "" {
		if think, ok := fields[reasoningExtraction]; ok {
			var reasoning string
			dipper.Must(json.Unmarshal([]byte(think.Raw()), &reasoning))
			agentMsg.Thoughts = reasoning
		}
	}

	if choice.FinishReason == "tool_calls" && len(cm.ToolCalls) > 0 {
		agentMsg.ToolCalls = buildToolCalls(cm.ToolCalls)
	}

	agentMsg.InputTokens = int(msg.Usage.PromptTokens)
	agentMsg.OutputTokens = int(msg.Usage.CompletionTokens)

	driver.SendMessage(agentbusMessage(sessionID, agentMsg))
}

// newOpenAIClient creates an OpenAI client from the per-engine configuration.
func newOpenAIClient(cfg engineConfig) *openai.Client {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}

	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	for k, v := range cfg.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}

	c := openai.NewClient(opts...)

	return &c
}

// buildMessages converts the agent conversation history into the slice of
// OpenAI message params expected by ChatCompletionNewParams.Messages.
func buildMessages(history []agentpkg.Message, reasoningInjection string) []openai.ChatCompletionMessageParamUnion {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(history))
	var lastToolCallIDs []string

	for histIdx, msg := range history {
		switch msg.Role {
		case agentpkg.RoleSystem:
			msgs = append(msgs, openai.SystemMessage(msg.Content))

		case agentpkg.RoleUser:
			msgs = append(msgs, openai.UserMessage(msg.Content))

		case agentpkg.RoleAgent:
			var oMsg openai.ChatCompletionMessageParamUnion
			if len(msg.Content) > 0 {
				oMsg = openai.AssistantMessage(msg.Content)
			} else {
				oMsg.OfAssistant = &openai.ChatCompletionAssistantMessageParam{}
			}
			if len(msg.Thoughts) > 0 && reasoningInjection != "" {
				extras := map[string]interface{}{}
				dipper.MapSet(extras, reasoningInjection, msg.Thoughts)
				oMsg.OfAssistant.SetExtraFields(extras)
			}
			if len(msg.ToolCalls) > 0 {
				oMsg.OfAssistant.ToolCalls, lastToolCallIDs = buildOpenAIToolCalls(histIdx, &msg)
			}
			msgs = append(msgs, oMsg)

		case agentpkg.RoleTool:
			oMsg := openai.ChatCompletionMessageParamUnion{
				OfAssistant: &openai.ChatCompletionAssistantMessageParam{},
			}
			oMsg.OfAssistant.ToolCalls, lastToolCallIDs = buildOpenAIToolCalls(histIdx, &msg)
			msgs = append(msgs, oMsg)

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

// buildOpenAIToolCalls converts agentpkg ToolCalls to openai ToolCalls.
func buildOpenAIToolCalls(histIdx int, msg *agentpkg.Message) ([]openai.ChatCompletionMessageToolCallUnionParam, []string) {
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

	return toolCalls, ids
}

// buildTools converts the agent tool map into the OpenAI tool params slice.

func buildTools(tools map[string]agentpkg.Tool) []openai.ChatCompletionToolUnionParam {
	if len(tools) == 0 {
		return nil
	}

	// Sort keys for deterministic ordering (important for prompt caching).
	keys := make([]string, 0, len(tools))
	for k := range tools {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))

	for _, k := range keys {
		tool := tools[k]
		// Sort parameter names for deterministic ordering (important for prompt caching).
		paramNames := make([]string, 0, len(tool.Params))
		for pn := range tool.Params {
			paramNames = append(paramNames, pn)
		}
		sort.Strings(paramNames)

		required := make([]string, 0, len(tool.Params))
		for _, paramName := range paramNames {
			paramDef := tool.Params[paramName]
			_, ok := paramDef.(map[string]interface{})
			if !ok {
				continue
			}

			required = append(required, paramName)
		}

		parameters := openai.FunctionParameters{
			"type":       "object",
			"properties": tool.Params,
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

// buildToolCalls converts OpenAI tool-call response entries into the
// agent package ToolCall format consumed by the agent session.
func buildToolCalls(toolCalls []openai.ChatCompletionMessageToolCallUnion) []agentpkg.ToolCall {
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

	return calls
}

// parseDuration parses a Go duration string (e.g. "1s", "500ms") or returns the
// default if the string is empty or invalid.
func parseDuration(s string, defaultVal time.Duration) time.Duration {
	if s == "" {
		return defaultVal
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		dipper.Logger.Warningf("[openai] invalid duration %q, using default %v", s, defaultVal)

		return defaultVal
	}

	return d
}

// calculateBackoff computes the delay before the next retry attempt using
// exponential backoff with jitter.  If the error response includes a
// Retry-After hint, the delay will be at least that value.
//
//nolint:gosec // G404: math/rand/v2 is acceptable for jitter; cryptographic randomness is not required.
func calculateBackoff(attempt int, initialDelay, maxDelay time.Duration, errInfo *apiErrorInfo) time.Duration {
	// Compute exponential backoff: initialDelay * 2^(attempt-1)
	delay := time.Duration(float64(initialDelay) * math.Pow(2, float64(attempt-1)))
	if delay > maxDelay {
		delay = maxDelay
	}

	// Add jitter: 0-25% of the delay to avoid thundering herd.
	jitter := time.Duration(float64(delay) * (rand.Float64() * 0.25))
	delay += jitter

	// Respect Retry-After hint from the error response, if present.
	if errInfo != nil && errInfo.RetryAfter > 0 {
		if errInfo.RetryAfter > delay {
			delay = errInfo.RetryAfter
		}

		// Add a small additional jitter around the retry-after value.
		extraJitter := time.Duration(float64(errInfo.RetryAfter) * (rand.Float64() * 0.1))
		delay += extraJitter
	}

	return delay
}

// retryWithBackoff calls fn up to maxAttempts times with exponential backoff
// between retries.  fn returns (shouldRetry bool, err error).  If shouldRetry
// is true, the function will sleep using exponential backoff with jitter and
// retry.  If shouldRetry is false, retryWithBackoff stops and returns the
// error.  On success (fn returns shouldRetry=false, err=nil), returns nil.
//
// If fn returns a *retryAfterError, the Retry-After duration is used to
// override the backoff delay for that attempt.  Context cancellation is
// respected between retries.
//
//nolint:gosec // G404: math/rand/v2 is fine for jitter. Complexity is inherent to backoff logic.
func retryWithBackoff(ctx context.Context, maxAttempts int, initialDelay, maxDelay time.Duration, fn func() (bool, error)) error {
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Compute delay for this retry attempt.
			delay := time.Duration(float64(initialDelay) * math.Pow(2, float64(attempt-1)))
			if delay > maxDelay {
				delay = maxDelay
			}

			// Add jitter.
			jitter := time.Duration(float64(delay) * (rand.Float64() * 0.25))
			delay += jitter

			// Check for Retry-After hint in the error.
			var retryAfterErr *retryAfterError

			if errors.As(lastErr, &retryAfterErr) && retryAfterErr.Duration > 0 {
				if retryAfterErr.Duration > delay {
					delay = retryAfterErr.Duration
				}

				extraJitter := time.Duration(float64(retryAfterErr.Duration) * (rand.Float64() * 0.1))
				delay += extraJitter
			}

			select {
			case <-ctx.Done():
				return fmt.Errorf("retry cancelled: %w", ctx.Err())

			case <-time.After(delay):
			}
		}

		shouldRetry, err := fn()
		if !shouldRetry {
			return err
		}

		lastErr = err
	}

	if lastErr != nil {
		return fmt.Errorf("all %d retry attempts exhausted: %w", maxAttempts, lastErr)
	}

	return nil
}

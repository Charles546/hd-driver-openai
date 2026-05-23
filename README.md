# OpenAI Driver for Honeydipper

This driver enables Honeydipper's agent service to call OpenAI-compatible chat completion endpoints. It implements the `send_to_model` RPC used by the agent service to exchange messages with a language model, including tool-call round-trips.

## Features

- **Multiple engines**: configure any number of named engines, each with its own model, API key, and base URL
- **OpenAI-compatible**: works with the official OpenAI API and any compatible endpoint (Azure OpenAI, local proxies, etc.)
- **Tool calls**: full support for function/tool call round-trips as defined by the agent protocol
- **Per-request overrides**: pass extra model parameters (e.g. `temperature`, `max_tokens`) via `model_data`
- **Interruptible RPC**: honours driver shutdown and context cancellation

## Installation

1. Build the driver:
   ```bash
   cd cmd/hd-driver-openai
   go build -o hd-driver-openai
   ```
2. Place the binary in your Honeydipper installation path.

## Configuration

Register the driver in your Honeydipper daemon config and provide engine definitions under `data.engines`.

```yaml
drivers:
  openai:
    data:
      engines:
        gpt4o:
          model: gpt-4o
          api_key: ${OPENAI_API_KEY}
          # base_url is optional; defaults to https://api.openai.com/v1/
          # base_url: https://api.openai.com/v1/

        azure-gpt4o:
          model: gpt-4o
          api_key: ${AZURE_OPENAI_API_KEY}
          base_url: https://<your-resource>.openai.azure.com/openai/deployments/<deployment>/

        local:
          model: llama3
          api_key: ignored
          base_url: http://localhost:11434/v1/
```

Each engine entry supports the following fields:

| Field      | Required | Description                                           |
|------------|----------|-------------------------------------------------------|
| `model`    | yes      | Model name passed to the chat completions API         |
| `api_key`  | yes      | API key for authentication                            |
| `base_url` | no       | Override the API base URL (useful for Azure or local) |

### Wiring to the agent service

Point the agent service at this driver by setting the `driver` field in your agent definition:

```yaml
agents:
  my-agent:
    driver: openai
    engine: gpt4o
    # Optional per-call overrides forwarded as model_data:
    # model_data:
    #   temperature: 0.2
    #   max_tokens: 4096
```

## RPC contract

The driver registers one RPC handler:

### `send_to_model|interruptible`

**Input labels**

| Label              | Description                    |
|--------------------|--------------------------------|
| `agent_session_id` | Identifies the calling session |

**Input payload**

| Field        | Type                          | Description                                              |
|--------------|-------------------------------|----------------------------------------------------------|
| `engine`     | `string`                      | Key into `data.engines` to select connection settings    |
| `history`    | `[]Message`                   | Conversation history in agent message format             |
| `tools`      | `map[string]Tool`             | Available tools the model may call                       |
| `model_data` | `map[string]interface{}`      | Extra fields merged into the request via `WithJSONSet`   |

**Output** — sent to `agentbus:receive` with the same `agent_session_id` label:

```yaml
message:
  Role: agent        # or "tool" when the model requests tool calls
  Content: "..."     # present for text responses
  IsComplete: true   # true for final (non-streaming) text responses
  ToolCalls:         # present for tool-call responses
    - FuncName: search_web
      Params:
        query: golang testing
```

## License

This project is dual-licensed. By default it is licensed under the GNU Affero General Public License v3.0. If you have a separate written commercial agreement, you may use it under those terms instead.

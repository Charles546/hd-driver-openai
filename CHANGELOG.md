## [1.7.1](https://github.com/Charles546/hd-driver-openai/compare/v1.7.0...v1.7.1) (2026-08-27)


### Bug Fixes

* deliver tool calls regardless of finish_reason; escalate malformed tool call args ([#22](https://github.com/Charles546/hd-driver-openai/issues/22)) ([7bc1c03](https://github.com/Charles546/hd-driver-openai/commit/7bc1c0347b50bfe593928dbd54161d7dfad55981))

# [1.7.0](https://github.com/Charles546/hd-driver-openai/compare/v1.6.3...v1.7.0) (2026-08-21)


### Features

* add debug detection for empty-complete agent messages ([#21](https://github.com/Charles546/hd-driver-openai/issues/21)) ([ccdc74c](https://github.com/Charles546/hd-driver-openai/commit/ccdc74c5d7fefc7bfebb6b0fed0351a295529f83))

## [1.6.3](https://github.com/Charles546/hd-driver-openai/compare/v1.6.2...v1.6.3) (2026-08-11)


### Bug Fixes

* treat whitespace-only responses as empty for retry ([#20](https://github.com/Charles546/hd-driver-openai/issues/20)) ([64210ef](https://github.com/Charles546/hd-driver-openai/commit/64210ef256bb325c9bbed5a9fe15feb2b53e6cf2))

## [1.6.2](https://github.com/Charles546/hd-driver-openai/compare/v1.6.1...v1.6.2) (2026-08-11)


### Bug Fixes

* correct detecting empty response for retrying ([#19](https://github.com/Charles546/hd-driver-openai/issues/19)) ([424f9b6](https://github.com/Charles546/hd-driver-openai/commit/424f9b6bd3cf5538734bd5a5f672e89c6522e71f))

## [1.6.1](https://github.com/Charles546/hd-driver-openai/compare/v1.6.0...v1.6.1) (2026-07-23)


### Bug Fixes

* skip tool results with empty tool_call_id in buildMessages ([#18](https://github.com/Charles546/hd-driver-openai/issues/18)) ([ec20311](https://github.com/Charles546/hd-driver-openai/commit/ec203111c82dbdc45a6ef0a39f3085dcf0ebd45f))

# [1.6.0](https://github.com/Charles546/hd-driver-openai/compare/v1.5.0...v1.6.0) (2026-07-15)


### Features

* Add IsChunk field support for streaming protocol ([#17](https://github.com/Charles546/hd-driver-openai/issues/17)) ([6a07817](https://github.com/Charles546/hd-driver-openai/commit/6a078173b5b5a4109930761edfa0509a98ba1caf))

# [1.5.0](https://github.com/Charles546/hd-driver-openai/compare/v1.4.1...v1.5.0) (2026-07-11)


### Features

* add empty response detection and retry mechanism ([#15](https://github.com/Charles546/hd-driver-openai/issues/15)) ([c81d208](https://github.com/Charles546/hd-driver-openai/commit/c81d2081f7cd76c343f3e6bb10c8512d9fe4c012))

## [1.4.1](https://github.com/Charles546/hd-driver-openai/compare/v1.4.0...v1.4.1) (2026-07-04)


### Bug Fixes

* ensure deterministic tool and parameter ordering in buildTools for prompt caching ([#14](https://github.com/Charles546/hd-driver-openai/issues/14)) ([2d63c42](https://github.com/Charles546/hd-driver-openai/commit/2d63c42d413e7b56e2a0f4c9fa82b511151e171c))

# [1.4.0](https://github.com/Charles546/hd-driver-openai/compare/v1.3.2...v1.4.0) (2026-07-04)


### Features

* add support for custom HTTP headers per engine instance ([#13](https://github.com/Charles546/hd-driver-openai/issues/13)) ([a969a65](https://github.com/Charles546/hd-driver-openai/commit/a969a65229d8f516ead9cc77f22b7fc04b7bfeef))

## [1.3.2](https://github.com/Charles546/hd-driver-openai/compare/v1.3.1...v1.3.2) (2026-06-18)


### Bug Fixes

* send error message to agent session on retry exhaustion ([#12](https://github.com/Charles546/hd-driver-openai/issues/12)) ([88ad814](https://github.com/Charles546/hd-driver-openai/commit/88ad814c60719f735b83e41250b4670c35549ea7))

## [1.3.1](https://github.com/Charles546/hd-driver-openai/compare/v1.3.0...v1.3.1) (2026-06-14)


### Bug Fixes

* detect more retryable errors ([#11](https://github.com/Charles546/hd-driver-openai/issues/11)) ([667aa31](https://github.com/Charles546/hd-driver-openai/commit/667aa31d5db25aa8a062a547284a8cb2e63a816c))

# [1.3.0](https://github.com/Charles546/hd-driver-openai/compare/v1.2.0...v1.3.0) (2026-06-11)


### Features

* enhanced error handling with SDK error differentiation and empty body retry ([#10](https://github.com/Charles546/hd-driver-openai/issues/10)) ([b15f0e4](https://github.com/Charles546/hd-driver-openai/commit/b15f0e4b916cfcbf9d8bc2d35b0652471a48e8dc))

# [1.2.0](https://github.com/Charles546/hd-driver-openai/compare/v1.1.3...v1.2.0) (2026-06-11)


### Features

* add configurable retry with exponential backoff for API errors ([#9](https://github.com/Charles546/hd-driver-openai/issues/9)) ([cb66c0e](https://github.com/Charles546/hd-driver-openai/commit/cb66c0e8b3f88f1aee4b12c415e735f6708cd9ae))

## [1.1.3](https://github.com/Charles546/hd-driver-openai/compare/v1.1.2...v1.1.3) (2026-06-09)


### Bug Fixes

* check finish reason to confirm turn complete ([#7](https://github.com/Charles546/hd-driver-openai/issues/7)) ([1f8e20d](https://github.com/Charles546/hd-driver-openai/commit/1f8e20d7d393e8f6c4a23d5dec386c4f8f01aa71))

## [1.1.2](https://github.com/Charles546/hd-driver-openai/compare/v1.1.1...v1.1.2) (2026-06-08)


### Bug Fixes

* assistant message content null ([#6](https://github.com/Charles546/hd-driver-openai/issues/6)) ([4bf0dc9](https://github.com/Charles546/hd-driver-openai/commit/4bf0dc92921efa3bf5dff566df0e3cc6bd56a0b1))

## [1.1.1](https://github.com/Charles546/hd-driver-openai/compare/v1.1.0...v1.1.1) (2026-06-07)


### Bug Fixes

* handling reasoning messages with agent_settings ([#5](https://github.com/Charles546/hd-driver-openai/issues/5)) ([99423f6](https://github.com/Charles546/hd-driver-openai/commit/99423f6fd475e06fe5448d13e737673884bbfe36))

# [1.1.0](https://github.com/Charles546/hd-driver-openai/compare/v1.0.1...v1.1.0) (2026-06-05)


### Features

* setting reasoning effort temperature using model_data ([#4](https://github.com/Charles546/hd-driver-openai/issues/4)) ([2397961](https://github.com/Charles546/hd-driver-openai/commit/2397961b526115597608ec4489bc1431b4b3d63a))

## [1.0.1](https://github.com/Charles546/hd-driver-openai/compare/v1.0.0...v1.0.1) (2026-06-04)


### Bug Fixes

* avoid losing tool parameter defs ([#3](https://github.com/Charles546/hd-driver-openai/issues/3)) ([39c4d78](https://github.com/Charles546/hd-driver-openai/commit/39c4d78a23ccebce0eae5c23c20d782441695de9))

# 1.0.0 (2026-05-28)


### Bug Fixes

* remove unused chunksq ([#2](https://github.com/Charles546/hd-driver-openai/issues/2)) ([8b89789](https://github.com/Charles546/hd-driver-openai/commit/8b89789f438adfe36ff3dbdb1e89f289bc94a5d6))


### Features

* **openai:** populate token counts in agentbus messages ([c0617aa](https://github.com/Charles546/hd-driver-openai/commit/c0617aa0b86fa8754d8c52e2cbe74286441c46db))

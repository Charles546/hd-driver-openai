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

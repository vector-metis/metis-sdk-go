# Changelog

## 0.3.1

- 增加 `ListRuntimeModels`，读取当前应用已绑定模型 slot 的展示元数据和非敏感能力字段。

## 0.3.0

- 增加平台用户、应用外部用户和其它用户三类模型用量归因构造器。
- 增加 `ModelConfig.NewRequest`，统一携带模型插槽 API Key 和互斥归因头。

## 0.2.0

- 强类型重构 `ModelConfig`，彻底移除弱类型 `Values` 字典。
- 增加模型特性与卡片参数属性：`SupportsVision`, `SupportsThinking`, `SupportsTools`, `ContextWindow`, `MaxInputTokens`, `MaxOutputTokens`, `Dimensions`, `Normalized`。
- 增加批量模型枚举 `Models(modelType)` 与安全探活 `TryModel(slot)` API。

## 0.1.1

- 暴露依赖版本、制品 SHA、直接依赖标记、可用性和解析诊断字段。

## 0.1.0

- 首次公开发布。
## Unreleased

- Expose dependency version, package SHA, directness, availability and resolution diagnostics.

# 发布说明 — v0.1.16

## 推理配置完全归用户管理

hellogrok 不再为任何渠道创建、迁移、重排或替换 `reasoning_effort`、`reasoning_efforts` 或 `supports_reasoning_effort`。用户可以配置当前档位、推理菜单、同时配置两者或全部不配置；缺失字段继续使用 Grok Build 模型目录和供应商默认行为。

恢复逻辑仍兼容早期版本记录的临时推理投影，因此停止代理或执行恢复时仍能清理这些旧受管值，同时不改动当前由用户管理的设置。

## 供应商推理档位保持原值

Responses、Chat Completions 和 Messages 不再通过 DeepSeek 专用映射表归一化供应商推理档位。未知或未来的非空档位会原样传给供应商，由供应商自行校验。

Responses 转 Messages 时遵循 Grok Build 原生序列化行为：`none` 和 `minimal` 不生成 Messages 推理字段，其他非空值保持不变。

## DeepSeek 关闭选择保持原有语义

对于 DeepSeek 官方端点，显式选择 `none` 时，Chat Completions 和 Messages 都会收到原生思考关闭开关。即使模型配置中只有 `reasoning_effort = "none"`，无需同时配置推理菜单，也能正确关闭思考。

未配置任何推理字段时，hellogrok 不添加思考开关，保持 Grok Build 和 DeepSeek 的默认行为。

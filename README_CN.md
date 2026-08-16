<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok 图标" width="128">
</p>

# hellogrok

跨平台 Grok Build 本地代理，让自定义模型渠道兼容常见 API 格式、Build 原生 Web 工具、独立鉴权和自动配置恢复。

[![Version](https://img.shields.io/badge/version-0.1.13-2f6feb.svg)](./internal/appinfo/appinfo.go)
[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8.svg)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Platforms](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg)](#平台支持)
[![LINUX DO](https://img.shields.io/badge/LINUX_DO-链接认可-0A84FF?logo=linux&logoColor=white)](https://linux.do)

[English](./README.md) · [简体中文](./README_CN.md) · [发布说明](./RELEASE_NOTES_CN.md) · [更新日志](./CHANGELOG.md)

> 🏅 此项目已链接认可 [LINUX DO](https://linux.do) 社区。

## 目录

- [为什么需要 hellogrok](#为什么需要-hellogrok)
- [功能](#功能)
- [搜索与配置](#搜索与配置)
- [DeepSeek](#deepseek)
- [下载](#下载)
- [快速开始](#快速开始)
- [平台支持](#平台支持)
- [托盘与 CLI](#托盘与-cli)
- [开机启动](#开机启动)
- [工作原理](#工作原理)
- [故障排查](#故障排查)
- [开发与测试](#开发与测试)
- [使用限制](#使用限制)
- [参与贡献](#参与贡献)
- [许可证](#许可证)

## 为什么需要 hellogrok

Grok Build 可以接入自定义模型端点，但不同服务商实际提供的协议、响应格式、鉴权方式和 Web 搜索能力并不一致。一个能够通过 `curl` 返回文本的渠道，在正常 Grok Build 对话中仍可能失败、无法使用原生 Web 工具，或者收到错误的登录凭据。

hellogrok 为这些自定义渠道提供统一的本地兼容层。运行时准备 Grok 必需配置，让每个渠道固定使用自己的端点和凭据，支持 Grok Build 原生 Web 工作流，并在停止时恢复原始配置。

它适合需要维护多个第三方模型渠道，并希望直接在 Grok Build 中切换模型，而不想每次手动修改 URL 或工具设置的用户。

## 功能

[发布说明](./RELEASE_NOTES_CN.md) | [完整更新日志](./CHANGELOG.md)

### 渠道兼容

- 支持采用 `responses`、`messages` 或 `chat_completions` 的上游渠道。
- 在供应商边界保留渠道配置的真实协议、URL 路径、模型、凭据、推理和工具语义。
- 当 `supports_backend_search = true` 时，临时让 Grok Build 将该渠道作为 Responses 消费，再在代理内把请求、响应和 SSE 事件转换到供应商的真实协议。三种上游格式因此都能向 Grok Build 返回 `web_search_call`；上游真实返回结果 URL 时，还会返回来源和站点数量。
- 当 `supports_backend_search = false` 时，Grok Build 保持使用配置的原生消费者，其客户端 `web_search` 依次使用 `[models].web_search`、`GROK_WEB_SEARCH_MODEL` 或已登录官方账号的回退路径。字段缺省时保留 Grok Build 模型目录行为，但精确指向 DeepSeek 官方端点的模型默认启用其文档所述 hosted 搜索。显式 false 仍会关闭它，除非该自定义渠道被选为默认搜索模型。
- 提供渠道隔离的 `/responses`、`/messages` 和 `/chat/completions` 路由，并在代理停止时逐字节恢复原始 `api_backend`。
- 转发前校验各协议的工具历史：Responses 调用必须有匹配的 `function_call_output`，Messages 的 `tool_use` 必须由紧邻的下一条 user 消息中的 `tool_result` 完整配对，Chat 工具调用必须有匹配的 tool 消息。确定性错误返回不可重试的 `400`，不会进入 Grok Build 重试循环。
- 在私有 `keepalive`、`keep-alive`、`keep_alive`、`heartbeat`、`ping` 帧到达 Grok Build 前将其转换为标准 SSE 注释，不占用 Responses 事件序号；收到各协议的终止事件后立即关闭上游流。
- 普通渠道的上游响应头等待及所有响应正文的两次读取间隔最多为 601 秒，比 Grok Build shell 默认的 600 秒多一秒；`api.deepseek.com` 官方路由使用 660 秒，以覆盖服务商记录的最长十分钟排队。请求没有总时限，非流式排队空行或规范化心跳等任意上游字节都会刷新空闲时限。
- 在规范化前记录原始上游响应声明的模型，支持终止帧优先、大小写不敏感的不一致判断和多帧冲突标记，不改变路由或响应数据。
- Messages 的 `thinking` 起始块缺少空 `signature` 时补齐该字段，同时保留供应商随后发送的真实 `signature_delta`，使 Messages 兼容中转可被 Grok Build 的严格原生解码器消费。
- 保留每个渠道配置的上游 URL 路径和模型标识。
- 使用前准备所有显式自定义渠道，避免通过 `/model` 切换后首次请求失败。
- 热切换模型时保留可移植的会话历史，只排除已知属于不同渠道、协议、线上模型或上游端点的加密推理。
- 在 Responses、Messages 与 Chat Completions 桥接中保留 Grok Build 的任意本地函数工具，包括 Shell、文件、补丁、Task 和 MCP 函数。第三方渠道不会收到 xAI 专属的 `x_search`；供应商 hosted 工具仍须由上游真实支持。

### 原生 Web 工具

- 支持 hosted 和客户端搜索两种 Grok Build 原生 `web_search` 工作流。
- 当 `supports_backend_search = true` 时，三种格式都使用当前渠道自身的 hosted 搜索：Responses 保持 Responses，Messages 保持 Messages 并使用 `web_search_20250305`，Chat 使用配置的搜索方言或协议桥接。DeepSeek Responses 正式发布后，官方渠道遵循已配置的原生协议；由于 DeepSeek Chat 目前只记录 function 工具，其 hosted 搜索默认桥接到 Responses。确实需要 Messages 桥接时仍可显式设置 `chat_search_dialect = "messages"`。
- 对设置了 `supports_backend_search = true` 的渠道，只要上游真实执行了搜索，Grok Build 就会收到规范的 Responses 搜索事件，包括已完成的 `web_search_call`，以及供应商实际返回的已验证来源、引用和用量。该字段是路由声明，不是能力探测结果。每个 Responses 搜索都会请求完整的 `web_search_call.action.sources` 元数据，同时保留调用方已有项目。DeepSeek 当前会接受但静默忽略这个标准 `include` 提示，因此 hellogrok 只能消费原生搜索 action 已带的 URL，无法恢复上游省略的结果；未来 DeepSeek 实现该字段后，同一请求会自动取得来源，无需更新 hellogrok。Messages 搜索结果/引用块、供应商顶层引用及 Chat 的注解/搜索结果容器会归一化到同一套规范来源字段。
- 通过 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL` 选中的自定义代理渠道，会在整个代理运行期投影为 `supports_backend_search = true`，即使原值是 false。Grok Build 因而能在普通会话和固定的非流式 WebSearchClient 请求中发现 hosted `web_search`；hellogrok 将两者转换为供应商协议，并在停止时恢复原始设置。
- 将适配后搜索结果中的真实 URL 同时写入 `web_search_call.action.sources` 与 `output_text.annotations`；只有响应能独立证明已执行搜索时，才会使用最终回答中的有效链接。Grok Build 因此可显示原生去重站点数。
- 只有上游能独立证明搜索已完成并同时返回非空回答文本时，适配后的客户端搜索才会成功。静默忽略搜索扩展的供应商会收到不可重试的 `502`；hellogrok 无法为此类渠道凭空生成搜索能力。
- 当前代理工具权限允许时，将 `web_fetch` 作为独立工具保留。
- 让受支持的子代理使用相同的搜索行为。
- Grok 官方模型继续使用 Grok Build 原生搜索和登录路径。

### 鉴权与配置安全

- 支持渠道自己的 API key、环境变量密钥、鉴权提供器和请求头。
- 避免把 Grok 官方登录令牌发送给无关的自定义渠道。
- 加载配置时校验渠道请求头名称和值；请求分帧、内容和连接请求头仍由代理控制。
- 代理启动时检查并临时补全 Grok 必需设置。
- 接受带或不带 BOM 的 UTF-8 `config.toml`。普通代理事务会保留文件当前的 BOM 选择，包括用户在代理运行期间主动改变的选择；TOML 无效时会显示文件路径、行号和列号，不再只输出缺少上下文的解析器错误。
- 根据每个自定义模型的有效上下文窗口和最大输出分别计算自动压缩预算；只临时降低不安全的阈值，不会提高用户设置的较低值，停止代理时恢复全部受管值。
- 正常停止、退出托盘、Ctrl+C、SIGTERM 或启动失败时恢复未被用户改动的临时值，并通过字段级三方合并保留代理运行期间的用户修改。无关修改使整份 TOML 无效时，逐行恢复仍会撤销可独立解析的受管字段，不改写用户的无效字节。
- 托盘“退出”始终会在尝试清理后结束进程。若文件无法访问或仍有不属于原事务结构的本地路由，恢复事务会留在磁盘供下次启动处理，不会把用户困在托盘程序中。
- 异常退出后可以使用 `hellogrok restore` 恢复代理管理的设置。

### 桌面与运维

- 提供 Windows 原生托盘程序和控制台 CLI。
- 记住用户选择的代理启用状态，并在下次打开托盘时恢复。
- 首次启动默认启用代理，让新用户立即可用。
- 支持 Windows、Linux 和 macOS 登录自启动。
- 提供路由检查、分类状态、实时日志搜索、按使用日保留日志和终端日志跟踪。
- 支持 Windows、Linux、macOS 的 amd64 和 arm64 构建。

hellogrok 是 Grok Build 渠道代理，不是系统代理、PAC 服务、VPN 或通用 HTTPS 拦截工具。

## 搜索与配置

### 搜索模式

搜索行为先看显式搜索模型选择，再看当前后端：

| 设置 | 搜索行为 |
|------|----------|
| 设置了 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL` | 所选模型若是自定义代理渠道，hellogrok 会在运行期把它临时投影为 `supports_backend_search = true` 和面向 Grok Build 的 Responses，普通会话与 WebSearchClient 均生效；上游仍可采用 `responses`、`messages` 或 `chat_completions`。环境变量优先，选择只在代理运行期覆盖原来的 false，不会发起启动请求。 |
| 任意渠道设置 `supports_backend_search = true` | Grok Build 使用 Responses hosted 工具，hellogrok 则调用该渠道自身实际可用的搜索 API：Responses、Messages `web_search_20250305`，或选定的 Chat 搜索方言/桥接。 |
| 任意渠道设置 `supports_backend_search = false` | Grok Build 使用客户端 `web_search`：优先 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL`，否则使用已登录官方账号的回退路径。被选中的自定义搜索渠道会临时覆盖为 true；精确指向 DeepSeek 官方端点的模型在字段缺省时也默认使用自身 hosted 搜索。 |
| 没有可用的 hosted 或客户端搜索路径 | 当前模型无法使用 `web_search`。 |
| `web_fetch` | 独立于搜索模型选择，并受当前工具权限限制。 |

后端搜索不是 Grok Build 发起的第二个请求。它的声明就在当前模型请求中，因此 hellogrok 必须在模型决定是否搜索之前选定供应商协议。客户端 `web_search` 不同：当前模型先调用本地函数，随后 Grok Build 的 `WebSearchClient` 才向所选搜索模型单独发送非流式 `/responses` 请求，再由 hellogrok 映射到该渠道的搜索协议。

hellogrok 不会创建、选择或替换 `[models].web_search`，启动时也不会发送搜索能力探测。标记为启用的 Messages 渠道必须真正支持 `web_search_20250305`。Chat 默认使用 `web_search_options`；xAI 官方和 DeepSeek 官方 Chat 自动桥接到 Responses。渠道需要指定策略时，可把 `chat_search_dialect` 设置为 `web_search_options`、`search_parameters`、`messages` 或 `responses`。临时能力标记只是路由声明，不证明供应商真实支持：上游 HTTP 拒绝会原样保留；固定 WebSearchClient 请求若被静默回答但没有完成搜索，hellogrok 会返回不可重试的 `502`，明确指出所选搜索模型未完成 backend `web_search`。

对于客户端搜索，hellogrok 会明确工具用途，但不会根据提示词推断或强制调用工具。强制选择只来自结构化 `tool_choice`。内部传输别名只修改协议定义的工具声明、选择和调用名称字段；响应正文、URL、工具参数、工具结果及其他业务 JSON 均保持不变。

### 配置示例

下面示例把一个支持 `web_search_options` 的 Chat Completions 渠道设为 Grok Build 默认搜索模型：

```toml
[models]
web_search = "search-relay"

[model.search-relay]
model = "provider-search-model"
base_url = "https://api.example.com/v1"
env_key = ["SEARCH_RELAY_API_KEY"]
api_backend = "chat_completions"
chat_search_dialect = "web_search_options"
supports_backend_search = false
```

示例中的原始 false 仍由用户所有，但 hellogrok 运行期间会因默认搜索模型选择临时覆盖为 true，停止时恢复；供应商仍必须真实支持指定搜索方言。没有被选为默认搜索模型的渠道，只有在确认供应商 API 支持 hosted 搜索后才应设置 `supports_backend_search = true`。Messages 使用 `web_search_20250305`；Chat 中转可显式选择 `chat_search_dialect`。

### 支持的渠道设置

| 设置 | 是否必需 | 默认值 | 用途 |
|------|----------|--------|------|
| `model` | 否 | 模型表 ID | 发送给上游渠道的模型标识。 |
| `base_url` 或 `api_base_url` | 是 | 无 | 自定义上游端点；没有自定义 URL 的模型不会进入代理。 |
| `api_backend` | 否 | 模型目录，其次 `chat_completions` | 上游真实 API 格式：`responses`、`messages` 或 `chat_completions`。启用搜索能力的非 Responses 渠道只会在 Grok Build 侧临时投影为 Responses；hellogrok 在供应商边界转换，并保持选定的原生协议，除非显式搜索桥接生效。 |
| `chat_search_dialect` | 否 | 按主机判断 | hosted 搜索策略覆盖：`web_search_options`、`search_parameters`、`messages` 或 `responses`。DeepSeek 与 xAI 官方 Chat 默认 `responses`，其他 Chat 主机默认 `web_search_options`；Responses 与 Messages 只有在该字段显式要求时才桥接。 |
| `api_key` | 三选一 | 无 | 静态渠道凭据；共享配置建议优先使用 `env_key`。 |
| `env_key` | 三选一 | 无 | 保存渠道凭据的环境变量名或按顺序尝试的名称列表。 |
| `auth_provider` | 三选一 | 无 | Grok 命令式鉴权提供器。 |
| `auth_scheme` | 否 | `bearer` | 上游鉴权方式；只有服务商明确要求 `X-Api-Key` 时才设置为 `x_api_key`。 |
| `extra_headers` | 否 | 空 | 额外的渠道自有 HTTP 请求头，包括供应商专用鉴权字段。拒绝由代理控制的分帧、内容和连接请求头；名称按大小写不敏感处理。 |
| `env_http_headers` | 否 | 空 | 从环境变量读取的 HTTP 请求头；解析后的值使用与 `extra_headers` 相同的请求头规则。 |
| `supports_backend_search` | 否 | 模型目录/供应商默认 | 为 true 时，三种上游格式都使用当前渠道自身的 hosted 搜索，并向 Grok Build 输出规范 Responses 搜索事件；为 false 时，Grok Build 使用配置或登录回退的客户端搜索路径，但被 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL` 选中的自定义模型会临时覆盖为 true。精确的 DeepSeek 官方端点不依赖模型 ID 而默认为 true；其他未知模型继续由显式配置或 Grok Build 模型目录决定。 |
| `context_window` | 否 | 供应商/模型目录 | 输入与输出共享的总上下文容量。显式模型/provider 值优先；缺失时，hellogrok 可以学习可信上游值并临时物化到模型级，使 Grok Build 使用相同的自动压缩分母。 |
| `max_completion_tokens` | 否 | 模型元数据 | 最大生成 token 额度。显式配置优先；缺失时，hellogrok 根据实际发出的请求和可信上游元数据计算预算，但不会把请求中观察到的值写回为模型输出上限。 |
| `auto_compact_threshold_percent` | 否 | 模型值，其次 `[session]`，最后 `85` | Grok Build 相对于 `context_window` 的首选压缩触发百分比。安全时保持原值；需要为最大输出和安全余量预留空间时，按模型临时降低。有效范围为 `0` 到 `100`。 |
| `inference_idle_timeout_secs` | 否 | Grok Build/供应商策略 | 等待上游响应头或正文数据时允许的最长空闲间隔。模型值优先于全局 `[models]` 值；DeepSeek 官方端点未显式设置时使用 660 秒。 |

模型设置可以直接写在 `[model.<id>]` 下，也可以从引用的 `[model_providers.<id>]` 继承；模型级设置优先。若模型和 provider 都没有设置 `api_backend`，hellogrok 会让该字段在 Grok Build 的活动配置中继续缺省，并跟随 Grok Build 当前模型目录解析后实际发来的协议；模型目录也不认识该模型时，才使用 Grok Build 的 `chat_completions` 回退。解析后的 Responses 请求若实际携带 hosted-search 工具，代理会把请求事实作为能力信号，因此未来目录新增模型不需要 hellogrok 增加模型 ID。

渠道 ID 含点号时，推荐按 TOML 语法引用完整 ID，例如 `[model."provider.v1-beta"]`。连字符不需要引用。`name = "Provider.v1-beta"` 只是显示名称，点号和连字符均可直接使用。hellogrok 也会在代理运行期间兼容旧的未引用点号表头，并在停止时恢复原文。

不要手动把自定义渠道 URL 设置成 hellogrok 的本地地址。本地临时 URL 只应由应用在代理运行期间管理。

### 自动压缩容量

Grok Build 根据完整上下文窗口计算触发点：`context_window * auto_compact_threshold_percent / 100`，不会先减去最大输出额度。因此，hellogrok 会在 Grok 下一个采样轮次前按模型计算安全百分比：

```text
margin    = max(8192, ceil(context_window / 20))
safe      = floor(100 * (context_window - max_completion_tokens - margin) / context_window)
effective = min(user_or_default_threshold, safe)
```

首选阈值依次来自 `[model.<id>]`、`[session]` 和 Grok Build 默认值 `85`。5% 安全余量且最低为 8K，用于覆盖 token 估算和压缩摘要开销。两项容量已知且用户阈值更低时，hellogrok 保持原值；最大输出与安全余量已经占满窗口时，不存在安全的正百分比，hellogrok 会报告容量冲突并保持现有阈值，不会注入 `0`。

显式模型/provider 容量始终优先。字段缺失时，hellogrok 从实际发出的 `max_output_tokens` 或 `max_tokens`、有效上游 `X-Grok-Context-Window` 与 `X-Grok-Max-Completion-Tokens` 响应头，或唯一且无歧义的结构化上下文上限中学习。可信窗口会临时写成模型级 `context_window`，因为 Grok Build 必须使用相同分母，安全百分比才成立；请求中观察到的输出额度只参与计算，不会写成 `max_completion_tokens`。

一个或两个容量仍未知时，hellogrok 会把模型标记为“容量学习中”，不会根据模型名称猜测阈值。因此首个请求仍可能使用 Grok Build 的模型目录或回退值；现有结构化上下文错误路径可以降低该请求的输出额度并重试一次。学习记录只保存哈希路由标识，不含 URL、模型名或凭据，30 天后过期，并以 `capacity_cache.json` 存放在 hellogrok 私有数据目录中。

阈值和学习窗口都使用与本地路由相同的恢复事务。模型目录重载并重选当前模型后，已空闲的打开会话会采用新值；活动会话则通过有界延迟重试，空闲后再刷新。停止代理会先取消该工作器，再恢复原始配置。`GROK_AUTO_COMPACT_THRESHOLD_PERCENT` 在 Grok Build 内仍高于 TOML；hellogrok 继承到该环境变量时可以提示不安全值，但无法替换已经运行的 Grok 进程环境。

## DeepSeek

DeepSeek 兼容行为只根据精确的官方主机 `api.deepseek.com` 启用，不绑定模型 ID。滚动别名和未来模型因此无需发布新版 hellogrok 就能获得鉴权、端点、hosted 搜索、协议级请求规范化、排队保活、用量与 SSE 分帧处理。同协议原生请求会保留未知字段；跨协议桥接只映射 Grok Build 当前实际发送且目标协议能够表达的字段。未来未知推理档位值仍会原样保留。截至 2026-08-16，当前正式模型 ID 为正式版 `deepseek-v4-pro` 和 `deepseek-v4-flash`；它们只是配置示例，不是 hellogrok 的白名单。仅复用 DeepSeek 模型名的中转不会获得官方端点假设。

### 推荐配置

Responses 与 Grok Build 的原生协议最接近，是推荐后端：

```toml
[model.deepseek-v4-pro]
model = "deepseek-v4-pro"
base_url = "https://api.deepseek.com"
env_key = ["DEEPSEEK_API_KEY"]
api_backend = "responses"
context_window = 1048576
max_completion_tokens = 384000
inference_idle_timeout_secs = 660

[model.deepseek-v4-flash]
model = "deepseek-v4-flash"
base_url = "https://api.deepseek.com"
env_key = ["DEEPSEEK_API_KEY"]
api_backend = "responses"
context_window = 1048576
max_completion_tokens = 384000
inference_idle_timeout_secs = 660
```

缺省 `supports_backend_search` 即使用 DeepSeek 原生 hosted Web Search；显式设为 `false` 则改用 Grok Build 客户端搜索。Responses 与 Messages 搜索请求保持各自配置的原生 API。由于 DeepSeek Chat 目前只记录 function 工具，Chat 默认桥接到 Responses；只有明确需要 Messages 桥接时才设置 `chat_search_dialect = "messages"`。hellogrok 会向 Responses 请求 `web_search_call.action.sources`，保留 DeepSeek 实际返回的 URL，但不会虚构被省略的来源。原生 Responses 与 Chat 使用 Bearer 鉴权；Messages 使用官方文档中的 SDK base URL `https://api.deepseek.com/anthropic`（实际端点为 `/anthropic/v1/messages`）和 `X-Api-Key`。`[1m]` 后缀只是 DeepSeek 文档中的 Anthropic 集成别名，不能用于 Responses。

上面两个容量上限对应 DeepSeek 文档中的 1M 总上下文和 384K 最大输出；`1,048,576` 来自其结构化上下文错误中实际报告的服务边界，不是 hellogrok 按模型 ID 写死的常量。使用这两个值时，85% 首选阈值会被自动预算临时限制为 58%。660 秒是覆盖 DeepSeek 官方最长十分钟排队的空闲策略。模型级配置优先于继承的 provider 配置；未显式配置时，hellogrok 接受有效的上游 `X-Grok-Context-Window`、`X-Grok-Max-Completion-Tokens`，或错误中无歧义的结构化上下文上限字段，不根据模型名称猜测。最后才使用 Grok Build 自己的模型目录；未知自定义模型缺少 `context_window` 时先使用 200,000 token 回退，直到 hellogrok 学到可信窗口。DeepSeek 没有公开这两个 Grok 私有响应头，因此建议显式填写两项上限，确保首轮行为可预测。未来模型或滚动别名改变容量时无需发布新版 hellogrok。

当前 DeepSeek 推理菜单默认选择 `High`。Grok Build 原生接受紧凑的 `reasoning_efforts = ["none", "low", "high", "max"]` 写法，并会从非空菜单推导推理支持，因此 hellogrok 不再添加冗余的 `supports_reasoning_effort`，也不再写四段对象表。hellogrok 把所有用户显式赋值视为用户所有，包括不含 `none` 的列表、空列表以及自定义标签、顺序或默认值的对象菜单，既不替换也不追加。当前 Grok Build 在合并模型目录默认值时会忽略空集合覆盖，因此 `reasoning_efforts = []` 能保证 hellogrok 不注入菜单，但不一定能清除 Grok Build 自己继承的菜单。旧版 hellogrok 精确生成的冗长菜单只会迁移一次；已有模型级或全局推理选择时保持原值，否则写入 `reasoning_effort = "high"` 维持原默认值。三种协议仍统一采用官方映射：`minimal`/`low` 映射为 `low`，`medium`/`high`/`xhigh` 映射为 `high`，`max` 保持不变，`none` 关闭思考（`minimal` 仅是 Responses 的输入值）。

### 原生能力映射

| DeepSeek 接口 | 在 Grok Build 中的适配行为 |
|---------------|----------------------------|
| Responses | 保留 `instructions`、developer 消息、推理、原生 `text.format` JSON Schema、函数工具和原生 Web Search；发送标准来源 `include` 提示但不虚构缺失结果，来源展示仅限 DeepSeek 实际返回的 URL。供应商 `action.queries` 数组保持原样，同时为每次调用补充 Grok Build 用于显示的单值 `action.query`；`response.completed`、`response.incomplete`、`response.failed` 都作为终止事件。 |
| Chat Completions | 保留工具调用历史中的 `reasoning_content`，请求流式终止用量块，在未显式设置 `max_tokens` 时把 `max_completion_tokens` 映射为 `max_tokens`，把 developer 消息转换为 system，为 assistant 工具消息补非 null 内容，并把显式 Responses `user` 映射为 DeepSeek `user_id`。DeepSeek 在该接口只记录了 function 工具，因此 hosted Web Search 必须桥接到 Messages 或 Responses。DeepSeek 在开启思考时不接受 `tool_choice`，因此会移除该选择器但保留函数声明供模型自动使用；显式关闭思考时仍保留官方支持的选择器形式。Grok Build 的 `json_schema` 会转换为官方支持的 `json_object` 加 schema 指令，返回 JSON 仍由 Grok Build 本地校验。 |
| Anthropic Messages | 使用 `X-Api-Key`，保留思考/工具历史、推理强度、函数工具及原生服务端 Web Search 块。Grok Build 省略字段表示的 `None` 会转换为 `thinking.type=disabled`，不会误落到 DeepSeek 默认的 `high`；只发送受支持的 `output_config.effort`，显式 Responses `user` 映射为 `metadata.user_id`，官方 `deepseek-v4-pro[1m]` 别名会继续保留在 Messages 请求中。 |
| 排队与用量 | 接受非流式空行保活和流式 `: keep-alive` 注释；保留真实终止用量，让 Grok Build 正确统计上下文并触发自动压缩。 |

在这三套接口中，只有 Responses 原生支持 JSON Schema 输出。Chat 只支持 `json_object`，且官方说明它偶尔可能返回空 content，因此适配依赖注入的 schema 指令和 Grok Build 本地校验。Chat 函数的 `strict: true` 是另一项 Beta 能力：确实需要该行为时，应配置 `base_url = "https://api.deepseek.com/beta"`。Messages 的结构化输出继续使用 Grok Build 自带且会校验的 `StructuredOutput` 函数，因为 DeepSeek Anthropic 兼容接口的 `output_config` 只支持 effort。调用方显式提供的用户隔离 ID 会在协议桥接时保留，但 hellogrok 不会凭空生成，也不会从无关身份请求头推导。Chat 返回 `insufficient_system_resource` 时，hellogrok 会输出结构化失败；若原生非流式协议仍能改写 HTTP 响应，则返回可重试的 `503`。

服务商 API 的范围大于 Grok Build 能直接调用的范围。DeepSeek 当前的 Responses 兼容表把 `include`、图片/文件输入、previous response、服务端 conversation、background、truncation 和 `stream_options` 标为不支持；原生 `file_search`、`code_interpreter`、`computer_use`、`mcp` 工具类型会被忽略。当前不支持的字段会被接受并静默忽略，因此 hellogrok 会保留 Web Search 来源 `include` 提示等面向未来的标准字段。这不会禁用 Grok Build 本地执行的 Shell、MCP、Task、文件和 `apply_patch` 工具：当前 Grok Build 源码会把它们作为受支持的函数工具发送。DeepSeek 还定义了名为 `apply_patch` 的自定义工具格式；调用方主动提供时 hellogrok 会原样保留，但当前 Grok Build 源码不会把返回的 `custom_tool_call` 分派给本地补丁执行器，因此这里不把协议透传宣传成本地编辑路径。DeepSeek Anthropic API 不支持图片/document 输入、code execution 块或原生 MCP 块。DeepSeek API 另有对话前缀续写和 FIM，但 hellogrok 不会虚构 Grok Build 普通 Agent 从未调用的界面或补全端点。

缓存保持供应商原生语义。hellogrok 在三种协议下都保留有序的会话、工具、推理历史以及 Grok Build 的 `x-grok-conv-id`；Responses 缓存键保持稳定，原生 Messages 的 `cache_control` 标记会保留，Responses 转 Messages 时则加入与 Grok Build 相同的 system、当前 tip 和上一轮结束位置标记。历史 backend 搜索使用 Grok Build 的稳定文本摘要，不再伪造 Messages 服务端工具块；实时调用和结果仍使用供应商原生格式。DeepSeek 会忽略 Responses 缓存键参数并自动管理前缀缓存，因此这些无效字段只保持稳定，不会被当成缓存开关。缓存用量字段会保留或投影到接收协议，但只有真实供应商响应才能证明发生了缓存命中。

## 下载

从 [GitHub Releases](https://github.com/hellowind777/hellogrok/releases/latest) 下载最新标签构建。Windows 发布包分别提供托盘程序和控制台程序；Linux 与 macOS 发布包提供标准前台 CLI。

| 平台 | 发布文件 |
|------|----------|
| Windows amd64 / arm64 | `hellogrok-windows-<arch>.exe` 和 `hellogrok-cli-windows-<arch>.exe` |
| Linux amd64 / arm64 | `hellogrok-linux-<arch>` |
| macOS Intel / Apple Silicon | `hellogrok-darwin-<arch>` |

每个二进制文件旁都有对应的 `.sha256` 文件。在 Windows 上，运行 amd64 托盘程序前可执行：

```powershell
$artifact = ".\hellogrok-windows-amd64.exe"
$expected = ((Get-Content -LiteralPath "${artifact}.sha256") -split '\s+')[0]
$actual = (Get-FileHash -LiteralPath $artifact -Algorithm SHA256).Hash.ToLowerInvariant()
$actual -eq $expected
```

最后一条命令必须输出 `True`。Linux 使用 `sha256sum -c <file>.sha256`，macOS 使用 `shasum -a 256 -c <file>.sha256`。当前发布二进制尚未签名，校验和能够证明文件完整性，但不能代替发布者身份签名。

## 快速开始

### 前置条件

- Grok Build 可以读取 `~/.grok/config.toml`，其中至少配置了一个自定义模型 URL。
- 每个自定义渠道均有有效的凭据来源。
- 从源码编译需要 Go **1.26.6**。

兼容性以每次获取到的 Grok Build 最新 `origin/main` 为验证基线，不针对旧发布分支；本机 Grok Build 可独立更新，因此交付测试结果会记录实际验证的提交。

可以通过 `GROK_HOME` 指定 `~/.grok` 以外的 Grok 配置目录。

### Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
.\dist\hellogrok-cli.exe routes
.\dist\hellogrok.exe
```

通过托盘菜单选择“启动代理”。新开的 Grok Build 进程会直接读取代理配置；已经打开且连接共享 leader 的空闲自定义模型会话会自动热切换。

### Linux 或 macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

成功启动时会显示本地渠道端点和配置改写成功信息。使用 Grok Build 期间保持进程运行；Ctrl+C 或 SIGTERM 会停止代理并恢复原始配置。

### 首次使用检查

1. 执行 `hellogrok routes`，确认需要使用的自定义模型均已列出，后端和鉴权来源正确。
2. 启动 hellogrok；若 Grok Build 已打开，查看状态或日志中的共享 leader 热切换结果。
3. 先在对话中写入一条不敏感的唯一信息，再通过 `/model` 切换计划使用的模型，并确认后续模型仍能引用可见会话历史。
4. 根据当前搜索模式分别测试 `web_search` 和 `web_fetch`。
5. 正常停止 hellogrok，确认 Grok Build 配置不再指向本地代理。

## 平台支持

| 平台 | 标准交互方式 | 标签发布产物 | 架构 |
|------|--------------|--------------|------|
| Windows | 原生托盘和 CLI | GUI 与控制台 `.exe` | amd64、arm64 |
| Linux | 前台 CLI 或 systemd 用户服务 | CLI 二进制 | amd64、arm64 |
| macOS | 前台 CLI 或 LaunchAgent | CLI 二进制 | amd64、arm64 |

标准发布二进制使用 `CGO_ENABLED=0`，标签发布流程会同时生成 SHA-256 校验文件。

Linux 和 macOS 用户可以从源码编译可选托盘界面：

```bash
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

Linux 托盘构建需要 GTK 3 和 AppIndicator 开发包，macOS 托盘构建需要 Xcode Command Line Tools。标准 Unix CLI 不依赖这些桌面组件。

当前 Windows 和 macOS 产物没有代码签名或公证。

## 托盘与 CLI

### 托盘功能

Windows 托盘程序和可选 Unix 托盘版提供：

- **启动代理**：首次打开默认启用；之后启动或停止代理时记住当前选择。
- **开机启动**：启用或禁用登录自启动。
- **状态与日志**：打开当前状态和实时日志窗口。
- **退出**：尝试恢复配置并停止代理，随后始终结束托盘进程；未完成的恢复事务会留给下次启动继续处理。

同一登录会话只运行一个托盘实例；再次打开会直接退出，不会创建第二个托盘。托盘记忆状态与前台运行的 `hellogrok start` 命令相互独立。

Windows 的“状态与日志”分割工具条提供自动清理天数选择和日志搜索。保留天数按 hellogrok 实际写过日志的不同日期计数，而不是按连续自然日计数；默认保留最近 7 个使用日，可选关闭、3、7、14、30。清理在下次启动应用时执行。重复点击“搜索”会跳到下一处匹配并在末尾回到开头。状态文本自动换行，原始日志行保持不换行，便于逐行检查。

**停止保护：** 当其他供应商管理工具持有 Grok Build，或临时 hellogrok 路由无法安全恢复时，“启动代理”开关和前台信号处理仍会保持失败关闭。托盘“退出”不同：完成清理尝试后始终结束进程，外部所有权冲突不会再把用户困在程序中。

代理运行期间产生的配置修改会在停止时按字段合并：仍等于 hellogrok 临时值的字段恢复为启动前值，用户改过的字段和已删除的模型渠道则原样保留。无关编辑让 TOML 暂时无效时，hellogrok 会独立比较每个受管赋值，只恢复仍归自己所有的值，逐字节保留用户的无效文本，并通过文本检查确认是否还有本地路由。若仍无法证明恢复安全，恢复状态会原样保留，但托盘退出仍会完成。

### 与 CC Switch 兼容

只有在 CC Switch 不管理 Grok Build 时，它才能与 hellogrok 同时运行。CC Switch 的 Grok Build 代理接管和供应商切换都会写入 `~/.grok/config.toml`；即使两个代理监听不同端口，同时操作仍会发生配置所有权冲突。

- 检测到 CC Switch 的 Grok Build 接管标记（其 `/grokbuild/v1` 地址上的 `PROXY_MANAGED`）时，hellogrok 会拒绝启动。
- 如果先启动 hellogrok、随后误开 CC Switch 接管，普通停止代理操作仍会等待 CC Switch 先释放 Grok Build。托盘“退出”仍会关闭 hellogrok 并保留恢复事务；先释放 CC Switch 仍是最干净的停止顺序。
- 如果供应商管理工具已整份替换 Grok 实时配置，且其中不再包含任何 hellogrok 地址，hellogrok 会保留外部配置并放弃过期的恢复状态。
- hellogrok 运行期间，CC Switch 仍可管理 Claude、Codex、Gemini 等其他应用。

如果误开了两个 Grok 代理，应先关闭 CC Switch 的 Grok Build 代理接管，再停止 hellogrok。hellogrok 运行期间不要切换 CC Switch 的 Grok Build 供应商。

### CLI 命令

| 命令 | 用途 |
|------|------|
| `hellogrok start` | 在前台运行代理。 |
| `hellogrok version` | 输出当前安装版本。 |
| `hellogrok routes` | 列出自定义渠道路由，不输出凭据。 |
| `hellogrok restore` | 异常退出后恢复代理管理的设置。 |
| `hellogrok normalize-config` | 校验 `config.toml` 后显式删除 UTF-8 BOM，不猜测或转码其他编码。 |
| `hellogrok autostart enable` | 为当前可执行文件启用登录自启动。 |
| `hellogrok autostart disable` | 禁用登录自启动。 |
| `hellogrok autostart status` | 查看当前自启动状态。 |
| `hellogrok log` | 输出并打开日志文件。 |
| `hellogrok logview` | 在当前终端持续查看日志。 |
| `hellogrok help` | 显示命令帮助。 |

### 运行数据

| 平台 | 位置 |
|------|------|
| Windows | `%LOCALAPPDATA%\hellogrok` |
| Linux 和 macOS | `~/.hellogrok` |

运行数据包括应用偏好、日志、用于恢复代理管理配置的恢复状态，以及 `reasoning_provenance.json`。hellogrok 自己拥有的文件统一写成 UTF-8 无 BOM。推理来源索引只保存不透明推理值和路由签名域的 SHA-256 摘要，不保存原始推理、渠道 ID、模型名、上游 URL 或凭据。

日志保留规则在所有平台生效。原生下拉框和窗口内搜索目前仅适用于 Windows，因为标准 Linux 与 macOS 构建使用终端日志视图，没有对应的 Win32 状态窗口。

## 开机启动

### Windows

通过托盘启用“开机启动”，或执行 `hellogrok autostart enable`。登录启动会打开托盘，并按照记忆的代理启用状态运行。

### Linux

标准 CLI 会注册 systemd 用户服务。启用并立即启动：

```bash
./dist/hellogrok autostart enable
systemctl --user start hellogrok.service
systemctl --user status hellogrok.service
```

### macOS

标准 CLI 会注册当前用户的 LaunchAgent。启用并立即加载：

```bash
./dist/hellogrok autostart enable
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hellogrok.proxy.plist"
```

自启动配置会记录当前可执行文件的绝对路径；移动二进制后需要禁用再重新启用。`env_key`、`env_http_headers` 或 `GROK_HOME` 使用的变量必须对登录启动进程可见，不能只存在于当前终端。

## 工作原理

```text
Grok Build
    |
    v
hellogrok local channel proxy
    |
    v
Configured custom API channel
```

启动时，hellogrok 校验每个自定义渠道，并把 URL 指向渠道隔离的本地路由。未启用后端搜索且未被选为默认搜索模型的渠道保持配置的 Grok Build 消费器和原生流格式。启用能力或被选中的搜索渠道会临时投影为 Responses，因为只有该 Grok Build 消费器会序列化 hosted 工具并展示结构化搜索结果；hellogrok 再在供应商边界完成转换，并在停止时恢复原始配置。

Responses 供应商继续使用 Responses。Messages 供应商接收 Messages 请求，其结果通过双向转换器返回为 Responses 事件。Chat 供应商使用 `web_search_options`、`search_parameters`，或按配置桥接到 Messages/Responses。当自定义渠道被选为客户端搜索模型时，Grok Build 固定的非流式 `WebSearchClient` 请求也复用同一组供应商适配器。

原生 `web_search`、`web_fetch`、Grok 官方登录行为和受支持的子代理工作流仍由 Grok Build 管理，不会被替换成独立搜索服务。

## 故障排查

### 启动时报告 TOML 或 UTF-8 错误

错误会显示 `config.toml` 路径，并在可以定位时给出准确行号和列号。标准 UTF-8 无论是否带 BOM 都可以直接读取。程序不会猜测或静默转码其他编码；请先用编辑器把文件另存为 UTF-8，再重新启动。

如果希望主动删除有效的 UTF-8 BOM，应先停止 hellogrok 和正在接管 Grok Build 的供应商管理工具，再执行 `hellogrok normalize-config`。该命令会先校验整份 TOML，再原子写入；输入无效时保持文件不变。普通启动不会静默删除 BOM。

### 没有发现自定义路由

确认目标 `[model.<id>]` 或其引用的 provider 配置了有效的 `base_url` 或 `api_base_url`。没有自定义 URL 的官方模型会被有意排除。

### 无法使用 `web_search`

先查看启动日志中的 Build 协议和上游协议，再检查首次失败的真实搜索请求。标记为启用的 Responses 渠道必须实现 Responses hosted 工具，Messages 必须支持 `web_search_20250305`，Chat 必须支持选定的 `chat_search_dialect`。标记为 false 的渠道则需要有效的 `[models].web_search` / `GROK_WEB_SEARCH_MODEL`，或可用的 xAI 官方凭据。选中该自定义渠道后会在运行期临时启用其后端搜索；供应商拒绝或不可重试的“未完成 backend web_search”错误就表示上游缺少真实能力，代理不会用其他回退掩盖。`web_fetch` 与搜索模型独立，但仍可能被当前工具权限排除。

### 请求返回 401、403 或 502

执行 `hellogrok routes` 并查看“状态与日志”，确认渠道 URL、后端、凭据来源、模型标识和服务商状态。上游故障、限流、不支持的载荷或被中转丢弃的搜索工具需要由服务商或中转解决。

502 也可能表示上游返回了结构不完整的成功响应。hellogrok 会在转发前校验 Responses、Messages 或 Chat Completions 的最小响应结构，日志会指出缺失或类型错误的字段。

渠道 ID 含点号时优先使用 `[model."完整.ID"]`。未引用的 `[model.foo.bar]` 会被 TOML 解析成嵌套表，Grok Build 原本只看到 `foo`；hellogrok 启用后会临时规范化并验证该表头。`name` 中的点号或连字符不会参与鉴权。

### `tool_use` ID 后没有紧邻的 `tool_result`

这个供应商错误表示 Messages 会话历史结构无效：只要 assistant 消息含一个或多个 `tool_use`，紧邻的下一条 user 消息就必须用开头的 `tool_result` 块完整解析这一批调用。hellogrok 会校验原生历史，并在 Responses 转 Messages 时把并行调用及结果分别合并为紧邻的一条 assistant/user 消息后再请求供应商。真正缺失的结果仍返回不可重试的 `400`，不会伪造工具状态。

### Messages 提示 `serialization error: missing field signature`

请使用当前构建重新启动代理。部分 Messages 兼容中转会在流式 `thinking` 起始块中省略必需的空 `signature`，随后才通过 `signature_delta` 发送真实不透明值；Grok Build 的原生 Messages 解码器会在读取增量前拒绝这个不完整起始块。hellogrok 只补齐缺失的协议字段，并原样保留真实增量供后续轮次使用。若中转始终不发送真实签名，下游无法重建可验证的隐藏推理，只能由供应商修复 Messages 响应。

### 输出最后一次性出现

对于流式请求，hellogrok 会向选定的供应商 API 发送 `stream=true`。未启用能力的渠道保持原生 SSE；启用能力的 Messages 和 Chat 渠道会被增量转换成 Responses 事件，使 Grok Build 能消费推理、文本、函数调用、`web_search_call`、来源和终止状态。日志若提示缓冲回退，说明上游只返回了一次性完整 JSON，本次请求无法实现真正流式。

当前 Grok Build 有两条 Responses 来源展示路径：hosted 搜索读取 `web_search_call.action.sources`，客户端 `web_search` 工具读取 `output_text.annotations` 中的 URL 引用。对 hosted 搜索而言，非空来源 URL 会生成折叠标题后的 `(N sites)` 和可展开的编号 URL 正文；没有 URL 就没有站点数和可展开内容。转发 Responses hosted 搜索前，hellogrok 会加入 `web_search_call.action.sources` 到 `include`，且不会覆盖调用方已有项目。DeepSeek 当前会接受但静默忽略该提示，因此代理直接规范化其原生 Responses 搜索 action，来源展示仅限它实际返回的 URL；保留该提示可让 DeepSeek 未来支持后自动生效。Responses 顶层 `citations`、Messages 搜索结果/引用块，以及 Chat 的 `annotations`、`citations`、`search_results` 或 `web_search_results` 都会归一化到 Grok Build 的两条展示路径。WebSearchClient 适配器复用相同逻辑；只有响应能独立证明已执行搜索时，才从最终回答恢复有效 HTTP(S) 链接。普通回答链接不会凭空创建搜索调用。如果供应商没有返回真实 URL，仍可显示搜索活动，但不能伪造可信站点数。

### 出现 `unknown variant keepalive` 或持续 `Waiting for response...`

请把两个 hellogrok 可执行文件升级到相同的当前发布版或构建版，然后重启代理。部分中转会向 SSE 流注入私有 `keepalive`、`keep-alive`、`keep_alive`、`heartbeat` 或 `ping` 事件；Grok Build 严格的 Responses 反序列化器会拒绝这些 JSON 事件，即使上游仍在生成。hellogrok 会从 SSE `event:` 字段、JSON `type`/`event` 字段、裸数据载荷以及空数据心跳帧中吸收这些名称，再输出标准的 `: keepalive` 注释。收到 Responses 完成事件、Messages `message_stop` 或 Chat Completions `[DONE]` 后，也会立即关闭上游请求，不再等待服务商套接字。

流结束日志会包含 `heartbeats=<数量>`。若仍出现同一错误且该计数始终为零，请用 `hellogrok routes` 确认 Grok Build 确实经过当前代理；此时服务商很可能使用了其他私有事件名，应根据不含凭据的流抓取结果诊断，而不是添加模型专用绕过逻辑。

普通渠道最多等待上游响应头 601 秒，任意响应正文（包括非流式 JSON 和错误正文）的两次读取之间也最多等待 601 秒，比 Grok Build shell 默认的 600 秒多一秒，以便由 Grok Build 原生负责超时分类。`api.deepseek.com` 官方路由使用 660 秒，因为 DeepSeek 最长可排队十分钟。非流式排队空行或流式心跳都属于有效数据，会刷新空闲时限。响应头返回前或读取非流式正文时超时会得到可重试的 `504`；若 `200` 流已经开始，则输出接收协议兼容的 `proxy_stream_error` 并关闭上游。代理不设置请求总时限，因此持续有数据的长响应不会被终止。日志中的 `response_model` 会同时显示上游声明模型和配置模型：`mismatch=true` 表示中转静默替换了模型，`conflict=true` 表示不同响应帧声明了不同模型。

### 自动压缩一直不触发

Grok Build 不会把每次响应的 `usage.total_tokens` 累加到一个总和。当前公开源码会在 Responses 终止事件的 `usage.context_details.input_tokens` 与 `usage.context_details.output_tokens` 都有效时，用两者之和覆盖类型化的 `total_tokens`；没有该扩展时才沿用供应商的 `total_tokens`。随后 Chat State 把这个结果视为“当前完整上下文”的测量值，用它覆盖 token 基线，再加上该响应之后由本地估算的新增项，并在下一次请求模型前检查配置的自动压缩阈值。

hellogrok 按这套合同统一处理所有渠道，而不是按供应商名称分支。完整有效的 Responses 用量可以采用 `input_tokens` / `output_tokens`，也可以采用常见的 `prompt_tokens` / `completion_tokens` 别名；详情容器会按相同方式规范化。Messages 与 Chat 转为 Responses 后也遵循同一规则。只有完整可信的一对计数才会得到 `context_details`；缺失、冲突、负数、小数、溢出或占位测量统一保持 `usage: null`，让 Grok Build 保留原基线。

DeepSeek 的 1M 上下文是输入与生成输出共享的总预算，Responses 的 `max_output_tokens` 同时包含隐藏推理和可见输出。用尽单次输出上限时，Responses 返回 `status = "incomplete"` 且原因为 `max_output_tokens`；Chat 返回 `finish_reason = "length"`，hellogrok 会映射为同一种不完整结果。已生成的部分仍可使用，但若推理消耗了额度，可见正文可能很少。此时可以在下一轮继续、降低推理档位，或仅在总上下文剩余空间足够时提高输出额度。只达到输出上限本身不应触发自动压缩。

在 `664K / 1.0M` 附近出现上下文过长，不代表存在隐藏的 `500K` 上限，也不能据此认定统计错误。当窗口为 `1,048,576` 且 `max_completion_tokens = 384000` 时，供应商会同时预留完整输出额度，所以消息一旦超过 `664,576` 就可能拒绝。例如 `664,712 + 384,000 = 1,048,712`，虽然消息占用约 63%，总请求仍比窗口多 136 token。自动预算会预留该输出额度和 5% 安全余量，把 85% 首选值限制为 58%，让后续轮次更早压缩。若本次请求前还不知道正确容量，hellogrok 仍会识别精确、结构化且数字自洽的供应商错误，只把当前请求的输出额度改为“最大窗口 - 消息 token”，透明重试一次，不会伪造界面统计。

活动工具循环中显示超过 100% 是另一条路径。Grok Build 的常规阈值检查发生在采样前，而工具结果后的独立预检只有在估算值已经大于上下文窗口时才触发。一次较大的工具输出因此可能让活动上下文从阈值以下直接增长到明显超过 100%，随后才压缩。这是当前活动上下文的估算值，不是累计计费用量。

旧版 hellogrok 会把供应商缺失的用量补成全零字段。这样语法有效的响应就会报告 `total_tokens: 0`，导致 Grok Build 每轮都把基线重置为零，始终无法达到配置阈值。当前版本会保留可信的供应商总量；只有输入和输出测量都完整时才推导总量；缺失、不完整、负数、小数、全零占位或其他不可信用量统一输出 `usage: null`。原生 Responses 若带有完整且为正数的 `context_details`，即使独立的计费计数为零，仍可作为实时上下文计量使用。

对于 DeepSeek 官方端点，hellogrok 还会请求流式终止用量并保留供应商计费 `total_tokens`。容量元数据对 DeepSeek、GPT、Claude、Grok、Gemini 及其他渠道统一遵循一个规则：显式 `[model.*]` 或继承的 `[model_providers.*]` 值优先；没有配置时才透传并学习有效的上游 `X-Grok-*` 响应头。上下文拒绝还可能通过 `context_window`、`max_context_tokens`、`maximum_context_length`、`maximum_context_tokens`、`model_context_window` 或 `max_model_len` 暴露窗口；hellogrok 只接受唯一、正数、无歧义且不溢出的值。供应商级 `max_completion_tokens` 和可信学习到的 `context_window` 会在当前 Grok Build 无法正确继承或处理响应头降级时临时物化到 `[model.*]`。终止用量更新分子；Grok Build 依据完整分母计算触发点，因此 hellogrok 会按文档中的输出预留和安全余量临时降低每个不安全的模型阈值。

当前 Grok Build 会有意忽略响应头对上下文窗口的降级。单纯转发响应头因此无法纠正模型目录或回退分母；hellogrok 现在会把可信学习窗口写入临时模型配置，并刷新已打开会话。该修正只能在供应商暴露容量后生效，因此显式设置 `context_window` 与 `max_completion_tokens` 仍是首轮行为可预测的唯一方式。若某个中转同时不返回容量和用量，代理在没有供应商元数据、tokenizer 和隐藏提示开销的情况下仍无法制造精确结果；`usage: null` 会保留 Grok Build 既有基线，而不是再次破坏它。

这也是内置 `grok-4.5` 通常显示正确的原因：Grok Build 模型目录提供 500,000 token 窗口和该模型专用的 80% 阈值，xAI 原生 Responses 流又提供采样器所需的实时上下文 `context_details`。未知自定义模型若未配置或发现容量，只会回退到 200,000 token；旧版 hellogrok 还可能把它的实时基线重置为零。系统不存在通用的隐藏 500K 截止点。

如果一个已经结束或恢复的会话在有效阈值之上，下一次用户输入会先触发 Grok Build 的采样前自动压缩，然后才向供应商发请求。hellogrok 的进程级测试已覆盖 Responses、Messages 与 Chat Completions 的恢复会话路径。活动轮次中的大工具结果仍可能在两次检查之间越过阈值；随后同一工具循环的下一道保护会压缩当前活动上下文。

常规 DeepSeek 请求的完整输入/输出用量可以精确更新上述基线。服务端 Web Search 是唯一需要保留的语义边界：DeepSeek 官方 Responses 文档没有公开 `context_details`，也没有说明一次服务端多步搜索的输入/输出用量代表最终活动上下文还是累计计费量；hellogrok 只能把完整返回值投影到 Grok Build 的该扩展中。因此自动压缩不会再因零基线失效，但 hosted search 的精确触发时点仍取决于 DeepSeek 返回值的实际语义，需要用真实 API 响应继续校验。非 DeepSeek 渠道同理：有可信终止用量且配置了正确 `context_window` 时可精确触发；完全不返回用量时只能保留基线并近似增长。

### Grok Build 整个窗口无法点击

先区分“界面无响应”和“任务仍在长时间运行”。如果 hellogrok 与 Grok Build 日志仍持续出现请求、心跳、工具调用或阶段变化，Agent 仍在执行或等待供应商队列；DeepSeek 合法排队时间最长可达十分钟。上述正文空闲时限已经避免上游静默连接永久占住代理路径。

如果整个窗口确实不再处理输入，日志也同时停止推进，仅凭 HTTP 代理无法证明 UI 线程死锁。记录准确时间，并保留同一时刻的 Grok Build unified 日志、hellogrok 日志、Windows Application Hang/崩溃事件及可用的进程 dump。出现 `504` 或 `proxy_stream_error` 说明是已受限的上游等待；没有这两类信号的渲染器或进程挂起需要 Grok Build 侧证据，不能用服务商专用网络绕过来掩盖。

### Claude Messages 渠道选错模型或返回 404

请使用复数写法 `api_backend = "messages"`。Grok Build 只定义 `chat_completions`、`responses`、`messages` 三种后端；hellogrok 会拒绝废弃的单数拼写。`base_url` 必须填写追加 `/messages` 之前的 API 根地址：若真实端点是 `/v1/messages`，配置应以 `/v1` 结尾。启用能力的 Messages 渠道会临时向 Grok Build 显示为 Responses，但上游仍调用该 `/messages` 端点。同时确认 `model` 填写的是供应商实际支持的上游模型 ID，而不是渠道 ID。

### 已打开窗口没有随代理切换

查看“状态与日志”中的“Grok 会话热切换”。自动热切换只适用于共享 leader 中的空闲自定义模型会话，并兼容新旧 ACP 模型切换方法。Windows 上若 Grok Build 1.0.x 把活动的命名管道 leader 误报为 stale，hellogrok 只会在其 leader 锁确实被占用时接管。正在生成或等待输入的会话会被安全跳过；完成当前操作后在 `/model` 中重新选择当前模型。使用 `--no-leader` 打开的窗口没有可供 hellogrok 连接的外部 IPC，也需要手动重选或新开窗口。

### 切换模型后提示必须新建会话

Grok Build 在 `/model` 切换后会重放全部历史推理项，其中可能包含服务商加密状态。hellogrok 会记录其来源签名域，并仅从目标请求中删除已知异域的加密推理；普通消息、工具调用、工具结果、搜索历史和未加密推理均保持不变。对于早于本地来源索引的旧会话不透明状态，hellogrok 首次仍保持透传，只有上游返回结构化签名或解密拒绝时才执行一次清理重放；若确定性拒绝再次发生，则标记为不可重试，避免进入 Grok Build 通用重试循环。

### 强制退出后配置仍指向 localhost

先确认没有 hellogrok 进程正在运行，再执行 `hellogrok restore`。不要对正在运行的代理执行 `restore`。

### 修改运行中的配置后仍无法停止代理

hellogrok 会在停止时逐字段合并代理受管配置，因此修改 `supports_backend_search` 或让无关 TOML 设置暂时未完成，都不需要在退出前手动回滚。只有恢复后仍发现不属于原事务结构的 `127.0.0.1:18787` 路由、配置文件无法访问，或其他工具持有配置时，普通停止才会推迟。恢复模型表头或把临时 URL 改为预期上游地址即可干净停止。托盘“退出”始终结束进程，并把未解决的恢复状态留给下次启动。

### 端口 `18787` 已占用

启用代理前先停止占用 `127.0.0.1:18787` 的进程。hellogrok 会先占用端口再修改 Grok 配置；端口不可用时直接显示启动错误。它不会静默改用随机端口，因为临时渠道地址与恢复状态必须使用同一地址。

### 开机启动成功，但渠道没有凭据

把只在终端中存在的环境变量写入持久用户环境或服务环境，然后重新启动登录服务。自启动进程无法继承先前终端会话里的临时变量。

### 供应商管理工具阻止干净停止代理

先打开供应商管理工具（如 CC Switch）关闭其 Grok Build 接管，再停止 hellogrok，以保持正确的配置恢复顺序。若必须立即结束，托盘“退出”仍会关闭 hellogrok，并保留待处理的恢复事务。

## 开发与测试

执行本地质量检查：

```bash
go test ./... -count=1
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Windows 用户配置真实渠道后，可以运行集成冒烟测试：

```powershell
.\scripts\run_grok_all_channels_test.ps1
.\scripts\run_grok_all_channels_test.ps1 -RequireWebSearch -MaxTurns 1 -TimeoutSeconds 150
.\scripts\run_grok_all_channels_test.ps1 -RequireSubagentSearch -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -RequireWebFetch -MaxTurns 2 -TimeoutSeconds 150
```

CI 会在 Windows、Linux、Intel macOS 和 Apple Silicon macOS 上运行测试与默认构建，并在 Linux 和 macOS 原生构建可选托盘目标。标签发布会生成三个操作系统的 amd64 与 arm64 产物。

## 使用限制

- hellogrok 无法创造服务商侧的搜索能力；hosted search 渠道必须真实支持搜索并返回结果。
- Responses 到 Messages/Chat 的普通会话转换只对已启用能力的渠道开放（显式 `supports_backend_search = true`、被选中的默认搜索模型、DeepSeek 官方端点默认值，或 Grok Build 远程模型目录解析出的 hosted-search 请求），另加 Grok Build 固定的非流式 WebSearchClient 请求；其他跨协议请求会被拒绝。
- 中转如果主动删除工具声明、工具调用、引用或结果事件，下游无法完整恢复。
- 供应商若无视 `stream=true`，等完整 JSON 已经返回后无法再变成真正流式；hellogrok 会记录并使用缓冲兼容回退。
- 服务商加密的隐藏推理只属于其来源签名域。跨域切换会保留可见会话与工具历史，但会主动排除不兼容的私有推理。
- 超出受支持 Responses、Chat Completions 和 Messages 格式的服务商私有扩展可能需要单独适配。
- 上游可用性、模型权限、账号池、限流和网关错误仍由服务商负责。
- 可选 Unix 托盘依赖已安装的桌面环境；标准 Unix CLI 是更通用的使用方式。
- 当前发布产物未签名；对本地信任有要求时应从源码构建。

## 参与贡献

1. 为改动创建目标明确的分支。
2. 遵循现有包边界，不夹带无关重构。
3. 行为变化需要新增或更新测试。
4. 执行上方质量检查。
5. 用户可见行为变化时同步更新两份 README。
6. 提交 Pull Request，并说明问题、实现方式和验证结果。

## 许可证

本项目采用 [MIT License](./LICENSE) 授权。

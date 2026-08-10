<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok 图标" width="128">
</p>

# hellogrok

跨平台 Grok Build 本地代理，让自定义模型渠道兼容常见 API 格式、Build 原生 Web 工具、独立鉴权和自动配置恢复。

[![Version](https://img.shields.io/badge/version-0.1.5-2f6feb.svg)](./internal/appinfo/appinfo.go)
[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8.svg)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Platforms](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg)](#平台支持)
[![LINUX DO](https://img.shields.io/badge/LINUX_DO-链接认可-0A84FF?logo=linux&logoColor=white)](https://linux.do)

[English](./README.md) · [简体中文](./README_CN.md) · [发布说明](./RELEASE_NOTES_CN.md) · [更新日志](./CHANGELOG.md)

> 🏅 此项目已链接认可 [LINUX DO](https://linux.do) 社区。

## 目录

- [为什么需要 hellogrok](#为什么需要-hellogrok)
- [功能](#功能)
- [搜索与配置](#搜索与配置)
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
- 当 `supports_backend_search = true` 时，临时让 Grok Build 将该渠道作为 Responses 消费，再在代理内把请求、响应和 SSE 事件转换到供应商的真实协议。三种上游格式因此都能向 Grok Build 返回 `web_search_call`、来源和站点数量。
- 当 `supports_backend_search = false` 或缺省时，Grok Build 保持使用配置的原生消费者，其客户端 `web_search` 依次使用 `[models].web_search`、`GROK_WEB_SEARCH_MODEL` 或已登录官方账号的回退路径。
- 提供渠道隔离的 `/responses`、`/messages` 和 `/chat/completions` 路由，并在代理停止时逐字节恢复原始 `api_backend`。
- 转发前校验各协议的工具历史：Responses 调用必须有匹配的 `function_call_output`，Messages 的 `tool_use` 必须由紧邻的下一条 user 消息中的 `tool_result` 完整配对，Chat 工具调用必须有匹配的 tool 消息。确定性错误返回不可重试的 `400`，不会进入 Grok Build 重试循环。
- 在私有 `keepalive`、`keep-alive`、`keep_alive`、`heartbeat`、`ping` 帧到达 Grok Build 前将其转换为标准 SSE 注释，不占用 Responses 事件序号；收到各协议的终止事件后立即关闭上游流。
- 将上游响应头等待和 SSE 数据间隔限制为 180 秒，但不对长时间运行的模型流设置总时限。收到任意上游字节（包括随后被规范化的心跳）都会重新计算流空闲时间。
- 在规范化前记录原始上游响应声明的模型，支持终止帧优先、大小写不敏感的不一致判断和多帧冲突标记，不改变路由或响应数据。
- Messages 的 `thinking` 起始块缺少空 `signature` 时补齐该字段，同时保留供应商随后发送的真实 `signature_delta`，使 Messages 兼容中转可被 Grok Build 的严格原生解码器消费。
- 保留每个渠道配置的上游 URL 路径和模型标识。
- 使用前准备所有显式自定义渠道，避免通过 `/model` 切换后首次请求失败。
- 热切换模型时保留可移植的会话历史，只排除已知属于不同渠道、协议、线上模型或上游端点的加密推理。

### 原生 Web 工具

- 支持 hosted 和客户端搜索两种 Grok Build 原生 `web_search` 工作流。
- 当 `supports_backend_search = true` 时，三种格式都使用当前渠道自身的 hosted 搜索：Responses 直接转发，Messages 使用 `web_search_20250305`，Chat 使用配置的搜索方言或协议桥接。
- 对启用能力的渠道，Grok Build 始终收到规范的 Responses 搜索事件，包括已完成的 `web_search_call`、已验证来源、引用、用量和原生去重站点数。
- 当 `supports_backend_search = false` 时，Grok Build 改用客户端 `web_search`。三种后端仍都可被选为客户端搜索模型；代理只适配 Grok Build 固定的非流式 WebSearchClient 请求。
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
- 正常停止、退出托盘、Ctrl+C、SIGTERM 或启动失败时恢复原始值。
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
| 设置了 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL` | Grok Build 客户端 `web_search` 使用所选模型。该模型可采用 `responses`、`messages` 或 `chat_completions`；它的 WebSearchClient 请求独立于 `supports_backend_search` 处理。环境变量优先，且选择过程不会发起启动请求。 |
| 任意渠道设置 `supports_backend_search = true` | Grok Build 使用 Responses hosted 工具，hellogrok 则调用该渠道自身实际可用的搜索 API：Responses、Messages `web_search_20250305`，或选定的 Chat 搜索方言/桥接。 |
| 任意渠道设置 `supports_backend_search = false` 或缺省 | Grok Build 使用客户端 `web_search`：优先 `[models].web_search` 或 `GROK_WEB_SEARCH_MODEL`，否则使用已登录官方账号的回退路径。 |
| 没有可用的 hosted 或客户端搜索路径 | 当前模型无法使用 `web_search`。 |
| `web_fetch` | 独立于搜索模型选择，并受当前工具权限限制。 |

hellogrok 不会创建、选择或替换 `[models].web_search`，启动时也不会发送搜索能力探测。标记为可用的 Messages 渠道必须真正支持 `web_search_20250305`。Chat 默认使用 `web_search_options`；DeepSeek 官方 Chat 自动桥接到 Messages，xAI 官方 Chat 自动桥接到 Responses。中转需要指定策略时，可把 `chat_search_dialect` 设置为 `web_search_options`、`search_parameters`、`messages` 或 `responses`。代理无法为供应商凭空增加搜索能力。

对于客户端搜索，hellogrok 会明确工具用途，但不会根据提示词推断或强制调用工具。强制选择只来自结构化 `tool_choice`。内部传输别名只修改协议定义的工具声明、选择和调用名称字段；响应正文、URL、工具参数、工具结果及其他业务 JSON 均保持不变。

### 配置示例

下面示例把一个支持 `web_search_options` 的 Chat Completions 渠道设为 Grok Build 客户端搜索模型：

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

任意渠道若要在普通会话中使用自身 hosted 搜索，都应仅在确认所选供应商 API 确实支持后设置 `supports_backend_search = true`。Messages 使用 `web_search_20250305`；DeepSeek 官方 Chat 自动调用 `/anthropic/messages`，其他 Chat 中转可显式选择 `chat_search_dialect`。

### 支持的渠道设置

| 设置 | 是否必需 | 默认值 | 用途 |
|------|----------|--------|------|
| `model` | 否 | 模型表 ID | 发送给上游渠道的模型标识。 |
| `base_url` 或 `api_base_url` | 是 | 无 | 自定义上游端点；没有自定义 URL 的模型不会进入代理。 |
| `api_backend` | 否 | `chat_completions` | 上游真实 API 格式：`responses`、`messages` 或 `chat_completions`。启用搜索能力的非 Responses 渠道只会在 Grok Build 侧临时投影为 Responses；上游格式及恢复后的配置不变。 |
| `chat_search_dialect` | 否 | 按主机判断 | Chat hosted 搜索策略：`web_search_options`、`search_parameters`、`messages` 或 `responses`。DeepSeek 官方默认 `messages`，xAI 官方默认 `responses`，其他主机默认 `web_search_options`。 |
| `api_key` | 三选一 | 无 | 静态渠道凭据；共享配置建议优先使用 `env_key`。 |
| `env_key` | 三选一 | 无 | 保存渠道凭据的环境变量名或按顺序尝试的名称列表。 |
| `auth_provider` | 三选一 | 无 | Grok 命令式鉴权提供器。 |
| `auth_scheme` | 否 | `bearer` | 上游鉴权方式；只有服务商明确要求 `X-Api-Key` 时才设置为 `x_api_key`。 |
| `extra_headers` | 否 | 空 | 额外的渠道自有 HTTP 请求头，包括供应商专用鉴权字段。拒绝由代理控制的分帧、内容和连接请求头；名称按大小写不敏感处理。 |
| `env_http_headers` | 否 | 空 | 从环境变量读取的 HTTP 请求头；解析后的值使用与 `extra_headers` 相同的请求头规则。 |
| `supports_backend_search` | 否 | `false` | 为 true 时，三种上游格式都使用当前渠道自身的 hosted 搜索，并向 Grok Build 输出规范 Responses 搜索事件；为 false 时，Grok Build 使用配置或登录回退的客户端搜索路径。 |

模型设置可以直接写在 `[model.<id>]` 下，也可以从引用的 `[model_providers.<id>]` 继承；模型级设置优先。

渠道 ID 含点号时，推荐按 TOML 语法引用完整 ID，例如 `[model."provider.v1-beta"]`。连字符不需要引用。`name = "Provider.v1-beta"` 只是显示名称，点号和连字符均可直接使用。hellogrok 也会在代理运行期间兼容旧的未引用点号表头，并在停止时恢复原文。

不要手动把自定义渠道 URL 设置成 hellogrok 的本地地址。本地临时 URL 只应由应用在代理运行期间管理。

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
- 从源码编译需要 Go **1.26.5**。

当前实测基线为 Grok Build **1.0.0**。Grok Build 的自定义模型行为可能继续变化，使用更新版本时应运行仓库内的冒烟测试。

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
- **退出**：恢复配置、停止代理并退出程序。配置所有权冲突时推迟退出。

同一登录会话只运行一个托盘实例；再次打开会直接退出，不会创建第二个托盘。托盘记忆状态与前台运行的 `hellogrok start` 命令相互独立。

Windows 的“状态与日志”分割工具条提供自动清理天数选择和日志搜索。保留天数按 hellogrok 实际写过日志的不同日期计数，而不是按连续自然日计数；默认保留最近 7 个使用日，可选关闭、3、7、14、30。清理在下次启动应用时执行。重复点击“搜索”会跳到下一处匹配并在末尾回到开头。状态文本自动换行，原始日志行保持不换行，便于逐行检查。

**退出保护：** 当供应商管理工具仍持有 Grok Build 配置所有权时，托盘会推迟退出以避免留下孤立代理地址——先解决配置冲突再退出。

代理运行期间删除整个模型渠道会被视为明确的用户操作：停止时保留该删除，同时恢复其余受管渠道和全局开关。只修改现有渠道中的单个代理受管字段仍会触发退出保护。

### 与 CC Switch 兼容

只有在 CC Switch 不管理 Grok Build 时，它才能与 hellogrok 同时运行。CC Switch 的 Grok Build 代理接管和供应商切换都会写入 `~/.grok/config.toml`；即使两个代理监听不同端口，同时操作仍会发生配置所有权冲突。

- 检测到 CC Switch 的 Grok Build 接管标记（其 `/grokbuild/v1` 地址上的 `PROXY_MANAGED`）时，hellogrok 会拒绝启动。
- 如果先启动 hellogrok、随后误开 CC Switch 接管，hellogrok 会拒绝停止或退出，直到 CC Switch 先释放 Grok Build，避免 CC Switch 日后恢复已停服的 `127.0.0.1:18787` 地址。
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

运行数据包括应用偏好、日志、用于恢复代理管理配置的恢复状态，以及 `reasoning_provenance.json`。推理来源索引只保存不透明推理值和路由签名域的 SHA-256 摘要，不保存原始推理、渠道 ID、模型名、上游 URL 或凭据。

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

启动时，hellogrok 校验每个自定义渠道，并把 URL 指向渠道隔离的本地路由。未启用后端搜索的渠道保持配置的 Grok Build 消费器和原生流格式。启用能力的渠道会临时投影为 Responses，因为只有该 Grok Build 消费器会序列化 hosted 工具并展示结构化搜索结果；hellogrok 再在供应商边界完成转换，并在停止时恢复原始配置。

Responses 供应商继续使用 Responses。Messages 供应商接收 Messages 请求，其结果通过双向转换器返回为 Responses 事件。Chat 供应商使用 `web_search_options`、`search_parameters`，或按配置桥接到 Messages/Responses。当自定义渠道被选为客户端搜索模型时，Grok Build 固定的非流式 `WebSearchClient` 请求也复用同一组供应商适配器。

原生 `web_search`、`web_fetch`、Grok 官方登录行为和受支持的子代理工作流仍由 Grok Build 管理，不会被替换成独立搜索服务。

## 故障排查

### 没有发现自定义路由

确认目标 `[model.<id>]` 或其引用的 provider 配置了有效的 `base_url` 或 `api_base_url`。没有自定义 URL 的官方模型会被有意排除。

### 无法使用 `web_search`

先查看启动日志中的 Build 协议和上游协议，再检查首次失败的真实搜索请求。启用能力的 Responses 渠道必须实现 Responses hosted 工具，Messages 必须支持 `web_search_20250305`，Chat 必须支持选定的 `chat_search_dialect`。标记为 false 的渠道则需要有效的 `[models].web_search` / `GROK_WEB_SEARCH_MODEL`，或可用的 xAI 官方凭据。`web_fetch` 与搜索模型独立，但仍可能被当前工具权限排除。

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

当前 Grok Build 有两条 Responses 来源展示路径：hosted 搜索读取 `web_search_call.action.sources`，客户端 `web_search` 工具读取 `output_text.annotations` 中的 URL 引用。WebSearchClient 适配器会把任一支持搜索后端的已验证 URL 写入两种结构：优先使用结构化结果和引用；只有响应能独立证明已执行搜索时，才从最终回答恢复有效 HTTP(S) 链接。普通回答链接不会凭空创建搜索调用。如果供应商没有返回真实 URL，仍可显示搜索活动，但不能伪造可信站点数。

### 出现 `unknown variant keepalive` 或持续 `Waiting for response...`

请把两个 hellogrok 可执行文件升级到相同的当前发布版或构建版，然后重启代理。部分中转会向 SSE 流注入私有 `keepalive`、`keep-alive`、`keep_alive`、`heartbeat` 或 `ping` 事件；Grok Build 严格的 Responses 反序列化器会拒绝这些 JSON 事件，即使上游仍在生成。hellogrok 会从 SSE `event:` 字段、JSON `type`/`event` 字段、裸数据载荷以及空数据心跳帧中吸收这些名称，再输出标准的 `: keepalive` 注释。收到 Responses 完成事件、Messages `message_stop` 或 Chat Completions `[DONE]` 后，也会立即关闭上游请求，不再等待服务商套接字。

流结束日志会包含 `heartbeats=<数量>`。若仍出现同一错误且该计数始终为零，请用 `hellogrok routes` 确认 Grok Build 确实经过当前代理；此时服务商很可能使用了其他私有事件名，应根据不含凭据的流抓取结果诊断，而不是添加模型专用绕过逻辑。

hellogrok 最多等待上游响应头 180 秒，SSE 两次数据读取之间也最多等待 180 秒；心跳属于有效数据，会刷新空闲时限。响应头返回前超时会得到可重试的 `504`；若 `200` 流已经开始，则输出接收协议兼容的 `proxy_stream_error` 并关闭上游。代理不设置请求总时限，因此持续有数据的长推理不会被终止。日志中的 `response_model` 会同时显示上游声明模型和配置模型：`mismatch=true` 表示中转静默替换了模型，`conflict=true` 表示不同响应帧声明了不同模型。

### Claude Messages 渠道选错模型或返回 404

请使用复数写法 `api_backend = "messages"`。Grok Build 只定义 `chat_completions`、`responses`、`messages` 三种后端；hellogrok 会拒绝废弃的单数拼写。`base_url` 必须填写追加 `/messages` 之前的 API 根地址：若真实端点是 `/v1/messages`，配置应以 `/v1` 结尾。启用能力的 Messages 渠道会临时向 Grok Build 显示为 Responses，但上游仍调用该 `/messages` 端点。同时确认 `model` 填写的是供应商实际支持的上游模型 ID，而不是渠道 ID。

### 已打开窗口没有随代理切换

查看“状态与日志”中的“Grok 会话热切换”。自动热切换只适用于共享 leader 中的空闲自定义模型会话，并兼容新旧 ACP 模型切换方法。Windows 上若 Grok Build 1.0.0 把活动的命名管道 leader 误报为 stale，hellogrok 只会在其 leader 锁确实被占用时接管。正在生成或等待输入的会话会被安全跳过；完成当前操作后在 `/model` 中重新选择当前模型。使用 `--no-leader` 打开的窗口没有可供 hellogrok 连接的外部 IPC，也需要手动重选或新开窗口。

### 切换模型后提示必须新建会话

Grok Build 在 `/model` 切换后会重放全部历史推理项，其中可能包含服务商加密状态。hellogrok 会记录其来源签名域，并仅从目标请求中删除已知异域的加密推理；普通消息、工具调用、工具结果、搜索历史和未加密推理均保持不变。对于早于本地来源索引的旧会话不透明状态，hellogrok 首次仍保持透传，只有上游返回结构化签名或解密拒绝时才执行一次清理重放；若确定性拒绝再次发生，则标记为不可重试，避免进入 Grok Build 通用重试循环。

### 强制退出后配置仍指向 localhost

先确认没有 hellogrok 进程正在运行，再执行 `hellogrok restore`。不要对正在运行的代理执行 `restore`。

### 端口 `18787` 已占用

启用代理前先停止占用 `127.0.0.1:18787` 的进程。hellogrok 会先占用端口再修改 Grok 配置；端口不可用时直接显示启动错误。它不会静默改用随机端口，因为临时渠道地址与恢复状态必须使用同一地址。

### 开机启动成功，但渠道没有凭据

把只在终端中存在的环境变量写入持久用户环境或服务环境，然后重新启动登录服务。自启动进程无法继承先前终端会话里的临时变量。

### 供应商管理工具持有 Grok Build 时无法退出

先打开供应商管理工具（如 CC Switch）关闭其 Grok Build 接管，再退出 hellogrok。这避免 CC Switch 日后恢复指向已停服代理的路由。

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
- Responses 到 Messages/Chat 的普通会话转换只对显式设置 `supports_backend_search = true` 的渠道开放，另加 Grok Build 固定的非流式 WebSearchClient 请求；其他跨协议请求会被拒绝。
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

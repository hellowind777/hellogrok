# 发布说明 — v0.1.20

## 上游瞬时 “busy” 故障不再打断本轮任务

第三方渠道过载时经常返回 `Service unavailable (503): The service is busy. Wait a minute and send again.`。旧版熔断器把连续几次这类失败当作渠道死亡，几秒内就强制结束本轮，白白浪费 Grok Build 原生的重试预算（15 次、约 5.5 分钟）。只要供应商进入繁忙窗口，长时间运行的 agent 任务就会中断。

hellogrok 现在完整保留客户端的重试预算，并在其前面增加吸收层：

- 响应头返回前的可重试软故障（busy/过载 `503`、`429`、可重试 `5xx`、响应头超时）会在代理内以指数退避（2s–30s，尊重上游 `Retry-After`，上限 60s）自动重试，默认最多等待 90 秒。窗口期内 Grok Build 完全看不到失败。`absorb_retry_max_secs = 0` 可按渠道关闭；`absorb_retry_backoff_cap_secs` 可调整退避上限。
- 窗口耗尽后，原始失败以可重试形式透传，客户端 15 次重试一次不少。透传的可重试 busy `5xx` 若没有上游 `Retry-After`，代理会补充 30 秒的 `Retry-After` 调整重试节奏。
- 传输错误刻意不吸收：Grok Build 首次重试时的 HTTP/1.1 客户端重建比代理内重放更擅长处理。
- `X-Should-Retry: false` 现在只用于重试必然无果的确定性错误（鉴权、权限、账单、额度、无效请求或模型），除此之外任何路径都不会缩短客户端的重试预算。
- 熔断器改为可选（`dead_channel_fail_fast = true`，默认关闭），且只统计拨号级失败——连接被拒、DNS、TLS 握手——这类失败重试永远不可能恢复。连续 6 次（可用 `dead_channel_fail_threshold` 调整）后代理返回不可重试的 `503 proxy_circuit_open`，冷却 5 分钟后放行一次探测，上游返回任何响应都会自动合闸。busy `503`、`429` 和超时永不计数。

升级后请重启代理。每次吸收等待都记录为 `UP absorb` 日志；死渠道快速失败显示为 `UP breaker` 日志。

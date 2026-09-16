# 发布说明 — v0.1.36

## 纯文本渠道不再因 `vision_not_supported` 终结回合

- **你看到的现象。** 会话一旦携带图像内容——例如 `read_file` 读取图片、Grok Build 将结果渲染为图像块——之后的每个请求都会把这些图像部分转发给上游。纯文本模型随即以 `400 vision_not_supported`（"The request model is not multimodal … does not support image input"）拒绝整个请求，回合停在红色报错上。
- **本次变化。** hellogrok 现在能在三种上游协议上识别该拒绝（`400` 且 code 含 `vision`，或消息含 `not multimodal` / `does not support image`）：把每个图像内容部分（`image_url`、`input_image`、`image`，含 tool result 内的图像）替换为一行文本占位符（告知模型视觉载荷被省略），并重试一次请求。回合不再失败，而是不带图像继续。渠道不会被记为纯文本——判定每次都来自上游的真实拒绝，上游日后获得视觉能力时渠道无需任何改动即可恢复透传。

## 参数名错误或零参数的第三方工具调用在分发前被归一或丢弃

- **你看到的现象。** 第三方模型偶尔按自身训练先验发出 Grok Build 的参数名——例如 `read_file` 写成 `target_path` 而非 `target_file`——Grok Build 的严格 schema 校验回以 `Failed to parse arguments for tool …: missing field …`，TUI 将该调用标为失败并消耗一轮往返。更罕见的中继/模型 glitch 会发出一个参数片段完全未到达的工具调用；空参数对象在同一校验下必然报 `missing field`。
- **本次变化。**
  - 参数别名按该请求已声明的工具归一：`target_path` 现在映射到 `target_file`、`target_directory` 或 `file_path`（取已声明 schema 实际拥有的那个），并入既有别名表。改写只作用于工具已声明的属性，无关 JSON 不受影响。
  - 一个参数片段都未累积的流式调用，在已声明工具含必填属性时于 Grok Build 分发前丢弃——空对象永远无法满足必填字段——缺陷以 `empty-args-discarded(name=…)` 代理日志注记保持可见。无必填属性的工具保留空参数，那是合法的零参调用约定。

## 行内推理不再泄漏进可见回复

- **你看到的现象。** 思考模型一个回合产出多段 CoT，部分中继还会把推理尾只带闭标签地误路由进 `content`。旧的 think 标签剥离器只处理答案最开头的 `<think>>…</think>` 块，于是第二阶段 span、无开标签的 `</think>` 推理尾、游离或跨增量分裂的闭标签都原样出现在回复气泡里。
- **本次变化。** Chat think 标签状态机现在剥离流中任意位置的 span：开标签前的文本立即下发，span 缓冲到闭标签后归入推理通道；回合开头无开标签的闭标签把其前的文本判定为推理；游离或跨增量分裂的闭标签被删除或挂起而不是显示。非流式路径剥离答案内容中的每一个 span。可见回复之后到达的推理仍按设计丢弃，因为 Grok Build 只把推理渲染为前缀 Thought。

升级后请重启两个 hellogrok 可执行文件。

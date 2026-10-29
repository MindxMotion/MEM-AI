# 渠道额外设置说明

该配置用于设置一些额外的渠道参数，可以通过 JSON 对象进行配置。主要包含以下三个设置项：

1. force_format
    - 用于标识是否对数据进行强制格式化为 OpenAI 格式
    - 类型为布尔值，设置为 true 时启用强制格式化

2. proxy
    - 用于配置网络代理
    - 类型为字符串，支持 `http`、`https`、`socks5` 和 `socks5h` 协议
    - 保存时必须包含协议和主机；仅允许空路径或根路径 `/`，不允许 query 或 fragment
    - SOCKS 代理未填写端口时，运行时使用默认端口 `1080`

3. thinking_to_content
   - 用于标识是否将思考内容`reasoning_content`转换为`<think>`标签拼接到内容中返回
   - 类型为布尔值，设置为 true 时启用思考内容转换

--------------------------------------------------------------

## JSON 格式示例

以下是一个示例配置，启用强制格式化并设置了代理地址：

```json
{
    "force_format": true,
    "thinking_to_content": true,
    "proxy": "socks5://proxy.example:1080"
}
```

--------------------------------------------------------------

通过调整上述 JSON 配置中的值，可以灵活控制渠道的额外行为，比如是否进行格式化以及使用特定的网络代理。

## 升级兼容性

旧版本会忽略代理地址中的 path、query 和 fragment。为避免升级后中断已有渠道流量，运行时会继续剥离这些遗留后缀，并对同一代理地址每个进程记录一次不含凭证和后缀的警告。该兼容逻辑不会改写数据库；再次保存渠道时必须按上述严格规则修正代理地址。

代理连接使用 30 秒 TCP 拨号超时和 30 秒 KeepAlive；TLS 握手超时为 10 秒。这些超时同样适用于未配置渠道代理的中转请求。

## Claude / Bedrock 请求兼容模式（按渠道显式启用）

仅用于通过 **Claude Messages 适配器**转发到不支持会话内 effort 配置的上游，不绑定渠道编号、域名或模型名称。默认关闭，原生模式不变。
在渠道的 `settings` JSON 字符串（`ChannelOtherSettings`，不是 `setting`）中合并以下字段，保留原有其他配置：

```json
{
    "claude_bedrock_compat": true
}
```

- 只转换 `role: "system"`、`content: ""` 或 `[]`、且 `output_config` **仅有**合法非空 `effort` 的标记；移除这些标记，把本次生成**当前生效**的 effort 合并到顶层 `output_config`。合法值为 `low`、`medium`、`high`、`xhigh`、`max`；不保证所有上游/模型都支持这些级别，不做 effort 降级。
- 按 [Anthropic effort 文档](https://platform.claude.com/docs/en/build-with-claude/effort#change-effort-mid-conversation)解释：顶层 effort 是初始值；紧接新用户输入的标记对该次回复生效，其他位置的标记等待下一次新用户输入，后来的待生效标记覆盖前面的待生效标记。只有 `tool_result` 的用户消息不会激活待生效标记。紧接新用户输入后的连续标记存在时序歧义，拒绝转换。
- 保留顶层输出格式及未知配置、普通消息和工具块、系统提示词、`safeguards`、数值精度，以及原有 thinking 类型/预算和 `max_tokens` 默认值/后缀处理行为。不会丢弃安全字段；上游仍可能因 `safeguards` 不受支持而返回 400，HTTP 200 也不证明安全配置已被执行。
- `thinking.display` 的 `summarized` / `omitted` 原样保留；其他字符串（含显式空字符串）变为 `summarized`；缺失时不添加，不关闭 thinking。非字符串值拒绝转换。
- 普通会话内 system 内容、携带额外字段/未知配置的标记、非 system 消息的 `output_config`、未知角色、重复配置键、无法判定的新用户输入、禁用 thinking 时的 effort 标记等情况返回不重试的 400，而不是静默丢弃。顶层 `system` 仍正常支持。
- 转换位于参数覆盖之后，因此覆盖新引入的标记和 display 也会被处理。全局/渠道请求体透传（含路由启用的透传）和 Chat→Responses 改道与此模式不兼容，明确拒绝；不要同时启用。
- 既有转换诊断记录 `claude_bedrock_effort_flattened` / `claude_bedrock_display_normalized` 或拒绝原因，不写入原始请求或字段值。展平只能保留**本次**有效 effort，无法保留历史每轮 effort 和原生提示词缓存语义；可能影响缓存命中与行为一致性。需要这些语义时使用原生转发。

此开关没有 UI 控件；运维更新渠道 JSON 时应合并而非覆盖原有配置。该模式不是上游安全能力探测，也不是自动重试或通用 Bedrock 协议转换。

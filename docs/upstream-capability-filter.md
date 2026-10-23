# 上游能力过滤（has）

Cline 的上游路由接受两个限制字段，行为差别很大：

- `only`：允许列表。告诉网关「优先在这些 provider 里选」，是**提示**。
- `has`：能力过滤。要求服务该模型的上游具备指定能力，是**硬条件**。

被钉在 `openai_compatible_private` 私有通道的账号，往往只对 `has` 有反应：`only` 写了也不会被采纳，请求照旧走私有通道；补上 `has` 之后，不满足能力条件的私有通道会被排除，请求回落到真实 provider。

本插件把这两个字段都放在「钉上游」页按模型配置，二者互不替代。经验做法是：`only` 留空或只写一家当兜底，真正驱动路由的是 `has`。

---

## 请求形态

字段位于请求体顶层的 `providerOptions.gateway`，与 `model`、`messages` 平级：

```json
{
  "model": "cline-pass/deepseek-v4.1-flash-1017",
  "messages": [{ "role": "user", "content": "只回复 OK" }],
  "stream": true,
  "providerOptions": {
    "gateway": {
      "only": ["deepseek"],
      "has": ["vision", "implicit-caching"]
    }
  }
}
```

常见能力标识：

| 标识 | 含义 |
| --- | --- |
| `vision` | 上游支持图片输入 |
| `implicit-caching` | 上游支持自动前缀缓存 |

这两项与模型是否真的会用到无关，只是路由筛子。挑上游不提供的组合会直接导致没有可用 provider。

旧的顶层 `provider` 形态（`{"provider": {"only": [...]}}`）只被当作路由提示，不参与能力匹配。实测账号被锁在私有通道时，这种写法反而会把请求钉死在私有通道，所以插件不往那里写 `has`。

---

## 为什么只用 `only` 常常无效

同一把 key、同一个 Pass 模型，直连 `https://api.cline.bot/api/v1/chat/completions` 的对照结果（2026-10）：

| 请求 | 路由结果（`choices[0].delta.provider_metadata.gateway.routing.finalProvider`） | 上游尝试次数 |
| --- | --- | --- |
| `deepseek-v4.1-flash` + `only:[deepseek]` + `has:[vision, implicit-caching]` | `openai-compatible-private` | 1 |
| `deepseek-v4.1-flash-1017` + `only:[deepseek]` + `has:[vision, implicit-caching]` | `deepseek` | 2 |
| `deepseek-v4.1-flash-1017` + 只有 `has` | `deepseek` | 2 |
| `deepseek-v4.1-flash-1017` + `has` + `only:[not-a-real-provider]` | 无可用 provider，SSE 里报 `stream_initialization_failed` | — |
| `deepseek-v4.1-flash-1017` + 顶层 `provider:{only:[deepseek]}` | `openai-compatible-private` | 1 |

读法：

1. **只有 `has` 起作用。** 第二行与第三行结果一致，说明 `only` 不是必要条件；第一行说明换模型 ID 后两个字段一起失效，字段本身没问题。
2. **模型 ID 决定可行性。** `deepseek-v4.1-flash` 只解析到私有通道那条路，`deepseek-v4.1-flash-1017` 才同时具备私有通道与真实 provider 两个候选。要钉上游就用 1017 这类的 ID。
3. **`has` 不是过滤掉候选，而是先试后排除。** 尝试次数 2 说明网关先试了私有通道、判定不满足能力条件，再切到真实 provider。
4. **失败发生在流里。** `only` 指向不存在的 provider 时，HTTP 状态仍是 200，错误写在 SSE 数据里，只判状态码会漏掉。

已有的排查记录见管理页「钉上游」页右上角的「详细证据」。

---

## 两个后果要知道

**计费。** 私有通道的账单记 `creditsUsed: 0`，真实 provider 会产生费用（一次极小的请求约 `0.0000133`）。配置 `has` 之前先确认这些流量本来就该按真实用量计费。

**失败方式。** `has` 与一个过窄的 `only` 叠加时，如果没有可用 provider，请求会直接失败而不是降级。只想要「尽量别走私有通道」的话，`only` 留空、只配 `has` 更稳。

---

## 客户端直接传字段

插件不是唯一的入口：能够完整透传请求体的客户端，自己带 `providerOptions` 也能生效，不需要依赖插件配置。OpenAI Python SDK 用 `extra_body` 传，字段照样落在请求体顶层：

```python
client.chat.completions.create(
    model="cline-pass/deepseek-v4.1-flash-1017",
    messages=[{"role": "user", "content": "只回复 OK"}],
    extra_body={"providerOptions": {"gateway": {"has": ["vision", "implicit-caching"]}}},
)
```

客户端已带 `has` 时，插件在「客户端优先」策略下不会再覆盖它。

---

## 能力标识从哪来

插件目前不猜能力：`has` 由使用者填写，取值不做白名单校验，只做格式归一化（小写、去重、最多 8 个）。写错拼写不会报错，只会让请求没有匹配的上游——所以改完先发一次测试请求，看请求日志的「实际上游」列从 `openai-compatible-private` 变成真实 provider 名称，才算配置成功。「测试模型」按钮会用当前凭据发一次真实请求并显示实际上游。

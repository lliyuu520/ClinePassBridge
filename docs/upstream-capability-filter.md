# 上游能力过滤（has）

想让 `cline-pass/deepseek-v4.1-flash-1017` 请求由 DeepSeek 提供，可以同时配置：

- `only: ["deepseek"]`：提供方允许列表，限定公开候选路由中的 provider。
- `has: ["vision", "implicit-caching"]`：要求候选满足这些能力标签。

两者作用不同。2026-10-23 的单账号实测中，仅 `only` 仍落私有通道，仅 `has` 则落到 `fireworks`；组合使用才得到 `finalProvider=deepseek`。因此，指定 DeepSeek 时应一起设置，能力过滤本身不指定提供方。

## 在插件中设置

在「钉上游」页，确认模型的上游 ID 为 `cline-pass/deepseek-v4.1-flash-1017`，点击「应用 DeepSeek 预设」填入提供方和能力组合。界面会自动保存，看到「已保存」后再探测或测试。两项都保留自由编辑；预设不会自动修改旧模型的上游 ID，也不会自动改写其他模型。

能力过滤没有默认值，升级不会给已有模型自动补上 `has`。详细配置和客户端优先级见[进阶配置](configuration.md#能力过滤has)。

「探测可钉上游」按已保存的能力条件发起小请求，同时移除提供方允许列表、顺序等其他路由选项，读取符合这些能力条件的候选。候选为空只表示上游没有列出候选；最终保存的组合是否命中 DeepSeek，请通过「测试模型」或正式请求的「实际上游」确认。

## 请求形态

`has` 写在请求体顶层的 `providerOptions.gateway` 中：

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

| 能力标签 | 含义 |
| --- | --- |
| `vision` | 网关标记的图片输入能力 |
| `implicit-caching` | 网关标记的自动前缀缓存能力 |

标签用于候选匹配；这次文本请求实验没有验证图片输入或缓存效果。插件不校验上游是否支持某个标签，拼写或组合不匹配可能导致没有可用候选。

插件把最终的 `only/order` 同时写入顶层 `provider` 和 `providerOptions.gateway`，兼容不同路由路径；`has` 只写入 `providerOptions.gateway`。下面 G 组验证了这种双字段形态可以与能力组合共同使用。

## 七组实测：2026-10-23

北京时间 22:16–22:19，使用同一账号、同一网络路径，直接请求 Cline `/api/v1/chat/completions`。全部为同一句短文本，`stream=true`、`stream_options.include_usage=true`、`max_tokens=64`，顺序执行且没有自动重试。表中的模型均带 `cline-pass/` 前缀。

| 组 | 上游模型 | `gateway` 参数 | `finalProvider` | 用时 |
| --- | --- | --- | --- | --- |
| A | `deepseek-v4.1-flash-1017` | 无 | `openai-compatible-private` | 2.290s |
| B | 同 A | 仅 `only=[deepseek]` | `openai-compatible-private` | 7.145s |
| C | 同 A | 仅 `has=[vision,implicit-caching]` | `fireworks` | 2.263s |
| D | 同 A | `only + has` | `deepseek` | 2.189s |
| E | 同 A | 重复 D | `deepseek` | 2.021s |
| F | `deepseek-v4.1-flash`（无后缀） | `only + has` | `openai-compatible-private` | 1.654s |
| G | 同 A | `only + order + has`，顶层 `provider` 同步写 `only/order` | `deepseek` | 1.819s |

七组均 HTTP 200、SSE 含 `[DONE]`、`finish_reason=stop`，没有 SSE JSON 解析错误。G 是按插件请求构造方式直连上游的对照，不等于已部署插件的端到端验证。

这组结果支持在该账号和时段使用 `-1017` 模型的组合参数；无后缀模型当时仍落私有通道。它不代表所有账号、其他模型或后续时段的路由都会相同，也没有验证非流式行为。

## 如何读路由结果

最终提供方看响应的 `provider_metadata.gateway.routing.finalProvider`；本轮流式字段位于 `choices[0].delta` 下，插件在日志中显示为「实际上游」。

- C 组的 `canonicalSlug` 已是 `deepseek/deepseek-v4.1-flash-1017`，但 `finalProvider=fireworks`。模型名中的 DeepSeek 不等于最终提供方是 DeepSeek。
- D/E/G 的规划摘要包含 `Provider set restricted to: deepseek`，最终提供方也都是 `deepseek`。这条公开候选路由会应用 `only` 的提供方限制。
- C/D/E/G 的 `modelAttemptCount=2`，但私有模型条目为 `success=false`、`providerAttemptCount=0`、`providerAttempts=[]`；公开模型只有一次提供方尝试。两个模型条目不等于向两家供应商各发送了一次请求。
- `affinity.outcome` 在成功组间也不同。本功能限制单次请求的路由，不代表永久绑定账号或会话。

元数据支持“能力条件改变了候选路由，提供方列表进一步限制选择”的解释；网关内部过滤与规划的具体顺序仍属于推断。

## 费用和失败

`cost`、`gatewayCost`、`marketCost`、`inferenceCost` 等是上游返回的费用字段。本轮没有核对账户账单或额度扣减，单凭这些字段不足以认定私有通道免费、公开提供方一定另行收费，或两条通道单价相同。

能力条件与提供方允许列表叠加后，候选过窄可能导致请求失败。测试时也要看 SSE 错误和实际提供方，HTTP 200 本身不代表请求已正常完成。

## 客户端直接传字段

客户端能够传递扩展请求体时，也可以直接设置这组参数。经插件调用时先选择「客户端优先」；默认的「插件配置优先」会使用模型配置中的路由条件。

例如 OpenAI Python SDK 的扩展字段写法：

```python
client.chat.completions.create(
    model="deepseek-v4.1-flash-1017",  # 填插件中已配置的客户端模型名称
    messages=[{"role": "user", "content": "只回复 OK"}],
    stream=True,
    extra_body={
        "providerOptions": {
            "gateway": {
                "only": ["deepseek"],
                "has": ["vision", "implicit-caching"],
            }
        }
    },
)
```

「客户端优先」按字段保留客户端值，缺少的字段才用插件配置补齐；客户端显式发送 `has: []` 也会保留。

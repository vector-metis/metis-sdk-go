# Metis Go SDK

Metis 应用后端 SDK。它读取平台注入的统一应用身份，访问 Runtime API，并解析声明的依赖、模型、对象存储和应用设置。

## 安装

```bash
go get github.com/vector-metis/metis-sdk-go@v0.1.0
```

```go
package main

import (
	"context"
	"log"

	metis "github.com/vector-metis/metis-sdk-go"
)

func main() {
	client, err := metis.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	embedding, err := client.Model("embedding.0")
	if err != nil {
		log.Fatal(err)
	}
	rerank, err := client.Model("rerank.0")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("embedding=%s rerank=%s", embedding.Model, rerank.Model)
	dependencies, err := client.ListDependencies(context.Background(), false)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("dependencies: %d", len(dependencies))
}
```

`Model` 支持 `llm.N`、`embedding.N` 和 `rerank.N` 三类 slot，并返回对应的网关地址、模型别名、API key 及类型专属参数。SDK 不创建厂商客户端。

使用 `RuntimeConfig` 可读取当前 service 的模型展示名称、图标、能力、上下文限制、状态和 revision；响应不包含 API key 或供应商凭据：

```go
config, err := client.RuntimeConfig(ctx, false)
if err != nil { log.Fatal(err) }
for slot, model := range config.Config.Models {
	log.Printf("%s: %s (%s)", slot, model.DisplayName, model.BusyLevel)
}
```

Runtime Config 中原始 protobuf 类型为 `int64` 或 `uint64` 的字段（如 `revision`、`installationId`、`contextWindow` 和 `iconFileId`）在 protojson 中按十进制字符串传输，SDK 公共类型也保持为 `string`。这是跨语言协议规则，不只是浏览器兼容处理；Node.js 后端同样不能用 `number` 安全表示全部 64 位整数。SDK 不做递归数字猜测或隐式转换，需要计算时请由应用显式使用 `strconv` 转换。`int32/uint32` 和有限浮点字段仍按数字处理。

模型插槽 API key 始终必填；最终用户归因与插槽授权相互独立。应用后端应按调用来源明确选择一种归因：

```go
model, _ := client.Model("llm.0")
attribution, _ := metis.PlatformIdentityFromRequest(incomingRequest)
request, _ := model.NewRequest(ctx, http.MethodPost, "/v1/chat/completions", body, attribution)

external, _ := metis.WithExternalUser("customer-42")
request, _ = model.NewRequest(ctx, http.MethodPost, "/v1/chat/completions", body, external)

request, _ = model.NewRequest(ctx, http.MethodPost, "/v1/embeddings", body, metis.WithoutUserAttribution())
```

优先从平台保护入口的可信请求头构造平台用户归因。`WithExternalUser` 只适用于应用已经自行认证的外部用户；后台任务无法对应到个人时才使用 `WithoutUserAttribution`。浏览器端不得持有模型插槽 API key。

运行环境必须提供 `METIS_PLATFORM_ENDPOINT`、`METIS_APP_ID` 和 `METIS_APP_TOKEN`。本地测试可以通过 `metis.Config` 显式传入替代值。应用 token 只用于当前应用声明的 Runtime API 和依赖调用，SDK 不实现业务协议客户端，也不自动重试。

依赖结果包含 `RequestedVersion`、`ResolvedVersion`、`PackageSHA256`、`Direct`、`Available` 和
`ResolutionError`，分别表示原始约束、平台锁定版本、MPK 摘要、直接/传递关系、当前可用性和诊断原因。

## 开发

```bash
go test ./...
go vet ./...
```

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。

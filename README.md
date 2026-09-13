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

运行环境必须提供 `METIS_PLATFORM_ENDPOINT`、`METIS_APP_ID` 和 `METIS_APP_TOKEN`。本地测试可以通过 `metis.Config` 显式传入替代值。应用 token 只用于当前应用声明的 Runtime API 和依赖调用，SDK 不实现业务协议客户端，也不自动重试。

## 开发

```bash
go test ./...
go vet ./...
```

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。

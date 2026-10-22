package metis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sharedFixture struct {
	Environment    map[string]string `json:"environment"`
	TrustedHeaders map[string]string `json:"trustedHeaders"`
	Dependencies   json.RawMessage   `json:"dependencies"`
	Endpoint       json.RawMessage   `json:"endpoint"`
}

func readSharedFixture(t *testing.T) sharedFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture sharedFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestClientUsesSharedContractAndSuccessCache(t *testing.T) {
	fixture := readSharedFixture(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer app-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "/endpoints/database"):
			_, _ = writer.Write(fixture.Endpoint)
		case strings.HasSuffix(request.URL.Path, "/dependencies"):
			_, _ = writer.Write(fixture.Dependencies)
		case request.URL.Path == "/api/runtime/v1/config":
			if request.Header.Get("X-Metis-Service") != "web" {
				t.Fatalf("service header = %q", request.Header.Get("X-Metis-Service"))
			}
			_, _ = writer.Write([]byte(`{"schemaVersion":1,"revision":"2","config":{"settings":{"site-name":{"type":"string","required":false,"value":"demo"}},"models":{"llm.0":{"slot":"llm.0","slotIndex":0,"declared":true,"bound":true,"status":"available","interface":"openai.chat.completions","model":"qwen-plus","displayName":"通义千问 Plus","traits":["vision","tools"],"supportsVision":true,"supportsTools":true,"contextWindow":"131072","iconFileId":"42","activity":"active","busyLevel":"unknown"}},"storage":{},"tenant":{},"features":{}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := New(Config{PlatformEndpoint: server.URL, AppID: "caller-a7x2m", AppToken: "app-token", ServiceName: "web", Environment: fixture.Environment, CacheTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		dependencies, listErr := client.ListDependencies(context.Background(), false)
		if listErr != nil || len(dependencies) != 2 || dependencies[0].Alias != "ui" {
			t.Fatalf("dependencies = %#v, err = %v", dependencies, listErr)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one cached request", requests)
	}
	if _, err = client.ListDependencies(context.Background(), true); err != nil || requests != 2 {
		t.Fatalf("refresh error = %v, requests = %d", err, requests)
	}
	endpoint, err := client.ServiceEndpoint(context.Background(), "data", "database", false)
	if err != nil || endpoint.Host != "10.0.0.10" || endpoint.Port != 31001 {
		t.Fatalf("endpoint = %#v, err = %v", endpoint, err)
	}
	config, err := client.RuntimeConfig(context.Background(), false)
	if err != nil || config.Revision != "2" || !config.Config.Models["llm.0"].SupportsVision || config.Config.Models["llm.0"].ContextWindow != "131072" || config.Config.Models["llm.0"].IconFileID != "42" {
		t.Fatalf("runtime config = %#v, err = %v", config, err)
	}
}

func TestRuntimeConfigKeepsProto64BitFieldsAsStrings(t *testing.T) {
	var model RuntimeModel
	if err := json.Unmarshal([]byte(`{"contextWindow":"9007199254740993","iconFileId":"640409671307362374"}`), &model); err != nil {
		t.Fatal(err)
	}
	if model.ContextWindow != "9007199254740993" || model.IconFileID != "640409671307362374" {
		t.Fatalf("model = %#v", model)
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"contextWindow":"9007199254740993"`) || !strings.Contains(string(encoded), `"iconFileId":"640409671307362374"`) {
		t.Fatalf("encoded = %s", encoded)
	}
}

func TestClientDoesNotCacheFailures(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte(`{"reason":"UPSTREAM_FAILURE","message":"temporary"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"dependencies":[]}`))
	}))
	defer server.Close()
	client, _ := New(Config{PlatformEndpoint: server.URL, AppID: "caller-a7x2m", AppToken: "token"})
	if _, err := client.ListDependencies(context.Background(), false); err == nil {
		t.Fatal("first request error = nil")
	}
	if _, err := client.ListDependencies(context.Background(), false); err != nil || requests != 2 {
		t.Fatalf("second request error = %v, requests = %d", err, requests)
	}
}

func TestContextModelsStorageAndWebRequest(t *testing.T) {
	fixture := readSharedFixture(t)
	headers := make(http.Header)
	for name, value := range fixture.TrustedHeaders {
		headers.Set(name, value)
	}
	trusted := ContextFromHeaders(headers)
	if trusted.TenantID != "42" || trusted.UserID != "84" || trusted.ActorType != "service_on_behalf_of_user" {
		t.Fatalf("context = %#v", trusted)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"appId":"web-dependency-a7x2m","alias":"ui","required":true,"appType":"RUNTIME_APPLICATION_TYPE_WEB","available":true,"webBasePath":"/apps/web-dependency-a7x2m"}`))
	}))
	defer server.Close()
	client, err := New(Config{PlatformEndpoint: server.URL, AppID: "caller-a7x2m", AppToken: "app-token", Environment: fixture.Environment})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.NewWebRequest(context.Background(), "ui", http.MethodPost, "/api/items?one=1", nil)
	if err != nil || request.URL.String() != server.URL+"/apps/web-dependency-a7x2m/api/items?one=1" || request.Header.Get("Authorization") != "Bearer app-token" {
		t.Fatalf("request = %#v, err = %v", request, err)
	}
	for _, path := range []string{"../admin", "%2e%2e/admin", `..\admin`} {
		if _, pathErr := client.WebURL(context.Background(), "ui", path); !errors.Is(pathErr, ErrInvalidConfig) {
			t.Fatalf("WebURL(%q) error = %v, want invalid config", path, pathErr)
		}
	}
	model, err := client.Model("llm.0")
	if err != nil || model.Model != "example-chat" || model.MaxOutputTokens != 8192 ||
		!model.SupportsVision || model.SupportsThinking || !model.SupportsTools || model.ContextWindow != 32768 {
		t.Fatalf("model = %#v, err = %v", model, err)
	}
	if tryM, ok := client.TryModel("llm.0"); !ok || tryM.Model != "example-chat" {
		t.Fatalf("TryModel(llm.0) = %#v, %v", tryM, ok)
	}
	if _, ok := client.TryModel("llm.1"); ok {
		t.Fatalf("TryModel(llm.1) expected false")
	}
	if _, ok := client.TryModel("invalid"); ok {
		t.Fatalf("TryModel(invalid) expected false")
	}
	llmList, err := client.Models("llm")
	if err != nil || len(llmList) != 1 || llmList[0].Model != "example-chat" {
		t.Fatalf("Models(llm) = %#v, err = %v", llmList, err)
	}
	if _, err := client.Models("unknown"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Models(unknown) err = %v, want ErrInvalidConfig", err)
	}
	embModel, err := client.Model("embedding.0")
	if err != nil || embModel.Model != "example-embedding" || embModel.Dimensions != 1024 || !embModel.Normalized {
		t.Fatalf("Model(embedding.0) = %#v, err = %v", embModel, err)
	}
	rerankModel, err := client.Model("rerank.0")
	if err != nil || rerankModel.Model != "example-rerank" {
		t.Fatalf("Model(rerank.0) = %#v, err = %v", rerankModel, err)
	}
	storage, err := client.ObjectStorage()
	if err != nil || storage.Bucket != "caller-a7x2m" || len(storage.SharedBuckets) != 1 {
		t.Fatalf("storage = %#v, err = %v", storage, err)
	}
}

// TestModelRequestAttribution 验证插槽密钥始终发送，三类归因头保持互斥。
func TestModelRequestAttribution(t *testing.T) {
	config := ModelConfig{Endpoint: "http://metis.internal/model-gateway", APIKey: "slot-key"}
	platform, err := WithPlatformIdentity(42, 84)
	if err != nil {
		t.Fatal(err)
	}
	request, err := config.NewRequest(t.Context(), http.MethodPost, "/v1/chat/completions", nil, platform)
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") != "Bearer slot-key" ||
		request.Header.Get("X-Platform-Tenant-Id") != "42" || request.Header.Get("X-Platform-User-Id") != "84" {
		t.Fatalf("platform request headers = %#v", request.Header)
	}

	external, err := WithExternalUser(" customer-1 ")
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Platform-Tenant-Id", "forged")
	if err := external.Apply(request); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("X-Platform-Tenant-Id") != "" || request.Header.Get("X-Platform-User-Id") != "" ||
		request.Header.Get("X-Platform-External-User-Id") != "customer-1" {
		t.Fatalf("external request headers = %#v", request.Header)
	}

	if err := WithoutUserAttribution().Apply(request); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("X-Platform-External-User-Id") != "" {
		t.Fatalf("other request headers = %#v", request.Header)
	}
}

func TestModelAttributionRejectsInvalidInput(t *testing.T) {
	if _, err := WithPlatformIdentity(0, 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("WithPlatformIdentity() error = %v", err)
	}
	for _, externalID := range []string{" ", strings.Repeat("a", 129), "bad\nvalue"} {
		if _, err := WithExternalUser(externalID); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("WithExternalUser(%q) error = %v", externalID, err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := PlatformIdentityFromRequest(request); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("PlatformIdentityFromRequest() error = %v", err)
	}
	request.Header.Set("X-Platform-Tenant-Id", "42")
	request.Header.Set("X-Platform-User-Id", "84")
	if attribution, err := PlatformIdentityFromRequest(request); err != nil || attribution.kind != "platform" {
		t.Fatalf("PlatformIdentityFromRequest() = %#v, %v", attribution, err)
	}
	config := ModelConfig{Endpoint: "http://metis.internal/model-gateway", APIKey: "slot-key"}
	if _, err := config.NewRequest(t.Context(), http.MethodPost, "/v1/embeddings", nil, ModelAttribution{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewRequest() error = %v", err)
	}
}

func TestNewRequiresIdentityUnlessExplicitlyConfigured(t *testing.T) {
	client, err := New(Config{Environment: map[string]string{}})
	if err == nil || client != nil {
		t.Fatalf("New() = %#v, %v", client, err)
	}
}

// TestExplicitEmptyEnvironmentDoesNotReadProcessCapabilities 验证本地显式环境不会混入宿主进程配置。
func TestExplicitEmptyEnvironmentDoesNotReadProcessCapabilities(t *testing.T) {
	t.Setenv("METIS_LLM_0_ENDPOINT", "http://unexpected.example")
	t.Setenv("METIS_LLM_0_MODEL", "unexpected")
	t.Setenv("METIS_LLM_0_API_KEY", "unexpected")
	client, err := New(Config{
		PlatformEndpoint: "http://localhost:80", AppID: "local-app", AppToken: "token",
		Environment: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Model("llm.0"); !errors.Is(err, ErrMissingConfig) {
		t.Fatalf("Model() error = %v, want missing config", err)
	}
}

// TestClientNormalizesInvalidRuntimeResponse 验证非 JSON 平台响应仍使用稳定错误 reason。
func TestClientNormalizesInvalidRuntimeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte("gateway failed"))
	}))
	defer server.Close()
	client, err := New(Config{PlatformEndpoint: server.URL, AppID: "local-app", AppToken: "token", Environment: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListDependencies(t.Context(), false)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != "UPSTREAM_FAILURE" {
		t.Fatalf("ListDependencies() error = %#v", err)
	}
}

// TestClientRejectsInvalidCapabilityConfiguration 验证模型下标与共享桶数组采用跨 SDK 一致规则。
func TestClientRejectsInvalidCapabilityConfiguration(t *testing.T) {
	client, err := New(Config{
		PlatformEndpoint: "http://localhost:80", AppID: "local-app", AppToken: "token",
		Environment: map[string]string{
			"METIS_S3_ENDPOINT": "http://storage.example.invalid", "METIS_S3_ACCESS_KEY": "key",
			"METIS_S3_SECRET_KEY": "secret", "METIS_S3_BUCKET": "bucket", "METIS_S3_SHARED_BUCKETS": "null",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Model("llm.-1"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Model() error = %v, want invalid config", err)
	}
	if _, err = client.ObjectStorage(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ObjectStorage() error = %v, want invalid config", err)
	}
}

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
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := New(Config{PlatformEndpoint: server.URL, AppID: "caller-a7x2m", AppToken: "app-token", Environment: fixture.Environment, CacheTTL: time.Minute})
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
	if err != nil || model.Model != "example-chat" || model.Values["MAX_OUTPUT_TOKENS"] != "8192" {
		t.Fatalf("model = %#v, err = %v", model, err)
	}
	storage, err := client.ObjectStorage()
	if err != nil || storage.Bucket != "caller-a7x2m" || len(storage.SharedBuckets) != 1 {
		t.Fatalf("storage = %#v, err = %v", storage, err)
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

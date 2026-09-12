package metis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrMissingConfig 表示运行所需环境变量或显式配置缺失。
	ErrMissingConfig = errors.New("metis: missing config")
	// ErrInvalidConfig 表示配置值存在但格式无效。
	ErrInvalidConfig = errors.New("metis: invalid config")
)

// Config 是本地开发时可显式覆盖的应用身份配置。
type Config struct {
	PlatformEndpoint string
	AppID            string
	AppToken         string
	HTTPClient       *http.Client
	CacheTTL         time.Duration
	Environment      map[string]string
}

// Client 调用平台 Runtime API，并只缓存成功的发现结果。
type Client struct {
	endpoint *url.URL
	appID    string
	token    string
	http     *http.Client
	ttl      time.Duration
	env      map[string]string
	mu       sync.Mutex
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	expires time.Time
	value   []byte
}

// Dependency 描述一个 manifest 直接依赖及其当前可用性。
type Dependency struct {
	AppID       string `json:"appId"`
	Alias       string `json:"alias"`
	Required    bool   `json:"required"`
	AppType     string `json:"appType"`
	Available   bool   `json:"available"`
	WebBasePath string `json:"webBasePath"`
}

// ServiceEndpoint 是 Service 依赖经 Master 代理后的稳定地址。
type ServiceEndpoint struct {
	AppID        string `json:"appId"`
	EndpointName string `json:"endpointName"`
	Protocol     string `json:"protocol"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Available    bool   `json:"available"`
}

// RequestContext 是平台入口覆盖写入的可信调用上下文。
type RequestContext struct {
	TenantID    string
	UserID      string
	Role        string
	SourceAppID string
	ActorType   string
}

// ModelConfig 是平台为一个模型 slot 注入的稳定网关配置。
type ModelConfig struct {
	Endpoint string
	Model    string
	APIKey   string
	Values   map[string]string
}

// ObjectStorageConfig 是平台为应用签发的 S3 兼容存储配置。
type ObjectStorageConfig struct {
	Endpoint      string
	Region        string
	AccessKey     string
	SecretKey     string
	Bucket        string
	SharedBuckets []string
}

// ApplicationConfig contains non-secret identity facts injected into every
// service. The application token remains private to Client and Runtime calls.
type ApplicationConfig struct {
	ID               string
	Name             string
	Version          string
	PlatformEndpoint string
}

// APIError 保存平台稳定错误 reason 和 HTTP 状态码。
type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (err *APIError) Error() string {
	return fmt.Sprintf("metis: runtime request failed: status=%d reason=%s message=%s", err.Status, err.Reason, err.Message)
}

// FromEnv 从进程环境创建客户端；三项应用身份均为必需配置。
func FromEnv() (*Client, error) { return New(Config{}) }

// New 创建客户端；非空显式值覆盖环境，供本地开发和测试使用。
func New(configuration Config) (*Client, error) {
	environment := configuration.Environment
	if environment == nil {
		environment = make(map[string]string)
		for _, item := range os.Environ() {
			name, value, ok := strings.Cut(item, "=")
			if ok {
				environment[name] = value
			}
		}
	}
	get := func(name, explicit string) string {
		if strings.TrimSpace(explicit) != "" {
			return strings.TrimSpace(explicit)
		}
		return strings.TrimSpace(environment[name])
	}
	endpointText, appID, token := get("METIS_PLATFORM_ENDPOINT", configuration.PlatformEndpoint), get("METIS_APP_ID", configuration.AppID), get("METIS_APP_TOKEN", configuration.AppToken)
	if endpointText == "" || appID == "" || token == "" {
		return nil, fmt.Errorf("%w: METIS_PLATFORM_ENDPOINT, METIS_APP_ID and METIS_APP_TOKEN are required", ErrMissingConfig)
	}
	endpoint, err := url.Parse(endpointText)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("%w: METIS_PLATFORM_ENDPOINT must be an absolute HTTP URL", ErrInvalidConfig)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	ttl := configuration.CacheTTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	if ttl < 0 {
		return nil, fmt.Errorf("%w: cache TTL cannot be negative", ErrInvalidConfig)
	}
	httpClient := configuration.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	copiedEnvironment := make(map[string]string)
	for name, value := range environment {
		copiedEnvironment[name] = value
	}
	return &Client{endpoint: endpoint, appID: appID, token: token, http: httpClient, ttl: ttl, env: copiedEnvironment, cache: make(map[string]cacheEntry)}, nil
}

// ContextFromRequest 只读取平台可信头，不读取 Cookie、query 或请求体。
func ContextFromRequest(request *http.Request) RequestContext {
	if request == nil {
		return RequestContext{}
	}
	return ContextFromHeaders(request.Header)
}

// ContextFromHeaders 从大小写不敏感的 HTTP header 集合解析平台可信上下文。
func ContextFromHeaders(headers http.Header) RequestContext {
	return RequestContext{
		TenantID: headers.Get("X-Platform-Tenant-Id"), UserID: headers.Get("X-Platform-User-Id"), Role: headers.Get("X-Platform-Role"),
		SourceAppID: headers.Get("X-Platform-Source-App-Id"), ActorType: headers.Get("X-Platform-Actor-Type"),
	}
}

// ListDependencies 返回 manifest 声明顺序中的全部直接依赖。
func (client *Client) ListDependencies(ctx context.Context, refresh bool) ([]Dependency, error) {
	var response struct {
		Dependencies []Dependency `json:"dependencies"`
	}
	if err := client.get(ctx, client.runtimePath("dependencies"), refresh, &response); err != nil {
		return nil, err
	}
	return response.Dependencies, nil
}

// Dependency 优先按 alias，再按应用 ID 解析直接依赖。
func (client *Client) Dependency(ctx context.Context, selector string, refresh bool) (Dependency, error) {
	var dependency Dependency
	err := client.get(ctx, client.runtimePath("dependencies", selector), refresh, &dependency)
	return dependency, err
}

// WebURL 返回 Web 依赖的绝对同源入口地址。
func (client *Client) WebURL(ctx context.Context, selector, requestPath string) (string, error) {
	dependency, err := client.Dependency(ctx, selector, false)
	if err != nil {
		return "", err
	}
	if !dependency.Available || dependency.WebBasePath == "" {
		return "", &APIError{Status: http.StatusServiceUnavailable, Reason: "DEPENDENCY_UNAVAILABLE", Message: "web dependency is unavailable"}
	}
	return client.absolutePath(dependency.WebBasePath, requestPath)
}

// NewWebRequest 创建携带同一应用 token 的依赖请求；调用方必须显式添加代表用户头。
func (client *Client) NewWebRequest(ctx context.Context, selector, method, requestPath string, body io.Reader) (*http.Request, error) {
	address, err := client.WebURL(ctx, selector, requestPath)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	return request, nil
}

// ServiceEndpoint 返回 Service 依赖的具名 Master 代理地址。
func (client *Client) ServiceEndpoint(ctx context.Context, selector, endpointName string, refresh bool) (ServiceEndpoint, error) {
	var endpoint ServiceEndpoint
	err := client.get(ctx, client.runtimePath("dependencies", selector, "endpoints", endpointName), refresh, &endpoint)
	return endpoint, err
}

// Model 解析一个形如 llm.0 的模型 slot，不创建任何厂商客户端。
func (client *Client) Model(slot string) (ModelConfig, error) {
	parts := strings.Split(slot, ".")
	if len(parts) != 2 || (parts[0] != "llm" && parts[0] != "embedding" && parts[0] != "rerank") || !asciiDigits(parts[1]) {
		return ModelConfig{}, fmt.Errorf("%w: invalid model slot %q", ErrInvalidConfig, slot)
	}
	prefix := "METIS_" + strings.ToUpper(parts[0]) + "_" + parts[1] + "_"
	values := make(map[string]string)
	for name, value := range client.environment() {
		if strings.HasPrefix(name, prefix) {
			values[strings.TrimPrefix(name, prefix)] = value
		}
	}
	configuration := ModelConfig{Endpoint: values["ENDPOINT"], Model: values["MODEL"], APIKey: values["API_KEY"], Values: values}
	if configuration.Endpoint == "" || configuration.Model == "" || configuration.APIKey == "" {
		return ModelConfig{}, fmt.Errorf("%w: model slot %s requires ENDPOINT, MODEL and API_KEY", ErrMissingConfig, slot)
	}
	return configuration, nil
}

// ObjectStorage 解析应用 S3 配置，不连接对象存储或刷新凭据。
func (client *Client) ObjectStorage() (ObjectStorageConfig, error) {
	environment := client.environment()
	configuration := ObjectStorageConfig{Endpoint: environment["METIS_S3_ENDPOINT"], Region: environment["METIS_S3_REGION"], AccessKey: environment["METIS_S3_ACCESS_KEY"], SecretKey: environment["METIS_S3_SECRET_KEY"], Bucket: environment["METIS_S3_BUCKET"], SharedBuckets: []string{}}
	if configuration.Endpoint == "" || configuration.AccessKey == "" || configuration.SecretKey == "" || configuration.Bucket == "" {
		return ObjectStorageConfig{}, fmt.Errorf("%w: object storage requires endpoint, credentials and bucket", ErrMissingConfig)
	}
	if raw := environment["METIS_S3_SHARED_BUCKETS"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &configuration.SharedBuckets); err != nil || configuration.SharedBuckets == nil {
			return ObjectStorageConfig{}, fmt.Errorf("%w: METIS_S3_SHARED_BUCKETS must be a JSON string array", ErrInvalidConfig)
		}
	}
	return configuration, nil
}

// Application returns the current installation identity without making a
// network request.
func (client *Client) Application() ApplicationConfig {
	environment := client.environment()
	return ApplicationConfig{
		ID: client.appID, Name: environment["METIS_APP_NAME"], Version: environment["METIS_APP_VERSION"],
		PlatformEndpoint: client.endpoint.String(),
	}
}

// Setting reads one manifest setting by its original key.
func (client *Client) Setting(key string) (string, error) {
	name := "METIS_SETTING_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.TrimSpace(key)))
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("%w: setting key is required", ErrInvalidConfig)
	}
	value, exists := client.environment()[name]
	if !exists {
		return "", fmt.Errorf("%w: setting %q is not injected", ErrMissingConfig, key)
	}
	return value, nil
}

// EntryPort returns the local loopback port of a Web application.
func (client *Client) EntryPort() (int, error) { return client.port("METIS_ENTRY_PORT") }

// EndpointPort returns the local loopback port of a Service endpoint.
func (client *Client) EndpointPort(name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("%w: endpoint name is required", ErrInvalidConfig)
	}
	return client.port("METIS_ENDPOINT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_PORT")
}

func (client *Client) port(name string) (int, error) {
	value := client.environment()[name]
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%w: %s is not a valid port", ErrMissingConfig, name)
	}
	return port, nil
}

func (client *Client) environment() map[string]string {
	return client.env
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (client *Client) runtimePath(parts ...string) string {
	path := "/api/runtime/v1/apps/" + url.PathEscape(client.appID)
	for _, part := range parts {
		path += "/" + url.PathEscape(strings.TrimSpace(part))
	}
	return path
}

func (client *Client) absolutePath(basePath, requestPath string) (string, error) {
	if strings.Contains(requestPath, "://") || strings.HasPrefix(requestPath, "//") {
		return "", fmt.Errorf("%w: dependency path must be relative", ErrInvalidConfig)
	}
	parsed, err := url.Parse(requestPath)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "", fmt.Errorf("%w: dependency path must be relative", ErrInvalidConfig)
	}
	if strings.Contains(parsed.Path, `\`) {
		return "", fmt.Errorf("%w: dependency path must stay within the application root", ErrInvalidConfig)
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("%w: dependency path must stay within the application root", ErrInvalidConfig)
		}
	}
	copy := *client.endpoint
	copy.Path = strings.TrimRight(client.endpoint.Path, "/") + strings.TrimRight(basePath, "/")
	if parsed.Path != "" {
		copy.Path += "/" + strings.TrimLeft(parsed.Path, "/")
	}
	copy.RawQuery = parsed.RawQuery
	return copy.String(), nil
}

func (client *Client) get(ctx context.Context, path string, refresh bool, target any) error {
	if !refresh && client.ttl > 0 {
		client.mu.Lock()
		entry, found := client.cache[path]
		client.mu.Unlock()
		if found && time.Now().Before(entry.expires) {
			return json.Unmarshal(entry.value, target)
		}
	}
	requestURL := *client.endpoint
	requestURL.Path = strings.TrimRight(client.endpoint.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct{ Reason, Message string }
		_ = json.Unmarshal(data, &payload)
		if payload.Reason == "" {
			payload.Reason = "UPSTREAM_FAILURE"
		}
		if payload.Message == "" {
			payload.Message = "runtime request failed"
		}
		return &APIError{Status: response.StatusCode, Reason: payload.Reason, Message: payload.Message}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return &APIError{Status: http.StatusBadGateway, Reason: "UPSTREAM_FAILURE", Message: "runtime response is invalid"}
	}
	if client.ttl > 0 {
		client.mu.Lock()
		client.cache[path] = cacheEntry{expires: time.Now().Add(client.ttl), value: append([]byte(nil), data...)}
		client.mu.Unlock()
	}
	return nil
}

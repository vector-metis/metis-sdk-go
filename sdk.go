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
	"unicode"
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
	ServiceName      string
}

// Client 调用平台 Runtime API，并只缓存成功的发现结果。
type Client struct {
	endpoint *url.URL
	appID    string
	token    string
	http     *http.Client
	ttl      time.Duration
	env      map[string]string
	service  string
	mu       sync.Mutex
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	expires time.Time
	value   []byte
}

// Dependency 描述一个 manifest 直接依赖及其当前可用性。
type Dependency struct {
	AppID            string `json:"appId"`
	Alias            string `json:"alias"`
	Required         bool   `json:"required"`
	RequestedVersion string `json:"requestedVersion"`
	ResolvedVersion  string `json:"resolvedVersion"`
	PackageSHA256    string `json:"packageSha256"`
	Available        bool   `json:"available"`
	Direct           bool   `json:"direct"`
	ResolutionError  string `json:"resolutionError"`
	AppType          string `json:"appType"`
	WebBasePath      string `json:"webBasePath"`
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
	Endpoint         string
	Model            string
	APIKey           string
	SupportsVision   bool
	SupportsThinking bool
	SupportsTools    bool
	ContextWindow    int
	MaxInputTokens   int
	MaxOutputTokens  int
	Dimensions       int
	Normalized       bool
}

// RuntimeModel 是平台 Runtime API 返回的当前生效模型公共投影。
// 它只包含展示与能力字段，不包含 API Key、供应商凭据或其它秘密。
type RuntimeModel struct {
	Declared           bool     `json:"declared"`
	Bound              bool     `json:"bound"`
	Status             string   `json:"status"`
	Slot               string   `json:"slot"`
	SlotType           string   `json:"slotType"`
	SlotIndex          int      `json:"slotIndex"`
	Interface          string   `json:"interface"`
	Endpoint           string   `json:"endpoint"`
	Model              string   `json:"model"`
	DisplayName        string   `json:"displayName"`
	IconURL            string   `json:"iconUrl"`
	Description        string   `json:"description"`
	Traits             []string `json:"traits"`
	SupportsVision     bool     `json:"supportsVision"`
	SupportsThinking   bool     `json:"supportsThinking"`
	SupportsTools      bool     `json:"supportsTools"`
	ContextWindow      int64    `json:"contextWindow,string"`
	MaxInputTokens     int64    `json:"maxInputTokens,string"`
	MaxOutputTokens    int64    `json:"maxOutputTokens,string"`
	Dimensions         int64    `json:"dimensions,string"`
	Activity           string   `json:"activity"`
	BusyLevel          string   `json:"busyLevel"`
	UpdatedAt          string   `json:"updatedAt"`
	IconFileID         int64    `json:"iconFileId,string"`
	SupportsAudio      bool     `json:"supportsAudio"`
	SupportsImageInput bool     `json:"supportsImageInput"`
	SupportsMultimodal bool     `json:"supportsMultimodal"`
}

// RuntimeSetting 是统一运行配置中的 service 级设置投影。
type RuntimeSetting struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Value    string `json:"value"`
}

// RuntimeConfigSnapshot 是一个 revision 一致的 service 运行配置快照。
type RuntimeConfigSnapshot struct {
	SchemaVersion int64  `json:"schemaVersion"`
	Revision      int64  `json:"revision,string"`
	UpdatedAt     string `json:"updatedAt"`
	Application   struct {
		AppID          string `json:"appId"`
		InstallationID string `json:"installationId"`
		Service        string `json:"service"`
		Version        string `json:"version"`
	} `json:"application"`
	Config struct {
		Settings map[string]RuntimeSetting `json:"settings"`
		Models   map[string]RuntimeModel   `json:"models"`
		Storage  map[string]any            `json:"storage"`
		Tenant   map[string]any            `json:"tenant"`
		Features map[string]any            `json:"features"`
	} `json:"config"`
}

// ModelAttribution 描述一次模型调用的最终用户归因，不参与插槽授权。
// 使用构造函数创建该值，零值不是有效归因。
type ModelAttribution struct {
	kind           string
	tenantID       int64
	userID         int64
	externalUserID string
}

// WithPlatformIdentity 把模型用量归属于平台租户中的平台用户。
func WithPlatformIdentity(tenantID, userID int64) (ModelAttribution, error) {
	if tenantID <= 0 || userID <= 0 {
		return ModelAttribution{}, fmt.Errorf("%w: platform tenant and user ids must be positive", ErrInvalidConfig)
	}
	return ModelAttribution{kind: "platform", tenantID: tenantID, userID: userID}, nil
}

// PlatformIdentityFromRequest 从平台受保护入口覆盖写入的可信头构造归因。
func PlatformIdentityFromRequest(request *http.Request) (ModelAttribution, error) {
	if request == nil {
		return ModelAttribution{}, fmt.Errorf("%w: platform request is required", ErrInvalidConfig)
	}
	context := ContextFromRequest(request)
	tenantID, tenantErr := strconv.ParseInt(strings.TrimSpace(context.TenantID), 10, 64)
	userID, userErr := strconv.ParseInt(strings.TrimSpace(context.UserID), 10, 64)
	if tenantErr != nil || userErr != nil {
		return ModelAttribution{}, fmt.Errorf("%w: request does not contain a valid platform identity", ErrInvalidConfig)
	}
	return WithPlatformIdentity(tenantID, userID)
}

// WithExternalUser 把用量归属于应用已认证的外部用户。
func WithExternalUser(externalUserID string) (ModelAttribution, error) {
	externalUserID = strings.TrimSpace(externalUserID)
	if len(externalUserID) == 0 || len(externalUserID) > 128 || strings.IndexFunc(externalUserID, unicode.IsControl) >= 0 {
		return ModelAttribution{}, fmt.Errorf("%w: external user id must be 1-128 bytes without control characters", ErrInvalidConfig)
	}
	return ModelAttribution{kind: "external", externalUserID: externalUserID}, nil
}

// WithoutUserAttribution 明确把后台任务等调用归入其它用户。
func WithoutUserAttribution() ModelAttribution {
	return ModelAttribution{kind: "other"}
}

// Apply 写入模型网关归因头，并先清理请求中可能存在的冲突值。
func (attribution ModelAttribution) Apply(request *http.Request) error {
	if request == nil {
		return fmt.Errorf("%w: model request is required", ErrInvalidConfig)
	}
	request.Header.Del("X-Platform-Tenant-Id")
	request.Header.Del("X-Platform-User-Id")
	request.Header.Del("X-Platform-External-User-Id")
	switch attribution.kind {
	case "platform":
		request.Header.Set("X-Platform-Tenant-Id", strconv.FormatInt(attribution.tenantID, 10))
		request.Header.Set("X-Platform-User-Id", strconv.FormatInt(attribution.userID, 10))
	case "external":
		request.Header.Set("X-Platform-External-User-Id", attribution.externalUserID)
	case "other":
	default:
		return fmt.Errorf("%w: model attribution is required", ErrInvalidConfig)
	}
	return nil
}

// NewRequest 创建携带 slot API Key 和显式用量归因的模型网关请求。
func (config ModelConfig) NewRequest(ctx context.Context, method, requestPath string, body io.Reader, attribution ModelAttribution) (*http.Request, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(config.Endpoint), "/")
	requestPath = strings.TrimSpace(requestPath)
	if endpoint == "" || config.APIKey == "" || requestPath == "" || !strings.HasPrefix(requestPath, "/") {
		return nil, fmt.Errorf("%w: model endpoint, API key and absolute request path are required", ErrInvalidConfig)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint+requestPath, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+config.APIKey)
	if err := attribution.Apply(request); err != nil {
		return nil, err
	}
	return request, nil
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
	serviceName := get("METIS_SERVICE_NAME", configuration.ServiceName)
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
	return &Client{endpoint: endpoint, appID: appID, token: token, http: httpClient, ttl: ttl, env: copiedEnvironment, service: serviceName, cache: make(map[string]cacheEntry)}, nil
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

// RuntimeConfig 读取当前 service 的统一运行配置；配置服务身份缺失时明确失败。
func (client *Client) RuntimeConfig(ctx context.Context, refresh bool) (RuntimeConfigSnapshot, error) {
	if client.service == "" {
		return RuntimeConfigSnapshot{}, fmt.Errorf("%w: METIS_SERVICE_NAME is required", ErrMissingConfig)
	}
	var response RuntimeConfigSnapshot
	requestPath := "/api/runtime/v1/config"
	if err := client.getWithHeaders(ctx, requestPath, refresh, &response, map[string]string{"X-Metis-Service": client.service}); err != nil {
		return RuntimeConfigSnapshot{}, err
	}
	return response, nil
}

func (client *Client) parseModelSlot(slot string) (ModelConfig, bool, error) {
	parts := strings.Split(slot, ".")
	if len(parts) != 2 || (parts[0] != "llm" && parts[0] != "embedding" && parts[0] != "rerank") || !asciiDigits(parts[1]) {
		return ModelConfig{}, false, fmt.Errorf("%w: invalid model slot %q", ErrInvalidConfig, slot)
	}
	prefix := "METIS_" + strings.ToUpper(parts[0]) + "_" + parts[1] + "_"
	env := client.environment()
	endpoint := env[prefix+"ENDPOINT"]
	modelName := env[prefix+"MODEL"]
	apiKey := env[prefix+"API_KEY"]
	if endpoint == "" || modelName == "" || apiKey == "" {
		return ModelConfig{}, false, nil
	}
	toInt := func(key string) int {
		v, _ := strconv.Atoi(env[prefix+key])
		return v
	}
	toBool := func(key string) bool {
		return strings.EqualFold(env[prefix+key], "true")
	}
	config := ModelConfig{
		Endpoint:         endpoint,
		Model:            modelName,
		APIKey:           apiKey,
		SupportsVision:   toBool("SUPPORTS_VISION"),
		SupportsThinking: toBool("SUPPORTS_THINKING"),
		SupportsTools:    toBool("SUPPORTS_TOOLS"),
		ContextWindow:    toInt("CONTEXT_WINDOW"),
		MaxInputTokens:   toInt("MAX_INPUT_TOKENS"),
		MaxOutputTokens:  toInt("MAX_OUTPUT_TOKENS"),
		Dimensions:       toInt("DIMENSIONS"),
		Normalized:       toBool("NORMALIZED"),
	}
	return config, true, nil
}

// Model 解析一个形如 llm.0 的模型 slot，不创建任何厂商客户端。未绑定或缺失配置时返回 ErrMissingConfig。
func (client *Client) Model(slot string) (ModelConfig, error) {
	config, bound, err := client.parseModelSlot(slot)
	if err != nil {
		return ModelConfig{}, err
	}
	if !bound {
		return ModelConfig{}, fmt.Errorf("%w: model slot %s requires ENDPOINT, MODEL and API_KEY", ErrMissingConfig, slot)
	}
	return config, nil
}

// TryModel 尝试读取指定模型插槽配置；若插槽未绑定或插槽名称格式非法，则返回 (ModelConfig{}, false)。
func (client *Client) TryModel(slot string) (ModelConfig, bool) {
	config, bound, err := client.parseModelSlot(slot)
	if err != nil || !bound {
		return ModelConfig{}, false
	}
	return config, true
}

// Models 批量枚举指定模型类型（llm, embedding, rerank）当前有效绑定的所有模型配置，按插槽序号（0~9）升序排列。
func (client *Client) Models(modelType string) ([]ModelConfig, error) {
	if modelType != "llm" && modelType != "embedding" && modelType != "rerank" {
		return nil, fmt.Errorf("%w: invalid model type %q", ErrInvalidConfig, modelType)
	}
	var configs []ModelConfig
	for i := 0; i < 10; i++ {
		slot := fmt.Sprintf("%s.%d", modelType, i)
		if config, ok := client.TryModel(slot); ok {
			configs = append(configs, config)
		}
	}
	if configs == nil {
		configs = []ModelConfig{}
	}
	return configs, nil
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
	return client.getWithHeaders(ctx, path, refresh, target, nil)
}

func (client *Client) getWithHeaders(ctx context.Context, path string, refresh bool, target any, headers map[string]string) error {
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
	for name, value := range headers {
		request.Header.Set(name, value)
	}
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

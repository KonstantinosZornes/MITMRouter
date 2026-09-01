package acctmap

// sync_sources.kind 的持久化取值。
const (
	SourceKindCLIProxyAPI = "cpa"
	SourceKindSub2API     = "sub2api"
)

// acct_map.source_type 的内置来源类型全名。
const (
	SourceTypeCLIProxyAPI = "CLIProxyAPI"
	SourceTypeSub2API     = "Sub2API"
)

// 账号映射使用的规范平台名。
const (
	PlatformOpenAI    = "openai"
	PlatformAnthropic = "anthropic"
	PlatformGemini    = "gemini"
	PlatformGrok      = "grok"
	PlatformKimi      = "kimi"
	PlatformDeepSeek  = "deepseek"
	PlatformGLM       = "glm"
	PlatformQwen      = "qwen"
	PlatformIFlow     = "iflow"
	PlatformOllama    = "ollama"
)

const (
	// SourcePush 是推送/手动通道的来源实例标识。
	SourcePush = "api"
	// SourceInstancePrefix 是拉取源来源实例标识的前缀。
	SourceInstancePrefix = "src:"
)

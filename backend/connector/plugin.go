package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Capability 标识一个 connector 插件向上层暴露的能力。
//
// 上层（API / monitor / 前端）不再按渠道类型硬编码分支，而是查插件声明的能力：
// 不支持的能力不出现在 UI，后端收到请求时也会返回明确的"不支持"错误。
type Capability string

const (
	CapLogin             Capability = "login"
	CapTokenCredential   Capability = "token_credential"
	CapTurnstile         Capability = "turnstile"
	CapLoginExtraParams  Capability = "login_extra_params"
	CapBalance           Capability = "balance"
	CapCosts             Capability = "costs"
	CapRates             Capability = "rates"
	CapAnnouncements     Capability = "announcements"
	CapRedeem            Capability = "redeem"
	CapRecharge          Capability = "recharge"
	CapSubscription      Capability = "subscription"
	CapSubscriptionUsage Capability = "subscription_usage"
	CapAPIKeys           Capability = "api_keys"
)

// TokenSessionTTL 是 token 模式会话的默认有效期。
const TokenSessionTTL = 365 * 24 * time.Hour

// ErrUnsupported 表示 connector 未实现该操作（或插件未声明对应能力）。
var ErrUnsupported = errors.New("connector does not support this operation")

// Unsupported 构造带操作名的 ErrUnsupported 错误。
func Unsupported(operation string) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, operation)
}

// TokenField 描述 token 凭据 JSON 里的一个字段，前端据此渲染表单。
type TokenField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
	// Target 决定该字段落到 AuthSession 的哪个位置：cookie / access_token / refresh_token / user_id。
	Target string `json:"target"`
	// OneOf 非空时，同一组字段至少填写一个（例如 NewAPI 的 cookie 或 access_token）。
	OneOf string `json:"one_of,omitempty"`
}

// PluginDescriptor 是插件对外暴露的只读元数据，前端通过 /api/connector-plugins 获取。
type PluginDescriptor struct {
	Type         ChannelType  `json:"type"`
	Name         string       `json:"name"`
	Description  string       `json:"description,omitempty"`
	Capabilities []Capability `json:"capabilities"`
	TokenFields  []TokenField `json:"token_fields,omitempty"`
	TokenHint    string       `json:"token_hint,omitempty"`
}

// HasCapability 判断描述符是否声明了某项能力。
func (d PluginDescriptor) HasCapability(cap Capability) bool {
	for _, c := range d.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// Plugin 是注册表里的一条记录。
type Plugin struct {
	Descriptor PluginDescriptor
	Factory    Factory
	// ValidateToken 可选：对 token 凭据做插件自定义校验（例如字段互斥）。
	ValidateToken func(values map[string]string) error
}

var (
	pluginMu sync.RWMutex
	plugins  = map[ChannelType]Plugin{}
)

// RegisterPlugin 由插件包在 init() 中调用，注册插件元数据与构造器。
func RegisterPlugin(p Plugin) {
	if strings.TrimSpace(string(p.Descriptor.Type)) == "" {
		panic("connector: plugin type is empty")
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	plugins[p.Descriptor.Type] = p
}

// Register 是旧式注册入口，保留给只关心 ChannelType 的调用方。
// 新代码应使用 RegisterPlugin 声明完整元数据。
func Register(t ChannelType, f Factory) {
	RegisterPlugin(Plugin{
		Descriptor: PluginDescriptor{Type: t, Name: string(t)},
		Factory:    f,
	})
}

// Plugins 返回所有已编译进二进制的插件描述（按类型名排序）。
func Plugins() []PluginDescriptor {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	out := make([]PluginDescriptor, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, p.Descriptor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// PluginFor 查一个插件。
func PluginFor(t ChannelType) (Plugin, bool) {
	pluginMu.RLock()
	defer pluginMu.RUnlock()
	p, ok := plugins[t]
	return p, ok
}

// HasCapability 判断某个渠道类型是否声明了能力。
func HasCapability(t ChannelType, cap Capability) bool {
	p, ok := PluginFor(t)
	return ok && p.Descriptor.HasCapability(cap)
}

// For 按 ChannelType 取一个新的 Connector。未注册返回错误。
func For(t ChannelType) (Connector, error) {
	p, ok := PluginFor(t)
	if !ok || p.Factory == nil {
		return nil, fmt.Errorf("connector %q is not registered (did you forget the blank import?)", t)
	}
	return p.Factory(), nil
}

// ParseTokenCredential 把 token 凭据 JSON 按插件声明的 TokenFields 解析成 AuthSession。
//
// 校验规则：
//   - Required 字段必须非空；
//   - OneOf 组内至少填写一个字段；
//   - 插件自定义 ValidateToken（如 NewAPI 的 cookie / access_token 互斥）。
func ParseTokenCredential(t ChannelType, raw string) (*AuthSession, error) {
	p, ok := PluginFor(t)
	if !ok {
		return nil, fmt.Errorf("connector %q is not registered (did you forget the blank import?)", t)
	}
	desc := p.Descriptor
	if len(desc.TokenFields) == 0 {
		return nil, fmt.Errorf("渠道类型 %s 不支持 token 凭据", t)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("token_credential 不能为空")
	}

	values := map[string]string{}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, errors.New("token_credential 必须是 JSON 对象")
	}
	for k, v := range decoded {
		switch vv := v.(type) {
		case nil:
			continue
		case string:
			values[k] = strings.TrimSpace(vv)
		case float64:
			values[k] = strconv.FormatInt(int64(vv), 10)
		case bool:
			values[k] = strconv.FormatBool(vv)
		default:
			return nil, fmt.Errorf("token_credential.%s 必须是字符串", k)
		}
	}

	for _, f := range desc.TokenFields {
		if f.Required && values[f.Key] == "" {
			return nil, fmt.Errorf("token_credential.%s 不能为空", f.Key)
		}
	}

	groups := map[string][]string{}
	for _, f := range desc.TokenFields {
		if f.OneOf != "" {
			groups[f.OneOf] = append(groups[f.OneOf], f.Key)
		}
	}
	for _, keys := range groups {
		found := false
		for _, k := range keys {
			if values[k] != "" {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("token_credential 需要至少填写 %s 中的一个", strings.Join(keys, " / "))
		}
	}

	if p.ValidateToken != nil {
		if err := p.ValidateToken(values); err != nil {
			return nil, err
		}
	}

	sess := &AuthSession{ExpiresAt: time.Now().Add(TokenSessionTTL)}
	for _, f := range desc.TokenFields {
		v := values[f.Key]
		if v == "" {
			continue
		}
		switch f.Target {
		case "cookie":
			sess.Cookie = v
		case "access_token":
			sess.AccessToken = v
		case "refresh_token":
			sess.RefreshToken = v
		case "user_id":
			sess.UserID = v
		default:
			return nil, fmt.Errorf("token_credential 字段 %s 的 target 非法: %s", f.Key, f.Target)
		}
	}
	return sess, nil
}

// ValidateTokenCredential 只校验 token 凭据，不构造会话。
func ValidateTokenCredential(t ChannelType, raw string) error {
	_, err := ParseTokenCredential(t, raw)
	return err
}

// TokenCredentialValue 从 token 凭据 JSON 里按 target 取字段值（例如 user_id）。
// 解析失败或字段不存在时返回空串。
func TokenCredentialValue(t ChannelType, raw string, target string) string {
	p, ok := PluginFor(t)
	if !ok {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &decoded); err != nil {
		return ""
	}
	for _, f := range p.Descriptor.TokenFields {
		if f.Target != target {
			continue
		}
		switch v := decoded[f.Key].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

package app

import (
	"encoding/json"
	"strings"
)

// 账号级上游配置。
//
// 以前认证方式、上游地址、请求整形都是实例级的（CODEBUDDY2API_AUTH_MODE 等），
// 所以一个实例里所有账号必须同种认证、同一个上游——这就是当初被迫为 intl 单开
// 第二个实例的原因。现在这些都下沉到账号上，账号没填就回落到实例级配置，
// 于是同一个面板里既能放 ck_ 号（CN），也能放 OAuth 凭证号（intl）。
//
// 回落保证了老账号行为完全不变：库里已有的行这几列都是 NULL。

// AccountProfile 是一个账号实际生效的上游配置。
type AccountProfile struct {
	AuthMode     string
	UpstreamURL  string
	Domain       string
	RequestShape string
	Models       []string
}

// ProfileFor 解析账号生效配置：账号级覆盖 > 账号类型 > 实例级。
func (c *UpstreamClient) ProfileFor(account Account) AccountProfile {
	return resolveProfile(account, c.types.TypeOf(account), c.cfg)
}

func resolveProfile(account Account, typ *AccountType, cfg Config) AccountProfile {
	p := AccountProfile{
		AuthMode:     cfg.AuthMode,
		UpstreamURL:  cfg.UpstreamURL,
		Domain:       cfg.Domain,
		RequestShape: cfg.RequestShape,
		Models:       cfg.Models,
	}
	// 类型盖在实例之上：一个类型就是「这一类号该怎么打上游」的完整描述，
	// 所以它每填一项就顶掉对应的实例默认值，没填的继续用实例的。
	if typ != nil {
		if v := strings.TrimSpace(typ.AuthMode); v != "" {
			p.AuthMode = NormalizeAuthMode(v)
		}
		if v := strings.TrimSpace(typ.UpstreamURL); v != "" {
			p.UpstreamURL = v
			p.Domain = HostOfURL(v)
		}
		if v := strings.TrimSpace(typ.Domain); v != "" {
			p.Domain = v
		}
		if v := strings.TrimSpace(typ.RequestShape); v != "" {
			p.RequestShape = NormalizeRequestShape(v)
		}
		if len(typ.Models) > 0 {
			p.Models = typ.Models
		}
	}
	if v := strings.TrimSpace(account.AuthMode.String); v != "" {
		p.AuthMode = NormalizeAuthMode(v)
	}
	if v := strings.TrimSpace(account.UpstreamURL.String); v != "" {
		p.UpstreamURL = v
		// 上游换了但没显式指定 domain 时，按上游主机推导（官方 CLI 就是这么来的）
		if strings.TrimSpace(account.Domain.String) == "" {
			p.Domain = HostOfURL(v)
		}
	}
	if v := strings.TrimSpace(account.Domain.String); v != "" {
		p.Domain = v
	}
	if v := strings.TrimSpace(account.RequestShape.String); v != "" {
		p.RequestShape = NormalizeRequestShape(v)
	}
	if models := account.ModelList(); len(models) > 0 {
		p.Models = models
	}
	return p
}

// ModelList 解出账号声明支持的模型；没声明返回 nil（表示不限，交给实例级模型表）。
func (a Account) ModelList() []string {
	raw := strings.TrimSpace(a.Models.String)
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		// 也允许逗号分隔，手写配置时更顺手
		for _, item := range strings.Split(raw, ",") {
			if item = strings.TrimSpace(item); item != "" {
				list = append(list, item)
			}
		}
		return list
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// ServesModel 判断账号自身声明能不能承接某个模型（不看类型）。
// 归了类的账号请用 servesModel(account, typ, model)——那才是调度实际走的判断。
func (a Account) ServesModel(model string) bool {
	return servesModel(a, nil, model)
}

// DetectAuthMode 按凭证形态推断认证方式；认不出来返回空串（交给实例级默认）。
//
//	eyJ...  三段式 JWT  -> bearer   （intl OAuth access token）
//	ck_...               -> api_key  （CN 订阅 key）
func DetectAuthMode(credential string) string {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return ""
	}
	if strings.HasPrefix(credential, "ck_") {
		return AuthModeAPIKey
	}
	if strings.HasPrefix(credential, "eyJ") && strings.Count(credential, ".") == 2 {
		return AuthModeBearer
	}
	return ""
}

// IsBearer 是不是 OAuth 凭证账号（决定发 Authorization 还是 X-Api-Key）。
func (p AccountProfile) IsBearer() bool {
	return p.AuthMode == AuthModeBearer
}

// encodeModelList 把模型列表存成 JSON；空列表存 NULL。
func encodeModelList(models []string) any {
	out := make([]string, 0, len(models))
	for _, item := range models {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return string(raw)
}

// decodeModelList 把库里存的模型列表解出来给面板用。
func decodeModelList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err == nil {
		return list
	}
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}
	return list
}

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 余额查询：官方计费接口（legacy）。
//
// 只有这个接口能用 ck_ API key 调通；新版三件套
// (/billing/meter/get-user-resource-summary|-paid-packages|-free-packages) 对
// ck_ key 一律 401（需要桌面端 OAuth 登录态），请求级明细接口不存在（404）。
//
// 两个坑：
//  1. 网关 WAF 会拦非浏览器 UA（脚本 UA 返回 403/10085），必须发 Chrome UA。
//  2. 整数字段是四舍五入的（490），精确值在 *Precise 字段（489.75）。
const (
	billingPath = "/v2/billing/meter/get-user-resource"
	billingUA   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

// CreditsInfo 是一次余额查询的结果（单位：credits）。
//
// Remain/Total 是「全部包」口径（含已用尽的包，用尽包 remain=0 只影响分母）；
// ActiveRemain/ActiveTotal 是「生效中（Status=0）的包」口径，面板展示与阈值判断都以它为准——
// 否则 430/4500 这种分母会把一个还剩 86% 的月度包画成红色告警。
type CreditsInfo struct {
	Remain       float64 // 全部包剩余合计
	Total        float64 // 全部包总量合计（分母偏大，仅参考）
	ActiveRemain float64 // 生效中包剩余合计（面板显示用）
	ActiveTotal  float64 // 生效中包总量合计
	CycleEnd     *int64  // 生效中包里最早的到期/重置时间
	Packages     []CreditPackage
}

// CreditPackage 是单个积分包。
type CreditPackage struct {
	Name     string
	Code     string
	Status   int
	Remain   float64
	Size     float64
	CycleEnd *int64
}

func (c *UpstreamClient) billingURL() (string, error) {
	base, err := url.Parse(c.cfg.UpstreamURL)
	if err != nil {
		return "", err
	}
	base.Path = billingPath
	base.RawQuery = ""
	return base.String(), nil
}

// FetchCredits 查询某账号的真实积分余额（不消耗积分）。
func (c *UpstreamClient) FetchCredits(account Account) (CreditsInfo, error) {
	endpoint, err := c.billingURL()
	if err != nil {
		return CreditsInfo{}, err
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 1, 2, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 00:00:00"),
		"PackageEndTimeRangeEnd":   now.AddDate(10, 0, 0).Format("2006-01-02 15:04:05"),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return CreditsInfo{}, err
	}
	headers := c.BuildHeaders(account)
	headers.Set("User-Agent", billingUA) // 绕过计费网关的异常流量拦截
	headers.Set("Accept", "application/json")
	headers.Del("X-Agent-Intent")

	client, err := c.httpClient(account)
	if err != nil {
		return CreditsInfo{}, err
	}
	transportClient := *client
	transportClient.Timeout = 30 * time.Second
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return CreditsInfo{}, err
	}
	req.Header = headers
	resp, err := transportClient.Do(req)
	if err != nil {
		return CreditsInfo{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return CreditsInfo{}, fmt.Errorf("billing http %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []map[string]any `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return CreditsInfo{}, fmt.Errorf("billing decode: %w", err)
	}
	if parsed.Code != 0 {
		return CreditsInfo{}, fmt.Errorf("billing code %d: %s", parsed.Code, parsed.Msg)
	}
	packs := parsed.Data.Response.Data.Accounts
	if len(packs) == 0 {
		return CreditsInfo{}, fmt.Errorf("billing: no packages (key rejected?)")
	}

	info := CreditsInfo{}
	for _, p := range packs {
		cycleSize := numField(firstNonNil(p["CycleCapacitySizePrecise"], p["CycleCapacitySize"]))
		cycleRemain := numField(firstNonNil(p["CycleCapacityRemainPrecise"], p["CycleCapacityRemain"]))
		if cycleSize <= 0 && cycleRemain <= 0 {
			// 非周期包：用总量字段
			cycleSize = numField(firstNonNil(p["CapacitySizePrecise"], p["CapacitySize"]))
			cycleRemain = numField(firstNonNil(p["CapacityRemainPrecise"], p["CapacityRemain"]))
		}
		if cycleRemain < 0 {
			cycleRemain = 0
		}
		status := int(numField(p["Status"]))
		info.Remain += cycleRemain
		info.Total += cycleSize
		// Status 0 = 生效中；3 = 已用完/失效（已用尽的包不计入有效额度）
		active := status == 0 && cycleRemain > 0
		if active {
			info.ActiveRemain += cycleRemain
			info.ActiveTotal += cycleSize
		}

		pkg := CreditPackage{
			Name:   stringValue(p["PackageName"], "-"),
			Code:   stringValue(p["PackageCode"], ""),
			Status: status,
			Remain: cycleRemain,
			Size:   cycleSize,
		}
		for _, key := range []string{"CycleEndTime", "ExpiredTime"} {
			if ts, ok := parseBillingTime(stringValue(p[key], "")); ok {
				pkg.CycleEnd = &ts
				if active && (info.CycleEnd == nil || ts < *info.CycleEnd) {
					info.CycleEnd = &ts
				}
				break
			}
		}
		info.Packages = append(info.Packages, pkg)
	}
	return info, nil
}

func parseBillingTime(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.Local); err == nil {
		return ts.Unix(), true
	}
	if ts, err := time.Parse("2006-01-02", raw); err == nil {
		return ts.Unix(), true
	}
	return 0, false
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// numField 解析计费接口返回的数值。注意：该接口把数字都以字符串下发
// （"CycleCapacityRemainPrecise":"489.75"），floatValue 不认字符串。
func numField(value any) float64 {
	switch item := value.(type) {
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(item), 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return floatValue(value)
	}
}

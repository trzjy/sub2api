package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DevinModelCatalogPath 是 GetCliModelConfigs 的 Connect RPC 端点路径。
const DevinModelCatalogPath = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"

// DevinModelConfig 描述一个 Devin 可选模型。
type DevinModelConfig struct {
	Selector        string `json:"selector"`                    // field 22
	Label           string `json:"label"`                       // field 1
	Provider        string `json:"provider,omitempty"`          // field 10（枚举，见 devinProviderName）
	Alias           string `json:"alias,omitempty"`             // field 23 内 field 23
	SupportsImages  bool   `json:"supports_images"`             // field 5
	ContextWindow   int    `json:"context_window,omitempty"`    // field 18
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"` // field 23 内 field 13
}

// decodeDevinModelMeta 解码模型 field 23 嵌套消息中的 alias（field 23）与
// maxOutputTokens（field 13）。
func decodeDevinModelMeta(data []byte) (alias string, maxOutputTokens int, err error) {
	d := NewProtoDecoder(data)
	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 13:
			if !d.requireWire(wire, 0) {
				continue
			}
			maxOutputTokens = int(d.ReadUint64())
		case 23:
			if !d.requireWire(wire, 2) {
				continue
			}
			alias = d.ReadString()
		default:
			d.Skip(wire)
		}
	}
	if err := d.Err(); err != nil {
		return "", 0, err
	}
	return alias, maxOutputTokens, nil
}

// devinProviderName 把 provider 枚举（field 10）映射为稳定字符串名；未知值返回空串。
func devinProviderName(v uint64) string {
	switch v {
	case 1:
		return "cognition"
	case 2:
		return "openai"
	case 3:
		return "anthropic"
	case 4:
		return "google"
	case 7:
		return "moonshot"
	case 9:
		return "zhipu"
	default:
		return ""
	}
}

// decodeDevinModelConfig 解码单个模型配置消息，附带跳过判定所需的 disabled 标志。
func decodeDevinModelConfig(data []byte) (cfg DevinModelConfig, disabled bool, err error) {
	d := NewProtoDecoder(data)
	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 1: // label
			if !d.requireWire(wire, 2) {
				continue
			}
			cfg.Label = d.ReadString()
		case 4: // disabled
			if !d.requireWire(wire, 0) {
				continue
			}
			disabled = d.ReadUint64() == 1
		case 5: // supportsImages
			if !d.requireWire(wire, 0) {
				continue
			}
			cfg.SupportsImages = d.ReadBool()
		case 10: // provider 枚举
			if !d.requireWire(wire, 0) {
				continue
			}
			cfg.Provider = devinProviderName(d.ReadUint64())
		case 18: // contextWindow
			if !d.requireWire(wire, 0) {
				continue
			}
			cfg.ContextWindow = int(d.ReadUint64())
		case 22: // selector
			if !d.requireWire(wire, 2) {
				continue
			}
			cfg.Selector = d.ReadString()
		case 23: // 嵌套 meta：alias / maxOutputTokens
			if !d.requireWire(wire, 2) {
				continue
			}
			alias, maxOut, decErr := decodeDevinModelMeta(d.ReadBytes())
			if decErr != nil {
				return DevinModelConfig{}, false, decErr
			}
			cfg.Alias = alias
			cfg.MaxOutputTokens = maxOut
		default:
			d.Skip(wire)
		}
	}
	if err := d.Err(); err != nil {
		return DevinModelConfig{}, false, err
	}
	return cfg, disabled, nil
}

// decodeGetCliModelConfigsResponse 解码响应：重复 field 1（message）。
// disabled==1 或 selector 为空的模型被跳过。
func decodeGetCliModelConfigsResponse(data []byte) ([]DevinModelConfig, error) {
	d := NewProtoDecoder(data)
	out := make([]DevinModelConfig, 0)
	for !d.Done() {
		field, wire := d.ReadTag()
		if field != 1 {
			d.Skip(wire)
			continue
		}
		if !d.requireWire(wire, 2) {
			continue
		}
		cfg, disabled, err := decodeDevinModelConfig(d.ReadBytes())
		if err != nil {
			return nil, err
		}
		if disabled || cfg.Selector == "" {
			continue
		}
		out = append(out, cfg)
	}
	if err := d.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// devinCatalogEndpoint 构造端点 URL；baseURL 为空时回落到 DevinAPIURL。
func devinCatalogEndpoint(baseURL string) string {
	base := baseURL
	if base == "" {
		base = DevinAPIURL
	}
	return strings.TrimRight(base, "/") + DevinModelCatalogPath
}

// FetchDevinModelCatalog 通过 GetCliModelConfigs 拉取 Devin 可选模型目录。
// 使用与 GetUserJwt 相同的 Metadata 认证体，不附加 Authorization 头。
func FetchDevinModelCatalog(ctx context.Context, token, baseURL string) ([]DevinModelConfig, error) {
	meta := &Metadata{
		IDEName:          DevinClientName,
		IDEVersion:       DevinIDEVersion,
		ExtensionName:    DevinClientName,
		ExtensionVersion: DevinExtensionVersion,
		APIKey:           NormalizeDevinToken(token),
		Locale:           "en",
	}
	reqBody := encodeGetUserJwtRequest(meta)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, devinCatalogEndpoint(baseURL), bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("devin model catalog create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")

	client := &http.Client{Timeout: DevinDefaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin model catalog http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("devin model catalog read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		n := len(respBody)
		if n > 200 {
			n = 200
		}
		return nil, fmt.Errorf("devin model catalog %d: %s", resp.StatusCode, string(respBody[:n]))
	}
	return decodeGetCliModelConfigsResponse(respBody)
}

package service

import (
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// ErrDevinRemoteImageUnsupported 表示 devin 只支持 base64 data URI 图片；
// 远程 URL 图片在归一为 Chat 格式时会被静默丢弃，必须失败关闭。
var ErrDevinRemoteImageUnsupported = errors.New("devin: 仅支持 base64 图片，远程 URL 图片不可用")

// validateDevinAnthropicImages 在 Anthropic body 归一为 Chat 格式之前校验图片形态。
// apicompat 转换层对 system 图片（parseAnthropicSystemContentParts 只收 text 块）与
// 非 base64 图片（anthropicImageToDataURI 对空 Data 返回空串后被跳过）都会静默丢弃，
// 因此在入口失败关闭，绝不静默丢失用户图片。
func validateDevinAnthropicImages(req *apicompat.AnthropicRequest) error {
	if req == nil {
		return nil
	}
	if err := validateDevinSystemImages(req.System); err != nil {
		return err
	}
	for i := range req.Messages {
		if err := validateDevinMessageImages(req.Messages[i].Content); err != nil {
			return err
		}
	}
	return nil
}

// validateDevinSystemImages 校验 system 块：system 只拼成纯文本 SystemPrompt，
// 无图片承载位，含图片一律拒绝（复用既有 sentinel）。
func validateDevinSystemImages(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var blocks []apicompat.AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil // 字符串形态 system 不含图片
	}
	for _, b := range blocks {
		if b.Type == "image" {
			return ErrDevinSystemImageUnsupported
		}
	}
	return nil
}

// validateDevinMessageImages 校验消息内图片：仅接受 base64 source；
// tool_result 内嵌的图片块同样校验。
func validateDevinMessageImages(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var blocks []apicompat.AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil // 字符串形态 content 不含图片
	}
	for _, b := range blocks {
		switch b.Type {
		case "image":
			if b.Source == nil || b.Source.Type != "base64" {
				return ErrDevinRemoteImageUnsupported
			}
		case "tool_result":
			var inner []apicompat.AnthropicContentBlock
			if len(b.Content) == 0 || json.Unmarshal(b.Content, &inner) != nil {
				continue
			}
			for _, ib := range inner {
				if ib.Type == "image" && (ib.Source == nil || ib.Source.Type != "base64") {
					return ErrDevinRemoteImageUnsupported
				}
			}
		}
	}
	return nil
}

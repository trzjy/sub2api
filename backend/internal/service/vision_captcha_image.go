package service

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/big"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/inconsolata"
	"golang.org/x/image/math/fixed"
)

// captchaDigitCount 随机验证码位数（方案 §3.5 实证口径：6 位）。
const captchaDigitCount = 6

// randomCaptchaCode 生成 captchaDigitCount 位随机数字验证码（0-9，无前导零限制）。
// 使用 crypto/rand 保证不可预测性；生成失败返回错误。
func randomCaptchaCode() (string, error) {
	const digits = "0123456789"
	var sb strings.Builder
	for i := 0; i < captchaDigitCount; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(digits))))
		if err != nil {
			return "", err
		}
		sb.WriteByte(digits[n.Int64()])
	}
	return sb.String(), nil
}

// renderCaptchaPNGDataURL 把验证码数字渲染为白底黑字 PNG 并返回 data URL。
// 用 inconsolata.Regular8x16 等宽字体保证数字清晰可读（实测区分度足够）。
func renderCaptchaPNGDataURL(code string) (string, error) {
	// 尺寸：6 位数字 + 边距，8x16 字体每字 10 像素宽、16 像素高。
	const (
		cellW    = 18
		cellH    = 26
		paddingX = 12
		paddingY = 14
	)
	width := paddingX*2 + len(code)*cellW
	height := paddingY*2 + cellH

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)

	face := inconsolata.Regular8x16
	drawer := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.Black),
		Face: face,
	}
	for i, r := range code {
		drawer.Dot = fixed.Point26_6{
			X: fixed.I(paddingX + i*cellW),
			Y: fixed.I(paddingY + cellH),
		}
		drawer.DrawString(string(r))
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

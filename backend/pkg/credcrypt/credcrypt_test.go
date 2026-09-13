package credcrypt

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func mustKey(t *testing.T) ([]byte, string) {
	t.Helper()
	raw := make([]byte, KeyLen)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return raw, base64.StdEncoding.EncodeToString(raw)
}

func mustCipher(t *testing.T) *Cipher {
	t.Helper()
	primary, _ := mustKey(t)
	c, err := NewCipher(primary, nil)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	c := mustCipher(t)
	cases := []string{
		"simple-token",
		"",
		"包含中文与 emoji 🔑 的凭证",
		strings.Repeat("long-secret-", 512),
	}
	for _, plain := range cases {
		enc, err := c.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plain, err)
		}
		if !IsEncrypted(enc) {
			t.Fatalf("Encrypt(%q) result %q missing prefix %q", plain, enc, Prefix)
		}
		if strings.Contains(enc, plain) && plain != "" {
			t.Fatalf("Encrypt(%q) result leaks plaintext", plain)
		}
		dec, err := c.Decrypt(enc)
		if err != nil {
			t.Fatalf("Decrypt roundtrip(%q): %v", plain, err)
		}
		if dec != plain {
			t.Fatalf("roundtrip mismatch: got %q want %q", dec, plain)
		}
	}
}

func TestEncryptProducesPrefixedUniqueCiphertext(t *testing.T) {
	c := mustCipher(t)
	a, err := c.Encrypt("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encrypt("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, Prefix) || !strings.HasPrefix(b, Prefix) {
		t.Fatalf("ciphertexts must carry %q prefix: %q %q", Prefix, a, b)
	}
	if a == b {
		t.Fatal("same plaintext encrypted twice must produce different ciphertexts (random nonce)")
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	c := mustCipher(t)
	got, err := c.Decrypt("plain-value-without-prefix")
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain-value-without-prefix" {
		t.Fatalf("got %q", got)
	}
}

func TestKeyRotationOldKeyDecrypts(t *testing.T) {
	oldRaw, _ := mustKey(t)
	oldCipher, err := NewCipher(oldRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := oldCipher.Encrypt("legacy-secret")
	if err != nil {
		t.Fatal(err)
	}

	newRaw, _ := mustKey(t)
	rotated, err := NewCipher(newRaw, oldRaw)
	if err != nil {
		t.Fatal(err)
	}

	// 轮换后：旧密文可用 OLD 密钥解密
	dec, err := rotated.Decrypt(legacy)
	if err != nil {
		t.Fatalf("decrypt legacy ciphertext with old key: %v", err)
	}
	if dec != "legacy-secret" {
		t.Fatalf("got %q", dec)
	}

	// 轮换后：新密文使用新主密钥
	fresh, err := rotated.Encrypt("fresh-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldCipher.Decrypt(fresh); err == nil {
		t.Fatal("old-only cipher must not decrypt ciphertext written by new primary key")
	}
	dec, err = rotated.Decrypt(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if dec != "fresh-secret" {
		t.Fatalf("got %q", dec)
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	c := mustCipher(t)
	enc, err := c.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.TrimPrefix(enc, Prefix)
	// 翻转 base64 载荷中部的字符（保证落在密文字节区而非仅 padding）
	mid := len(payload) / 2
	replacement := "A"
	if payload[mid] == 'A' {
		replacement = "B"
	}
	tampered := Prefix + payload[:mid] + replacement + payload[mid+1:]
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext must fail GCM authentication")
	}

	// 截断密文同样必须失败
	if _, err := c.Decrypt(enc[:len(enc)-8]); err == nil {
		t.Fatal("truncated ciphertext must fail decryption")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	c1 := mustCipher(t)
	c2 := mustCipher(t)
	enc, err := c1.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Decrypt(enc); err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
}

func TestParseKeyValidation(t *testing.T) {
	if _, err := ParseKey("not-valid-base64!!!"); err == nil {
		t.Fatal("invalid base64 must error")
	}
	if _, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("non-32-byte key must error")
	}
	raw, b64 := mustKey(t)
	got, err := ParseKey(b64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatal("ParseKey roundtrip mismatch")
	}
	// 容忍首尾空白
	if _, err := ParseKey("  " + b64 + "\n"); err != nil {
		t.Fatalf("ParseKey must tolerate surrounding whitespace: %v", err)
	}
}

func TestNewCipherValidation(t *testing.T) {
	if _, err := NewCipher([]byte("short"), nil); err == nil {
		t.Fatal("short primary key must error")
	}
	if _, err := NewCipher(nil, nil); err == nil {
		t.Fatal("nil primary key must error")
	}
	primary, _ := mustKey(t)
	if _, err := NewCipher(primary, []byte("short")); err == nil {
		t.Fatal("short old key must error")
	}
}

func TestConfigureUnsetPassthrough(t *testing.T) {
	t.Cleanup(func() { _, _ = Configure("", "") })
	enabled, err := Configure("", "")
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("Configure with no keys must report disabled")
	}
	if Enabled() {
		t.Fatal("Enabled must be false without keys")
	}
	// 未配置密钥：Encrypt 明文 passthrough
	enc, err := Encrypt("plain-secret")
	if err != nil {
		t.Fatal(err)
	}
	if enc != "plain-secret" {
		t.Fatalf("passthrough Encrypt must return plaintext, got %q", enc)
	}
	// 未配置密钥：Decrypt 明文 passthrough
	dec, err := Decrypt("plain-secret")
	if err != nil {
		t.Fatal(err)
	}
	if dec != "plain-secret" {
		t.Fatalf("passthrough Decrypt must return plaintext, got %q", dec)
	}
	// 未配置密钥：遇带前缀密文必须报错而非误当明文放行
	if _, err := Decrypt(Prefix + "AAAA"); err == nil {
		t.Fatal("Decrypt of prefixed ciphertext without keys must error")
	}
}

func TestConfigureInvalidKeysFailFast(t *testing.T) {
	t.Cleanup(func() { _, _ = Configure("", "") })
	if _, err := Configure("not-base64!!!", ""); err == nil {
		t.Fatal("invalid primary key must error")
	}
	_, validB64 := mustKey(t)
	if _, err := Configure(validB64, "not-base64!!!"); err == nil {
		t.Fatal("invalid old key must error")
	}
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := Configure(short, ""); err == nil {
		t.Fatal("short primary key must error")
	}
	// 只配 OLD 不配主密钥：自相矛盾，必须报错
	if _, err := Configure("", validB64); err == nil {
		t.Fatal("old key without primary key must error")
	}
}

func TestPackageLevelEncryptDecryptWithConfiguredKeys(t *testing.T) {
	t.Cleanup(func() { _, _ = Configure("", "") })
	_, primaryB64 := mustKey(t)
	enabled, err := Configure(primaryB64, "")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || !Enabled() {
		t.Fatal("Configure with primary key must enable package-level cipher")
	}
	enc, err := Encrypt("pkg-level-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(enc) {
		t.Fatalf("package-level Encrypt must produce prefixed ciphertext, got %q", enc)
	}
	dec, err := Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec != "pkg-level-secret" {
		t.Fatalf("got %q", dec)
	}
}

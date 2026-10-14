//go:build unit

package wasm

import (
	"encoding/json"
	"os"
	"testing"
)

// loadVector 读取权威向量（/root/cursor-evidence/06-zk01-vector.json）。
func loadVector(t *testing.T) (metadata, mail, timestamp, expMail, expAT, expRT string, expRole int, expExpire interface{}, expMode int) {
	t.Helper()
	raw, err := os.ReadFile("/root/cursor-evidence/06-zk01-vector.json")
	if err != nil {
		t.Skipf("vector file unavailable: %v", err)
	}
	var v struct {
		Input struct {
			Metadata  string `json:"metadata"`
			Mail      string `json:"mail"`
			Timestamp string `json:"timestamp"`
		} `json:"input"`
		Expected struct {
			Mail            string      `json:"mail"`
			AccessToken     string      `json:"access_token"`
			RefreshToken    string      `json:"refresh_token"`
			Role            int         `json:"role"`
			AccountExpireIn interface{} `json:"account_expire_in"`
			Mode            int         `json:"mode"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	return v.Input.Metadata, v.Input.Mail, v.Input.Timestamp,
		v.Expected.Mail, v.Expected.AccessToken, v.Expected.RefreshToken,
		v.Expected.Role, v.Expected.AccountExpireIn, v.Expected.Mode
}

func TestDecryptZk01Vector(t *testing.T) {
	metadata, mail, ts, expMail, expAT, expRT, expRole, expExpire, expMode := loadVector(t)
	out, err := DecryptZk01(metadata, mail, ts)
	if err != nil {
		t.Fatalf("DecryptZk01: %v", err)
	}
	var got struct {
		Mail            string      `json:"mail"`
		AccessToken     string      `json:"access_token"`
		RefreshToken    string      `json:"refresh_token"`
		Role            int         `json:"role"`
		AccountExpireIn interface{} `json:"account_expire_in"`
		Mode            int         `json:"mode"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got.Mail != expMail {
		t.Errorf("mail mismatch\ngot=%q\nexp=%q", got.Mail, expMail)
	}
	if got.AccessToken != expAT {
		t.Errorf("access_token mismatch\ngot=%q\nexp=%q", got.AccessToken, expAT)
	}
	if got.RefreshToken != expRT {
		t.Errorf("refresh_token mismatch\ngot=%q\nexp=%q", got.RefreshToken, expRT)
	}
	if got.Role != expRole {
		t.Errorf("role mismatch got=%d exp=%d", got.Role, expRole)
	}
	if got.Mode != expMode {
		t.Errorf("mode mismatch got=%d exp=%d", got.Mode, expMode)
	}
	if !equalJSON(got.AccountExpireIn, expExpire) {
		t.Errorf("account_expire_in mismatch got=%v exp=%v", got.AccountExpireIn, expExpire)
	}
}

func equalJSON(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

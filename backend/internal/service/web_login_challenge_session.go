package service

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"
)

const WebLoginChallengeTTL = 3 * time.Minute

var (
	ErrWebLoginChallengeNotFound = errors.New("web login challenge session not found")
	ErrWebLoginChallengeExpired  = errors.New("web login challenge session expired")
	ErrWebLoginChallengeConsumed = errors.New("web login challenge session already consumed")
	ErrWebLoginChallengeMismatch = errors.New("web login challenge session binding mismatch")
	ErrWebLoginChallengeNotReady = errors.New("web login challenge is not complete")
	ErrWebLoginChallengeInFlight = errors.New("web login challenge action already in progress")
	ErrWebLoginChallengeContext  = errors.New("context_gap: verified challenge result unavailable")
)

type WebLoginChallengeCreateInput struct {
	AdminID      int64
	Platform     string
	Phone        string
	Stage        string
	AccountID    *int64
	AccountDraft any
	ProxyID      *int64
}

type WebLoginChallengeSession struct {
	ID              string
	AdminID         int64
	Platform        string
	Phone           string
	AccountID       *int64
	ProxyID         *int64
	Stage           string
	HelperSessionID string
	AccountDraft    any
	CreatedAt       time.Time
	ExpiresAt       time.Time
	Status          string
	Consumed        bool
	Challenge       WebSMSChallenge
	resultReady     bool
	claimStage      string
	claimToken      string
}

type WebLoginChallengeClaim struct {
	Token           string
	Challenge       WebSMSChallenge
	HelperSessionID string
	AccountID       *int64
	AccountDraft    any
	ProxyID         *int64
}

// WebLoginChallengeSessionStore is an in-memory, one-time challenge session store.
// It deliberately stores only login metadata and challenge values, never long-lived credentials.
type WebLoginChallengeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*WebLoginChallengeSession
	now      func() time.Time
}

func NewWebLoginChallengeSessionStore() *WebLoginChallengeSessionStore {
	return &WebLoginChallengeSessionStore{sessions: make(map[string]*WebLoginChallengeSession), now: time.Now}
}

func (s *WebLoginChallengeSessionStore) Create(in WebLoginChallengeCreateInput) (*WebLoginChallengeSession, error) {
	platform := strings.ToLower(strings.TrimSpace(in.Platform))
	if platform != PlatformZhipu && platform != PlatformKimi {
		return nil, errors.New("platform must be one of: zhipu, kimi")
	}
	phone := strings.TrimSpace(in.Phone)
	if phone == "" {
		return nil, errors.New("phone is required")
	}
	id, err := opaqueChallengeID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	stage := strings.ToLower(strings.TrimSpace(in.Stage))
	if stage == "" {
		stage = "send_code"
	}
	if stage != "send_code" && stage != "login" {
		return nil, errors.New("stage must be one of: send_code, login")
	}
	sess := &WebLoginChallengeSession{ID: id, AdminID: in.AdminID, Platform: platform, Phone: phone, Stage: stage, AccountID: cloneOptionalInt64(in.AccountID), AccountDraft: in.AccountDraft, ProxyID: cloneOptionalInt64(in.ProxyID), CreatedAt: now, ExpiresAt: now.Add(WebLoginChallengeTTL), Status: "pending"}
	s.mu.Lock()
	// 惰性清扫：创建时顺带删除全部已过期 session（无后台定时器；访问频率与创建频率同阶，足以防积累）。
	for k, v := range s.sessions {
		if !now.Before(v.ExpiresAt) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = sess
	s.mu.Unlock()
	return cloneChallengeSession(sess), nil
}

// Status returns session metadata after validating only administrator ownership.
func (s *WebLoginChallengeSessionStore) Status(id string, adminID int64) (*WebLoginChallengeSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookupLocked(id, adminID, "", "", true)
	if err != nil {
		return nil, err
	}
	return cloneChallengeSession(sess), nil
}

// SetHelperSessionID binds the local helper session to this opaque login session.
func (s *WebLoginChallengeSessionStore) SetStatus(id string, adminID int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookupLocked(id, adminID, "", "", true)
	if err != nil {
		return err
	}
	if sess.Consumed {
		return ErrWebLoginChallengeConsumed
	}
	sess.Status = strings.TrimSpace(status)
	return nil
}

func (s *WebLoginChallengeSessionStore) SetHelperSessionID(id string, adminID int64, helperID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookupLocked(id, adminID, "", "", true)
	if err != nil {
		return err
	}
	if sess.Consumed {
		return ErrWebLoginChallengeConsumed
	}
	sess.HelperSessionID = strings.TrimSpace(helperID)
	return nil
}

func (s *WebLoginChallengeSessionStore) BeginConsume(id string, adminID int64, platform, phone string) (*WebLoginChallengeClaim, error) {
	return s.begin(id, adminID, platform, phone, "result")
}

func (s *WebLoginChallengeSessionStore) FinishConsume(id, claimToken string, success bool) error {
	return s.finish(id, claimToken, "result", success)
}

// SetConsumeResult atomically records a verified helper result for the active consume claim.
// The claim token prevents a stale caller from replacing an already verified result.
func (s *WebLoginChallengeSessionStore) SetConsumeResult(id, claimToken string, challenge WebSMSChallenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(id)]
	if !ok {
		return ErrWebLoginChallengeNotFound
	}
	if sess.claimStage != "result" || sess.claimToken == "" || sess.claimToken != claimToken {
		return ErrWebLoginChallengeMismatch
	}
	if sess.Consumed {
		return ErrWebLoginChallengeConsumed
	}
	// md5 可选（2026-09-22 chatglm.cn 线上取证：官方滑块 onSuccess 仅回调
	// {rid, pass}，md5 是落地链接 query 可选参数，正常滑块流不带）；
	// rid 必填失败关闭不变。
	if sess.Platform == PlatformZhipu && (strings.TrimSpace(challenge.ZhipuCaptchaRid) == "" || strings.TrimSpace(challenge.ZhipuPhoneCode) == "") {
		return errors.New("incomplete zhipu challenge result")
	}
	if sess.Platform == PlatformKimi && strings.TrimSpace(challenge.KimiCaptchaValidate) == "" {
		return errors.New("incomplete kimi challenge result")
	}
	sess.Challenge = challenge
	sess.Status = "succeeded"
	sess.resultReady = true
	return nil
}

// CloseConsumeContextGap closes only the current consume claim. It cannot overwrite
// a result that was committed by another caller after this claim was released.
func (s *WebLoginChallengeSessionStore) CloseConsumeContextGap(id, claimToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(id)]
	if !ok {
		return ErrWebLoginChallengeNotFound
	}
	if sess.claimStage != "result" || sess.claimToken == "" || sess.claimToken != claimToken {
		return ErrWebLoginChallengeMismatch
	}
	if sess.resultReady || sess.Status == "succeeded" {
		return ErrWebLoginChallengeMismatch
	}
	sess.Status = "context_gap"
	sess.claimStage = ""
	sess.claimToken = ""
	return nil
}

func (s *WebLoginChallengeSessionStore) BeginSendCode(id string, adminID int64, platform, phone string) (*WebLoginChallengeClaim, error) {
	return s.begin(id, adminID, platform, phone, "send_code")
}

func (s *WebLoginChallengeSessionStore) FinishSendCode(id, claimToken string, success bool) error {
	return s.finish(id, claimToken, "send_code", success)
}

func (s *WebLoginChallengeSessionStore) BeginLogin(id string, adminID int64, platform, phone string) (*WebLoginChallengeClaim, error) {
	return s.begin(id, adminID, platform, phone, "login")
}

func (s *WebLoginChallengeSessionStore) FinishLogin(id, claimToken string, success bool) error {
	return s.finish(id, claimToken, "login", success)
}

func (s *WebLoginChallengeSessionStore) begin(id string, adminID int64, platform, phone, stage string) (*WebLoginChallengeClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.lookupLocked(id, adminID, platform, phone, false)
	if err != nil {
		return nil, err
	}
	if sess.Consumed {
		return nil, ErrWebLoginChallengeConsumed
	}
	if sess.Status == "context_gap" {
		return nil, ErrWebLoginChallengeContext
	}
	if stage == "result" {
		if sess.resultReady {
			return nil, ErrWebLoginChallengeMismatch
		}
		if sess.Status != "pending" && sess.Status != "succeeded" {
			return nil, ErrWebLoginChallengeNotReady
		}
	} else {
		if sess.Status != "succeeded" {
			return nil, ErrWebLoginChallengeNotReady
		}
		if sess.Stage != stage {
			return nil, ErrWebLoginChallengeMismatch
		}
	}
	if sess.claimToken != "" {
		return nil, ErrWebLoginChallengeInFlight
	}
	claimToken, err := opaqueChallengeID()
	if err != nil {
		return nil, err
	}
	sess.claimStage = stage
	sess.claimToken = claimToken
	return &WebLoginChallengeClaim{
		Token:           claimToken,
		Challenge:       sess.Challenge,
		HelperSessionID: sess.HelperSessionID,
		AccountID:       cloneOptionalInt64(sess.AccountID),
		AccountDraft:    sess.AccountDraft,
		ProxyID:         cloneOptionalInt64(sess.ProxyID),
	}, nil
}

func (s *WebLoginChallengeSessionStore) finish(id, claimToken, stage string, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[strings.TrimSpace(id)]
	if !ok {
		return ErrWebLoginChallengeNotFound
	}
	if sess.claimStage != stage || sess.claimToken == "" || sess.claimToken != claimToken {
		return ErrWebLoginChallengeMismatch
	}
	if success {
		switch stage {
		case "send_code":
			sess.Stage = "login"
		case "login":
			sess.Consumed = true
			sess.Challenge = WebSMSChallenge{}
		}
	}
	sess.claimStage = ""
	sess.claimToken = ""
	return nil
}

func (s *WebLoginChallengeSessionStore) lookupLocked(id string, adminID int64, platform, phone string, allowEmptyBinding bool) (*WebLoginChallengeSession, error) {
	sess, ok := s.sessions[strings.TrimSpace(id)]
	if !ok {
		return nil, ErrWebLoginChallengeNotFound
	}
	if !s.now().Before(sess.ExpiresAt) {
		// 惰性清理：命中过期即删除，避免长期运行内存无限积累（TTL 3 分钟）。
		delete(s.sessions, id)
		sess.Status = "expired"
		return nil, ErrWebLoginChallengeExpired
	}
	if sess.AdminID != adminID || (!allowEmptyBinding && (strings.ToLower(strings.TrimSpace(platform)) != sess.Platform || strings.TrimSpace(phone) != sess.Phone)) {
		return nil, ErrWebLoginChallengeMismatch
	}
	return sess, nil
}

func cloneChallengeSession(in *WebLoginChallengeSession) *WebLoginChallengeSession {
	if in == nil {
		return nil
	}
	out := *in
	out.AccountID = cloneOptionalInt64(in.AccountID)
	out.ProxyID = cloneOptionalInt64(in.ProxyID)
	// 挑战结果和内部 claim 永不随公开会话读取返回。
	out.Challenge = WebSMSChallenge{}
	out.claimStage = ""
	out.claimToken = ""
	return &out
}

func cloneOptionalInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func opaqueChallengeID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

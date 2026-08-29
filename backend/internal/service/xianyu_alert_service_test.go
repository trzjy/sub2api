package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// xianyuAlertSettingStub 提供告警收件人设置。
type xianyuAlertSettingStub struct {
	recipients string
}

func (s *xianyuAlertSettingStub) GetValue(_ context.Context, _ string) (string, error) {
	return s.recipients, nil
}
func (s *xianyuAlertSettingStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = s.recipients
	}
	return out, nil
}
func (s *xianyuAlertSettingStub) SetMultiple(_ context.Context, _ map[string]string) error {
	return nil
}

// xianyuAlertNotifyStub 记录发送的告警邮件。
type xianyuAlertNotifyStub struct {
	inputs []NotificationEmailSendInput
}

func (s *xianyuAlertNotifyStub) Send(_ context.Context, input NotificationEmailSendInput) error {
	s.inputs = append(s.inputs, input)
	return nil
}

func TestWorkerHealthTransitionInitialUnhealthyThenNoRepeatThenRecover(t *testing.T) {
	now := time.Now()
	// 首次 unknown -> unhealthy：告警。
	d1 := workerHealthTransition(false, false, XianyuWorkerHealthUnhealthy, "1", now)
	require.True(t, d1.Alerted)
	require.False(t, d1.Recovery)
	require.Equal(t, "unhealthy", d1.Reminder)

	// 已知 unhealthy 且仍 unhealthy：不重复告警。
	d2 := workerHealthTransition(true, true, XianyuWorkerHealthUnhealthy, "1", now)
	require.False(t, d2.Alerted)

	// 已知 unhealthy -> healthy：恢复通知。
	d3 := workerHealthTransition(true, true, XianyuWorkerHealthHealthy, "1", now)
	require.True(t, d3.Alerted)
	require.True(t, d3.Recovery)
	require.Equal(t, "recovered", d3.Reminder)

	// 已恢复后保持 healthy：不再发送。
	d4 := workerHealthTransition(true, false, XianyuWorkerHealthHealthy, "1", now)
	require.False(t, d4.Alerted)
}

func TestWorkerHealthTransitionInitialHealthyNoAlert(t *testing.T) {
	now := time.Now()
	// 首次巡检即 healthy：不发送任何通知。
	d := workerHealthTransition(false, false, XianyuWorkerHealthHealthy, "1", now)
	require.False(t, d.Alerted)
}

func TestWorkerHealthTransitionAlertsUnknownStatus(t *testing.T) {
	now := time.Now()
	d := workerHealthTransition(false, false, XianyuWorkerHealthUnknown, "1", now)
	require.True(t, d.Alerted)
	require.Equal(t, XianyuWorkerHealthUnknown, d.HealthStatus)

	d = workerHealthTransition(true, true, XianyuWorkerHealthUnknown, "1", now)
	require.False(t, d.Alerted)
}

func TestXianyuAlertServiceSendsToVerifiedRecipientsOnly(t *testing.T) {
	notify := &xianyuAlertNotifyStub{}
	svc := NewXianyuAlertService(nil, &xianyuAlertSettingStub{recipients: `[{"email":"admin@example.com","verified":true},{"email":"disabled@example.com","verified":true,"disabled":true},{"email":"unverified@example.com","verified":false}]`}, notify)
	svc.send(context.Background(), NotificationEmailEventXianyuWorkerUnhealthy, "worker:1", "unhealthy", map[string]string{"status": "unhealthy"})
	require.Len(t, notify.inputs, 1)
	require.Equal(t, "admin@example.com", notify.inputs[0].RecipientEmail)
	require.Equal(t, "worker:1", notify.inputs[0].SourceID)
	require.Equal(t, "unhealthy", notify.inputs[0].ReminderKey)
}

package cursorupstream

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCrypto_EncryptDecrypt(t *testing.T) {
	crypto := NewCrypto()
	original := []byte(`{"device_code":"BFEBFBFF000906EA-Default-123456","device_code_md5":"abcdef0123456789"}`)

	encrypted, err := crypto.Encrypt(original)
	require.NoError(t, err)
	require.NotEmpty(t, encrypted)

	decrypted, err := crypto.Decrypt(encrypted)
	require.NoError(t, err)
	require.True(t, bytes.Equal(original, decrypted))
}

func TestDevice_GenerateVirtualDeviceCode(t *testing.T) {
	code, md5Val := GenerateVirtualDeviceCode()
	require.NotEmpty(t, code)
	require.NotEmpty(t, md5Val)
	require.Contains(t, code, "BFEBFBFF000906EA-Default string-")
	require.Len(t, md5Val, 32)
}

func TestDevice_GenerateMachineIDs(t *testing.T) {
	m := GenerateMachineIDs()
	require.NotEmpty(t, m.DevDeviceId)
	require.Len(t, m.MacMachineId, 128) // sha512 hex
	require.Len(t, m.MachineId, 64)     // sha256 hex
	require.True(t, m.SqmId[0] == '{' && m.SqmId[len(m.SqmId)-1] == '}')
}

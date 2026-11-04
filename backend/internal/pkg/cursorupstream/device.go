package cursorupstream

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	Salt = "sky_cc"
)

// GenerateVirtualDeviceCode 生成符合老 Windows 客户端标准的虚拟硬件串与防篡改 MD5
// 对应 Python: machine_device.pyc 中 _serial_windows 逻辑: f"{cpu_id}-{board_result}-{win_uuid}"
func GenerateVirtualDeviceCode() (deviceCode string, deviceCodeMD5 string) {
	// 标准老版 CPU ProcessorId 样本格式
	fakeCPU := "BFEBFBFF000906EA"
	fakeBoard := "Default string"
	fakeUUID := strings.ReplaceAll(uuid.New().String(), "-", "")

	deviceCode = fmt.Sprintf("%s-%s-%s", fakeCPU, fakeBoard, fakeUUID)

	// 计算 MD5("sky_cc" + deviceCode)
	h := md5.New()
	h.Write([]byte(Salt))
	h.Write([]byte(deviceCode))
	deviceCodeMD5 = hex.EncodeToString(h.Sum(nil))

	return deviceCode, deviceCodeMD5
}

// MachineIDs 对应 Cursor 官方出站请求需要携带的设备标识集
// 对应 Python: seamless_switch.pyc 中 generate_machine_ids() 逻辑
type MachineIDs struct {
	DevDeviceId  string `json:"devDeviceId"`
	MacMachineId string `json:"macMachineId"`
	MachineId    string `json:"machineId"`
	SqmId        string `json:"sqmId"`
}

// GenerateMachineIDs 生成一套随机且格式合法的 Cursor 客户端指纹
func GenerateMachineIDs() MachineIDs {
	devID := uuid.New().String()

	// macMachineId: 64 字节随机数的 sha512 hexdigest (128 hex chars)
	r64 := uuid.New().String() + uuid.New().String()
	h512 := sha512.Sum512([]byte(r64))
	macMid := hex.EncodeToString(h512[:])

	// machineId: 32 字节随机数的 sha256 hexdigest (64 hex chars)
	r32 := uuid.New().String()
	h256 := sha256.Sum256([]byte(r32))
	mid := hex.EncodeToString(h256[:])

	// sqmId: {UPPERCASE-UUID}
	sqm := fmt.Sprintf("{%s}", strings.ToUpper(uuid.New().String()))

	return MachineIDs{
		DevDeviceId:  devID,
		MacMachineId: macMid,
		MachineId:    mid,
		SqmId:        sqm,
	}
}

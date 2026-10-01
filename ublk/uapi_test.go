package ublk

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

func TestIoctlEncoding(t *testing.T) {
	t.Parallel()
	// Values from the kernel headers: UBLK_U_CMD_ADD_DEV = _IOWR('u', 0x04, struct ublksrv_ctrl_cmd)
	assert.Equal(t, uint32(0xc0207504), ioctl(cmdAddDev, uint32(unsafe.Sizeof(ctrlCmd{}))))
	assert.Equal(t, uint32(0xc0207506), ioctl(cmdStartDev, uint32(unsafe.Sizeof(ctrlCmd{}))))
	// UBLK_U_IO_FETCH_REQ = _IOWR('u', 0x20, struct ublksrv_io_cmd)
	assert.Equal(t, uint32(0xc0107520), ioctl(cmdFetchReq, uint32(unsafe.Sizeof(ioCmd{}))))
	assert.Equal(t, uint32(0xc0107521), ioctl(cmdCommitAndFetchReq, uint32(unsafe.Sizeof(ioCmd{}))))
}

func TestParamsLayout(t *testing.T) {
	t.Parallel()
	// Offsets within struct ublk_params as the kernel lays it out
	assert.Equal(t, uintptr(8), unsafe.Offsetof(params{}.Basic))
	assert.Equal(t, uintptr(40), unsafe.Offsetof(params{}.Discard))
	assert.Equal(t, uintptr(32), unsafe.Sizeof(paramBasic{}))
	assert.Equal(t, uintptr(20), unsafe.Sizeof(paramDiscard{}))
	assert.Equal(t, uintptr(40), unsafe.Offsetof(ringParams{}.SQOff))
	assert.Equal(t, uintptr(80), unsafe.Offsetof(ringParams{}.CQOff))
	// The CQ offsets block has cqes at 20, where the SQ block has dropped
	assert.Equal(t, uintptr(20), unsafe.Offsetof(cqOffsets{}.CQEs))
	assert.Equal(t, uintptr(24), unsafe.Offsetof(sqOffsets{}.Array))
}

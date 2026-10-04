package ublk

import "unsafe"

// Kernel ABI from include/uapi/linux/ublk_cmd.h and io_uring.h. All structs are laid out
// exactly like their C counterparts (no padding differences on amd64/arm64), so they are
// passed to the kernel by address or copied into SQEs byte for byte.

const (
	controlPath = "/dev/ublk-control"
	charPrefix  = "/dev/ublkc"
	blockPrefix = "/dev/ublkb"
	// sectorSize is the unit of every sector count in the ABI, whatever the logical block size.
	sectorSize = 512

	// Control commands (nr of the ioctl-encoded _IOWR('u', nr, 32))
	cmdGetDevInfo = 0x02
	cmdAddDev     = 0x04
	cmdDelDev     = 0x05
	cmdStartDev   = 0x06
	cmdStopDev    = 0x07
	cmdSetParams  = 0x08
	cmdGetParams  = 0x09
	// Recovery: re-attach a new server to a device whose server died
	cmdStartUserRecovery = 0x10
	cmdEndUserRecovery   = 0x11
	cmdGetFeatures       = 0x13
	// I/O commands (nr of _IOWR('u', nr, 16)) on /dev/ublkcN
	cmdFetchReq          = 0x20
	cmdCommitAndFetchReq = 0x21
	ioctlType            = 'u'
	ioctlDirRW           = 3 // _IOC_READ | _IOC_WRITE
	ioctlDirR            = 2 // _IOC_READ
	ioctlSizeShift       = 16
	ioctlTypeShift       = 8
	ioctlDirShift        = 30

	// Feature flags negotiated at ADD_DEV
	featURingCmdCompInTask  = 1 << 1
	featUserRecovery        = 1 << 3 // the device outlives its server, I/O waits for a new one
	featUserRecoveryReissue = 1 << 4 // requests in flight at the server's death are reissued
	// Request ops in ioDesc.OpFlags bits 0..7
	opRead        = 0
	opWrite       = 1
	opFlush       = 2
	opDiscard     = 3
	opWriteZeroes = 5
	opMask        = 0xff
	// Device attributes in paramBasic.Attrs
	attrReadOnly      = 1 << 0
	attrVolatileCache = 1 << 2
	// params.Types
	paramTypeBasic   = 1 << 0
	paramTypeDiscard = 1 << 1

	queueIDControl   = 0xffff // ctrlCmd.QueueID for device-level commands
	stateLive        = 1      // devInfo.State while the device serves I/O
	stateQuiesced    = 2      // a recoverable device whose server died
	devIDAuto        = ^uint32(0)
	maxQueueDepth    = 4096
	maxQueues        = 4096    // UBLK_MAX_NR_QUEUES
	maxIOSize        = 1 << 25 // UBLK_IO_BUF_BITS: the kernel encodes buffer offsets in 25 bits
	resultAbort      = -19     // -ENODEV: the kernel is tearing the queue down
	resultEIO        = -5
	resultEOpNotSupp = -95
	// descMmapStride is the per-queue offset of the descriptor array in /dev/ublkcN's mmap
	// space: the kernel keys it on the maximum depth, not the configured one.
	descMmapStride = maxQueueDepth * int(unsafe.Sizeof(ioDesc{}))

	// io_uring
	ringSetupSQE128    = 1 << 10
	ringSetupCQE32     = 1 << 11
	ringOffSQRing      = 0
	ringOffCQRing      = 0x8000000
	ringOffSQEs        = 0x10000000
	ringOpURingCmd     = 46
	ringOpRead         = 22
	ringEnterGetEvents = 1 << 0
	ringEnterExtArg    = 1 << 3
	sqeSize            = 128
	sqeCmdOffset       = 48 // start of the 80-byte cmd area in a 128-byte SQE
	cqeSize            = 32
	sysIoUringSetup    = 425
	sysIoUringEnter    = 426
)

// ctrlCmd is struct ublksrv_ctrl_cmd (32 bytes), placed in the SQE cmd area.
type ctrlCmd struct {
	DevID      uint32
	QueueID    uint16
	Len        uint16
	Addr       uint64
	Data       uint64
	DevPathLen uint16
	Pad        uint16
	Reserved   uint32
}

// devInfo is struct ublksrv_ctrl_dev_info (64 bytes), exchanged with ADD_DEV/GET_DEV_INFO.
type devInfo struct {
	NrHwQueues    uint16
	QueueDepth    uint16
	State         uint16
	Pad0          uint16
	MaxIOBufBytes uint32
	DevID         uint32
	UblksrvPID    int32
	Pad1          uint32
	Flags         uint64
	UblksrvFlags  uint64
	OwnerUID      uint32
	OwnerGID      uint32
	Reserved      [2]uint64
}

// ioDesc is struct ublksrv_io_desc (24 bytes), one per tag in the mmap'd descriptor array.
type ioDesc struct {
	OpFlags     uint32
	NrSectors   uint32
	StartSector uint64
	Addr        uint64
}

// ioCmd is struct ublksrv_io_cmd (16 bytes), the FETCH/COMMIT payload in the SQE cmd area.
type ioCmd struct {
	QID    uint16
	Tag    uint16
	Result int32
	Addr   uint64
}

// paramBasic is struct ublk_param_basic.
type paramBasic struct {
	Attrs            uint32
	LogicalBSShift   uint8
	PhysicalBSShift  uint8
	IOOptShift       uint8
	IOMinShift       uint8
	MaxSectors       uint32
	ChunkSectors     uint32
	DevSectors       uint64
	VirtBoundaryMask uint64
}

// paramDiscard is struct ublk_param_discard.
type paramDiscard struct {
	DiscardAlignment      uint32
	DiscardGranularity    uint32
	MaxDiscardSectors     uint32
	MaxWriteZeroesSectors uint32
	MaxDiscardSegments    uint16
	Reserved0             uint16
}

// params is the leading part of struct ublk_params (the devt and zoned blocks are omitted;
// Len tells the kernel how much we send and Types which blocks are meaningful).
type params struct {
	Len     uint32
	Types   uint32
	Basic   paramBasic
	Discard paramDiscard
}

// ringParams is struct io_uring_params.
type ringParams struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SQThreadCPU  uint32
	SQThreadIdle uint32
	Features     uint32
	WQFd         uint32
	Resv         [3]uint32
	SQOff        sqOffsets
	CQOff        cqOffsets
}

// sqOffsets is struct io_sqring_offsets.
type sqOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

// cqOffsets is struct io_cqring_offsets. Note the field order differs from the SQ one.
type cqOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	CQEs        uint32
	Flags       uint32
	Resv1       uint32
	UserAddr    uint64
}

// getEventsArg is struct io_uring_getevents_arg for IORING_ENTER_EXT_ARG.
type getEventsArg struct {
	Sigmask   uint64
	SigmaskSz uint32
	Pad       uint32
	TS        uint64
}

// Compile-time layout checks against the kernel sizes
var (
	_ [32]byte  = [unsafe.Sizeof(ctrlCmd{})]byte{}
	_ [64]byte  = [unsafe.Sizeof(devInfo{})]byte{}
	_ [24]byte  = [unsafe.Sizeof(ioDesc{})]byte{}
	_ [16]byte  = [unsafe.Sizeof(ioCmd{})]byte{}
	_ [120]byte = [unsafe.Sizeof(ringParams{})]byte{}
	_ [24]byte  = [unsafe.Sizeof(getEventsArg{})]byte{}
)

// ctrlIoctl encodes control command nr. The kernel decodes most by number alone but
// matches GET_FEATURES, a newer command, on its exact _IOR encoding.
func ctrlIoctl(nr uint32) uint32 {
	op := ioctl(nr, uint32(unsafe.Sizeof(ctrlCmd{})))
	if nr == cmdGetFeatures {
		op = op&^(ioctlDirRW<<ioctlDirShift) | ioctlDirR<<ioctlDirShift
	}
	return op
}

// ioctl encodes _IOWR('u', nr, size) the way the ublk driver expects its cmd_op.
func ioctl(nr, size uint32) uint32 {
	return ioctlDirRW<<ioctlDirShift | size<<ioctlSizeShift | ioctlType<<ioctlTypeShift | nr
}

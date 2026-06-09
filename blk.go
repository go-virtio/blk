// Package blk is a pure-Go virtio-blk (block device) driver. It drives a
// modern (Virtio 1.0+) PCI virtio-blk device through the transport
// interfaces defined in github.com/go-virtio/common; the same code drives
// a UEFI-backed device, a bare-metal device, or a virtio-mmio device
// depending on which common.Transport implementation the caller supplies.
//
// Scope — like go-virtio/net this package owns device bring-up, the
// single request virtqueue, and the on-the-wire request format (header +
// data + status descriptor chain, Virtio 1.1 §5.2.6), exposing a
// block-level ReadBlocks / WriteBlocks / Flush API. The protocol sector
// size is always 512 bytes (BlockSize) regardless of any logical block
// size the device advertises.
//
//   - Modern transport (VIRTIO_F_VERSION_1 mandatory). Legacy devices
//     are rejected by the common init sequence.
//   - One request virtqueue; requests are descriptor chains built with
//     common.AddChain.
//   - VIRTIO_BLK_F_RO is honoured (read-only devices reject writes).
//     No other feature bit is negotiated (FLUSH works regardless on the
//     devices we target; richer features are out of scope).
//
// References:
//
//   - Virtio 1.1 §5.2   "Block Device" — device-type 2 binding.
//   - Virtio 1.1 §5.2.4 "Device configuration layout" — le64 capacity.
//   - Virtio 1.1 §5.2.6 "Device Operation" — struct virtio_blk_req.
//   - Virtio 1.1 §3.1.1 "Device Initialization".
package blk

import (
	"encoding/binary"

	"github.com/go-virtio/common"
)

// DeviceType is the virtio device-type encoding for virtio-blk
// (Virtio 1.1 §5.2.1). Retained from the package's placeholder era for
// callers enumerating PCI devices that want a stable name.
const DeviceType uint16 = 2

// RequestQueueIdx is the index of the single virtio-blk request queue.
const RequestQueueIdx uint16 = 0

// RequestQueueSize is the desired ring size (clamped + rounded during
// setup). A request consumes 2–3 descriptors, so this bounds the number
// of in-flight requests; the driver issues them one at a time.
const RequestQueueSize uint16 = 16

// BlockSize is the virtio-blk protocol sector size — always 512 bytes
// (Virtio 1.1 §5.2.6); the device's `sector` fields count 512-byte units.
const BlockSize = 512

// BlkReqHeaderSize is the on-the-wire size of struct virtio_blk_req's
// header (Virtio 1.1 §5.2.6.1): le32 type, le32 reserved, le64 sector.
const BlkReqHeaderSize = 16

// Request types (Virtio 1.1 §5.2.6 — virtio_blk_req.type).
const (
	BlkTypeIn    uint32 = 0 // read (device writes data)
	BlkTypeOut   uint32 = 1 // write (device reads data)
	BlkTypeFlush uint32 = 4
)

// Status byte values (Virtio 1.1 §5.2.6 — the device-writable trailer).
const (
	BlkStatusOK     uint8 = 0
	BlkStatusIOErr  uint8 = 1
	BlkStatusUnsupp uint8 = 2
)

// VIRTIO_BLK_F_RO (bit 5) — device is read-only (Virtio 1.1 §5.2.3).
const featureRO uint64 = 1 << 5

// TxPollIterations is the default busy-poll budget for one request.
const TxPollIterations = 200000

// AcceptedFeatures is the feature mask the driver negotiates ON — only
// the non-negotiable VIRTIO_F_VERSION_1. (F_RO is inspected from the
// device's offered set but is not a driver-acked bit.)
const AcceptedFeatures uint64 = common.FeatureVersion1

// AcceptFeatures returns the negotiated mask (requires VERSION_1).
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// VirtioBlk wraps one initialised virtio-blk device.
type VirtioBlk struct {
	// Cfg is the modern-transport handle.
	Cfg *common.ModernConfig

	// Capacity is the device size in 512-byte sectors (Virtio 1.1
	// §5.2.4), read from DeviceCfg at OpenVirtioBlk.
	Capacity uint64

	// ReadOnly reflects VIRTIO_BLK_F_RO in the device's offered features.
	ReadOnly bool

	// NegotiatedFeatures records the driver-feature handshake result.
	NegotiatedFeatures uint64

	transport common.Transport
	rq        *common.Virtqueue
}

// OpenVirtioBlk drives the full bring-up of one virtio-blk device:
//
//  1. Verify the PCI device ID is 0x1042 (modern block).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, record RO, mask to VERSION_1, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Allocate + publish the request queue (queue 0).
//  7. DRIVER_OK status.
//  8. Read capacity (le64 sectors) from DeviceCfg.
func OpenVirtioBlk(t common.Transport) (*VirtioBlk, error) {
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernBlock {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	readOnly := deviceFeats&featureRO != 0
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	rq, err := setupQueue(cfg, t, RequestQueueIdx, RequestQueueSize)
	if err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	// Capacity: le64 at DeviceCfg offset 0 (Virtio 1.1 §5.2.4).
	capacity, err := cfg.DeviceCfgRead64(0)
	if err != nil {
		return nil, err
	}

	return &VirtioBlk{
		Cfg:                cfg,
		Capacity:           capacity,
		ReadOnly:           readOnly,
		NegotiatedFeatures: negotiated,
		transport:          t,
		rq:                 rq,
	}, nil
}

// setupQueue performs the per-queue init (select, size, allocate,
// publish addresses, enable).
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// RequestQueue exposes the request virtqueue handle for diagnostics.
func (b *VirtioBlk) RequestQueue() *common.Virtqueue { return b.rq }

// ReadBlocks reads `count` 512-byte sectors starting at `sector` and
// returns the data. count must be positive.
func (b *VirtioBlk) ReadBlocks(sector uint64, count int) ([]byte, error) {
	if count <= 0 {
		return nil, ErrZeroCount
	}
	out := make([]byte, count*BlockSize)
	if err := b.doRequest(BlkTypeIn, sector, out, true); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteBlocks writes `data` (a whole number of 512-byte sectors)
// starting at `sector`. Returns ErrReadOnly if the device is read-only,
// or ErrUnalignedLength if len(data) is not a positive multiple of
// BlockSize.
func (b *VirtioBlk) WriteBlocks(sector uint64, data []byte) error {
	if b.ReadOnly {
		return ErrReadOnly
	}
	if len(data) == 0 || len(data)%BlockSize != 0 {
		return ErrUnalignedLength
	}
	return b.doRequest(BlkTypeOut, sector, data, false)
}

// Flush issues a VIRTIO_BLK_T_FLUSH request, asking the device to commit
// volatile write caches to stable storage.
func (b *VirtioBlk) Flush() error {
	return b.doRequest(BlkTypeFlush, 0, nil, false)
}

// doRequest builds one virtio_blk_req descriptor chain — header (R),
// optional data (R for writes / W for reads), status (W) — rings the
// doorbell, busy-polls for completion, and maps the status byte to an
// error. For reads (deviceWritesData) the device-filled data is copied
// back into `data` on success.
func (b *VirtioBlk) doRequest(reqType uint32, sector uint64, data []byte, deviceWritesData bool) error {
	// Meta page holds the 16-byte header at offset 0 and the 1-byte
	// status trailer at BlkReqHeaderSize.
	metaPhys, metaMem, err := b.transport.AllocatePages(1)
	if err != nil {
		return err
	}
	if metaPhys == 0 {
		return common.ErrAllocReturnedZero
	}
	binary.LittleEndian.PutUint32(metaMem[0:4], reqType)
	binary.LittleEndian.PutUint32(metaMem[4:8], 0) // reserved
	binary.LittleEndian.PutUint64(metaMem[8:16], sector)
	metaMem[BlkReqHeaderSize] = 0xFF // sentinel; device overwrites

	chain := []common.ChainBuffer{
		{Addr: uintptr(metaPhys), Phys: metaPhys, Len: BlkReqHeaderSize, Writable: false},
	}

	var dataMem []byte
	if len(data) > 0 {
		pages := (len(data) + int(common.PageSize) - 1) / int(common.PageSize)
		var dataPhys uint64
		dataPhys, dataMem, err = b.transport.AllocatePages(pages)
		if err != nil {
			return err
		}
		if dataPhys == 0 {
			return common.ErrAllocReturnedZero
		}
		if !deviceWritesData {
			copy(dataMem, data) // write: load the source bytes for the device to read
		}
		chain = append(chain, common.ChainBuffer{
			Addr: uintptr(dataPhys), Phys: dataPhys, Len: uint32(len(data)), Writable: deviceWritesData,
		})
	}

	statusPhys := metaPhys + BlkReqHeaderSize
	chain = append(chain, common.ChainBuffer{
		Addr: uintptr(statusPhys), Phys: statusPhys, Len: 1, Writable: true,
	})

	head, err := b.rq.AddChain(chain)
	if err != nil {
		return err
	}
	if err := b.Cfg.NotifyQueue(RequestQueueIdx, b.rq.NotifyOff); err != nil {
		return err
	}
	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := b.rq.PollUsed()
		if !ok {
			continue
		}
		_ = b.rq.ReclaimChain(gotIdx)
		switch metaMem[BlkReqHeaderSize] {
		case BlkStatusOK:
			if deviceWritesData {
				copy(data, dataMem[:len(data)])
			}
			return nil
		case BlkStatusUnsupp:
			return ErrUnsupported
		default:
			return ErrIO
		}
	}
	_ = b.rq.ReclaimChain(head)
	return ErrRequestTimeout
}

// Sentinel errors for the virtio-blk path.
var (
	ErrNotModernDevice   = commonBlkError("go-virtio/blk: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK     = commonBlkError("go-virtio/blk: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID = commonBlkError("go-virtio/blk: PCI device ID is not 0x1042 (modern block device)")
	ErrQueueNotAvailable = commonBlkError("go-virtio/blk: device reports QueueSize=0 for the request queue")
	ErrRequestTimeout    = commonBlkError("go-virtio/blk: request poll timeout (device did not complete the request)")
	ErrZeroCount         = commonBlkError("go-virtio/blk: ReadBlocks count must be positive")
	ErrUnalignedLength   = commonBlkError("go-virtio/blk: write length must be a positive multiple of BlockSize (512)")
	ErrReadOnly          = commonBlkError("go-virtio/blk: device is read-only (VIRTIO_BLK_F_RO)")
	ErrIO                = commonBlkError("go-virtio/blk: device reported VIRTIO_BLK_S_IOERR")
	ErrUnsupported       = commonBlkError("go-virtio/blk: device reported VIRTIO_BLK_S_UNSUPP")
)

// commonBlkError is the package's tiny sentinel-error type.
type commonBlkError string

func (e commonBlkError) Error() string { return string(e) }

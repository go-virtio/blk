// Tests for the OpenVirtioBlk driver path and the ReadBlocks /
// WriteBlocks / Flush request path. fakeBlkDevice is a minimal in-memory
// virtio-blk device that, on a request-queue doorbell, walks the
// descriptor chain (header + optional data + status), executes the
// request against a flat backing store, writes the status byte, and
// publishes a used-ring entry.
//
// The driver itself needs no unsafe (it reads the DMA []byte it holds);
// the test does, to play the device side that reads/writes guest memory
// by physical address.

package blk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"unsafe"

	"github.com/go-virtio/common"
)

var le = binary.LittleEndian

func uintptrFromSlice(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

// sliceAt reconstructs a guest-memory byte view from a physical address
// — the device side of the DMA contract (in this fake, phys is a real Go
// pointer produced by AllocatePages).
func sliceAt(phys uint64, n int) []byte {
	if phys == 0 || n <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(phys))), n)
}

func TestDeviceType(t *testing.T) {
	if DeviceType != 2 {
		t.Errorf("DeviceType: got %d, want 2", DeviceType)
	}
}

type fakeBlkDevice struct {
	mu sync.Mutex

	cfg []byte

	deviceFeatureSelect uint32
	deviceFeatures      uint64
	driverFeatures      uint64
	deviceStatus        uint8
	currentQueue        uint16

	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	bar map[uint64]uint64

	capacity        uint64
	backing         []byte
	clearFeaturesOK bool
	completes       bool
	forceStatus     int // -1 = process normally; >=0 = force this status byte
	reqConsumed     uint16

	heldPages [][]byte
	allocFail bool
}

func newFakeBlkDevice(deviceFeats uint64, capacitySectors uint64) *fakeBlkDevice {
	d := &fakeBlkDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0},
		bar:            map[uint64]uint64{},
		capacity:       capacitySectors,
		backing:        make([]byte, capacitySectors*BlockSize),
		completes:      true,
		forceStatus:    -1,
	}
	for i := range d.backing {
		d.backing[i] = byte(i) // deterministic pattern for read verification
	}
	d.cfg = buildVirtioBlkCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

func (d *fakeBlkDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeBlkDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeBlkDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

func (d *fakeBlkDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

func (d *fakeBlkDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeBlkDevice) commonCfgOffset() uint64 { return 0 }

const deviceCfgOff = 0x8000

func (d *fakeBlkDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeBlkDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 1, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeBlkDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeBlkDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	if bar == 0 && off >= deviceCfgOff && off < deviceCfgOff+8 {
		return d.capacity, nil
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeBlkDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeBlkDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleRequest()
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeBlkDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleRequest()
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeBlkDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

type fakeDesc struct {
	addr   uint64
	length uint32
	flags  uint16
	next   uint16
}

// handleRequest is the device side of one request: walk the chain,
// execute it against the backing store, write status, publish used.
// Called from Write32/Write16 with d.mu held.
func (d *fakeBlkDevice) handleRequest() {
	if !d.completes {
		return
	}
	const q = RequestQueueIdx
	availAddr := d.qdriver[q]
	usedAddr := d.qdevice[q]
	descAddr := d.qdesc[q]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return
	}
	size := d.qsize[q]
	availSlice := sliceAt(availAddr, 4+2*int(size))
	availIdx := le.Uint16(availSlice[2:4])
	if d.reqConsumed >= availIdx {
		return
	}
	slot := d.reqConsumed % size
	head := le.Uint16(availSlice[4+slot*2 : 4+slot*2+2])

	descSlice := sliceAt(descAddr, 16*int(size))
	var descs []fakeDesc
	idx := head
	for i := 0; i < int(size); i++ {
		o := int(idx) * 16
		dd := fakeDesc{
			addr:   le.Uint64(descSlice[o : o+8]),
			length: le.Uint32(descSlice[o+8 : o+12]),
			flags:  le.Uint16(descSlice[o+12 : o+14]),
			next:   le.Uint16(descSlice[o+14 : o+16]),
		}
		descs = append(descs, dd)
		if dd.flags&common.VirtqDescFNext == 0 {
			break
		}
		idx = dd.next
	}

	hdr := sliceAt(descs[0].addr, BlkReqHeaderSize)
	reqType := le.Uint32(hdr[0:4])
	sector := le.Uint64(hdr[8:16])
	statusDesc := descs[len(descs)-1]
	var dataDesc *fakeDesc
	if len(descs) == 3 {
		dataDesc = &descs[1]
	}

	status := BlkStatusOK
	usedLen := uint32(1)
	if d.forceStatus >= 0 {
		status = byte(d.forceStatus)
	} else {
		switch reqType {
		case BlkTypeIn:
			if dataDesc != nil {
				buf := sliceAt(dataDesc.addr, int(dataDesc.length))
				d.copyOut(buf, sector)
				usedLen = dataDesc.length + 1
			}
		case BlkTypeOut:
			if dataDesc != nil {
				buf := sliceAt(dataDesc.addr, int(dataDesc.length))
				d.copyIn(buf, sector)
			}
		case BlkTypeFlush:
			// no data
		default:
			status = BlkStatusUnsupp
		}
	}
	sliceAt(statusDesc.addr, 1)[0] = status

	usedSlice := sliceAt(usedAddr, 4+8*int(size))
	usedIdx := le.Uint16(usedSlice[2:4])
	uslot := usedIdx % size
	uo := 4 + int(uslot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(head))
	le.PutUint32(usedSlice[uo+4:uo+8], usedLen)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
	d.reqConsumed++
}

func (d *fakeBlkDevice) copyOut(dst []byte, sector uint64) {
	off := sector * BlockSize
	for i := range dst {
		if off+uint64(i) < uint64(len(d.backing)) {
			dst[i] = d.backing[off+uint64(i)]
		}
	}
}

func (d *fakeBlkDevice) copyIn(src []byte, sector uint64) {
	off := sector * BlockSize
	for i := range src {
		if off+uint64(i) < uint64(len(d.backing)) {
			d.backing[off+uint64(i)] = src[i]
		}
	}
}

func buildVirtioBlkCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], common.PCIDeviceIDModernBlock)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50
	cfg[0x42] = 16
	cfg[0x43] = common.PCICapCommonCfg
	le.PutUint32(cfg[0x48:], 0)
	le.PutUint32(cfg[0x4C:], 0x38)

	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x68
	cfg[0x52] = 20
	cfg[0x53] = common.PCICapNotifyCfg
	le.PutUint32(cfg[0x58:], 0x1000)
	le.PutUint32(cfg[0x5C:], 0x100)
	le.PutUint32(cfg[0x60:], 4)

	cfg[0x68] = common.PCICapIDVendorSpecific
	cfg[0x69] = 0x00
	cfg[0x6A] = 16
	cfg[0x6B] = common.PCICapDeviceCfg
	le.PutUint32(cfg[0x70:], deviceCfgOff)
	le.PutUint32(cfg[0x74:], 8) // capacity le64

	return cfg
}

// --- happy path + semantics -------------------------------------------

func TestOpenVirtioBlk_Success(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 2048)
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	if v.Capacity != 2048 {
		t.Errorf("Capacity: got %d, want 2048", v.Capacity)
	}
	if v.ReadOnly {
		t.Error("ReadOnly should be false")
	}
	if v.RequestQueue() == nil {
		t.Error("RequestQueue nil")
	}
}

func TestOpenVirtioBlk_ReadOnly(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1|featureRO, 64)
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	if !v.ReadOnly {
		t.Error("ReadOnly should be true (F_RO offered)")
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1 | featureRO); err != nil || got != common.FeatureVersion1 {
		t.Errorf("modern: got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(featureRO); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("legacy: got %v", err)
	}
}

func TestOpenVirtioBlk_WrongDeviceID(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernNet)
	if _, err := OpenVirtioBlk(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBlk_LegacyDevice(t *testing.T) {
	d := newFakeBlkDevice(featureRO, 64) // no VERSION_1
	if _, err := OpenVirtioBlk(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBlk_FeaturesNotOK(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioBlk(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBlk_QueueZeroSize(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	d.qsize[0] = 0
	if _, err := OpenVirtioBlk(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioBlk_QueueSizeClampAndRound(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	d.qsize[0] = 6 // clamp 16->6, round 6->4
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	if got := v.RequestQueue().Layout.Size; got != 4 {
		t.Errorf("queue size: got %d, want 4", got)
	}
}

// --- request path -----------------------------------------------------

func TestReadBlocks_RoundTrip(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	got, err := v.ReadBlocks(1, 2) // sectors 1..2
	if err != nil {
		t.Fatalf("ReadBlocks: %v", err)
	}
	if len(got) != 2*BlockSize {
		t.Fatalf("len: got %d, want %d", len(got), 2*BlockSize)
	}
	if !bytes.Equal(got, d.backing[BlockSize:3*BlockSize]) {
		t.Error("read data does not match backing store")
	}
}

func TestReadBlocks_ZeroCount(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, _ := OpenVirtioBlk(d)
	if _, err := v.ReadBlocks(0, 0); !errors.Is(err, ErrZeroCount) {
		t.Errorf("got %v", err)
	}
}

func TestWriteBlocks_RoundTrip(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	payload := bytes.Repeat([]byte{0xAB}, BlockSize)
	if err := v.WriteBlocks(3, payload); err != nil {
		t.Fatalf("WriteBlocks: %v", err)
	}
	if !bytes.Equal(d.backing[3*BlockSize:4*BlockSize], payload) {
		t.Error("backing store not updated by write")
	}
}

func TestWriteBlocks_Unaligned(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, _ := OpenVirtioBlk(d)
	if err := v.WriteBlocks(0, make([]byte, 500)); !errors.Is(err, ErrUnalignedLength) {
		t.Errorf("got %v", err)
	}
	if err := v.WriteBlocks(0, nil); !errors.Is(err, ErrUnalignedLength) {
		t.Errorf("empty: got %v", err)
	}
}

func TestWriteBlocks_ReadOnly(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1|featureRO, 64)
	v, _ := OpenVirtioBlk(d)
	if err := v.WriteBlocks(0, make([]byte, BlockSize)); !errors.Is(err, ErrReadOnly) {
		t.Errorf("got %v", err)
	}
}

func TestFlush_RoundTrip(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	if err := v.Flush(); err != nil {
		t.Errorf("Flush: %v", err)
	}
}

func TestRequest_IOErr(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, _ := OpenVirtioBlk(d)
	d.forceStatus = int(BlkStatusIOErr)
	if _, err := v.ReadBlocks(0, 1); !errors.Is(err, ErrIO) {
		t.Errorf("got %v", err)
	}
}

func TestRequest_Unsupported(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, _ := OpenVirtioBlk(d)
	d.forceStatus = int(BlkStatusUnsupp)
	if err := v.Flush(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v", err)
	}
}

func TestRequest_Timeout(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, _ := OpenVirtioBlk(d)
	d.completes = false
	if _, err := v.ReadBlocks(0, 1); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestRequest_AllocFailMeta(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, _ := OpenVirtioBlk(d)
	d.allocFail = true
	if _, err := v.ReadBlocks(0, 1); err == nil {
		t.Error("expected meta alloc error")
	}
}

func TestRequest_AllocZeroPhysMeta(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	it := newInject(d, false)
	v, _ := OpenVirtioBlk(it)
	it.enable = true
	it.zeroPhys = true // zeroPhysAfter=0 -> first (meta) alloc returns 0
	if _, err := v.ReadBlocks(0, 1); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestRequest_AllocFailData(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	it := newInject(d, false)
	v, _ := OpenVirtioBlk(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 2} // meta ok, data fails
	if _, err := v.ReadBlocks(0, 1); err == nil {
		t.Error("expected data alloc error")
	}
}

func TestRequest_AllocZeroPhysData(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	it := newInject(d, false)
	v, _ := OpenVirtioBlk(it)
	it.enable = true
	it.zeroPhys = true
	it.zeroPhysAfter = 1 // meta (#1) real, data (#2) zero
	if _, err := v.ReadBlocks(0, 1); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestRequest_AddChainFull(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	v, err := OpenVirtioBlk(d)
	if err != nil {
		t.Fatalf("OpenVirtioBlk: %v", err)
	}
	q := v.RequestQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, 16, false); err != nil {
			t.Fatalf("saturate[%d]: %v", i, err)
		}
	}
	if _, err := v.ReadBlocks(0, 1); err == nil {
		t.Error("expected AddChain queue-full error")
	}
}

func TestRequest_NotifyFail(t *testing.T) {
	d := newFakeBlkDevice(common.FeatureVersion1, 64)
	it := newInject(d, false)
	v, _ := OpenVirtioBlk(it)
	it.enable = true
	it.fp = failPoint{"Write32", 1} // request doorbell
	if _, err := v.ReadBlocks(0, 1); err == nil {
		t.Error("expected notify error")
	}
}

func TestSentinelError(t *testing.T) {
	if got := ErrIO.Error(); got != string(ErrIO) {
		t.Errorf("Error(): %q", got)
	}
}

// --- injection harness + Open transport-error coverage ----------------

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int
}

type injectTransport struct {
	*fakeBlkDevice
	fp            failPoint
	counts        map[string]int
	enable        bool
	zeroPhys      bool
	zeroPhysAfter int
	allocCalls    int
}

func newInject(d *fakeBlkDevice, enable bool) *injectTransport {
	return &injectTransport{fakeBlkDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeBlkDevice.ReadConfig16(o)
}
func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeBlkDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeBlkDevice.Read16(b, o)
}
func (t *injectTransport) Read64(b uint8, o uint64) (uint64, error) {
	if t.fail("Read64") {
		return 0, errInjected
	}
	return t.fakeBlkDevice.Read64(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeBlkDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeBlkDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	return t.fakeBlkDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeBlkDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	phys, mem, err := t.fakeBlkDevice.AllocatePages(c)
	// Count only while armed so zeroPhysAfter is relative to the
	// request under test, not to the queue allocs done during Open.
	if t.enable {
		t.allocCalls++
		if t.zeroPhys && t.allocCalls > t.zeroPhysAfter {
			return 0, mem, nil
		}
	}
	return phys, mem, err
}

func TestOpenVirtioBlk_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}},
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}},
		{"DriverFeatures", failPoint{"Write32", 3}},
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		{"SelectQueue", failPoint{"Write16", 1}},
		{"QueueSize", failPoint{"Read16", 1}},
		{"SetQueueSize", failPoint{"Write16", 2}},
		{"QueueNotifyOff", failPoint{"Read16", 2}},
		{"AllocVirtqueue", failPoint{"AllocatePages", 1}},
		{"SetQueueDesc", failPoint{"Write64", 1}},
		{"SetQueueDriver", failPoint{"Write64", 2}},
		{"SetQueueDevice", failPoint{"Write64", 3}},
		{"SetQueueEnable", failPoint{"Write16", 3}},
		{"DriverOKStatus", failPoint{"Write8", 5}},
		{"CapacityRead", failPoint{"Read64", 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeBlkDevice(common.FeatureVersion1, 64)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioBlk(it); err == nil {
				t.Fatalf("%s: expected error at %+v", tc.name, tc.fp)
			}
		})
	}
}

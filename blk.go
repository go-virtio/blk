// Package blk is a placeholder for a future pure-Go virtio-blk driver
// targeting the go-virtio/common transport interfaces.
//
// Not yet implemented — cloud-boot's pre-ExitBootServices phase uses
// UEFI's EFI_BLOCK_IO_PROTOCOL for block access today. When a concrete
// post-EBS caller needs pure-Go block access, this package will host
// the driver, mirroring the structure of go-virtio/net.
//
// References (for the future implementation):
//
//   - Virtio 1.1 §5.2  "Block Device".
//   - Linux drivers/block/virtio_blk.c — canonical Go-translatable
//     reference.
package blk

// DeviceType is the virtio device-type encoding for virtio-blk
// (Virtio 1.1 §5.2.1). Exposed for callers enumerating PCI devices
// that want a stable name.
const DeviceType uint16 = 2

// TODO: implement OpenVirtioBlk(transport common.Transport) plus
// the request virtqueue driver (Virtio 1.1 §5.2.6).

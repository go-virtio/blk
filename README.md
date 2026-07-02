<p align="center"><img src="https://raw.githubusercontent.com/go-virtio/brand/main/social/go-virtio-blk.png" alt="go-virtio/blk" width="720"></p>

# go-virtio/blk

[![Go Reference](https://pkg.go.dev/badge/github.com/go-virtio/blk.svg)](https://pkg.go.dev/github.com/go-virtio/blk)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-virtio/blk/actions/workflows/ci.yml/badge.svg)](https://github.com/go-virtio/blk/actions/workflows/ci.yml)

Pure-Go virtio-blk (block device) driver targeting the `go-virtio/common`
transport interfaces. Implements the modern-transport (Virtio 1.0+) init
sequence and the request-queue I/O path for the standard PCI-bound
virtio-blk device (VID 0x1AF4, DID 0x1042).

## Scope

Like [`go-virtio/net`](https://github.com/go-virtio/net) this package
owns device bring-up, the single request virtqueue, and the on-the-wire
request format (the header + data + status **descriptor chain**, Virtio
1.1 §5.2.6, built with `common.AddChain`), exposing a block-level
`ReadBlocks` / `WriteBlocks` / `Flush` API. The protocol sector size is
always 512 bytes (`BlockSize`).

`VIRTIO_BLK_F_RO` is honoured (read-only devices reject writes); no other
feature bit is negotiated.

The device backing the driver is the host's concern and is transparent to
the guest — it can be a local disk image **or** a network volume served
over NBD (with NBD itself tunnelled through WireGuard or TLS). The driver
just sees a block device.

## Quick start

```go
import (
    virtioblk "github.com/go-virtio/blk"
)

// transport is any value that implements go-virtio/common.Transport.
vb, err := virtioblk.OpenVirtioBlk(transport)
if err != nil {
    return err
}
fmt.Printf("capacity: %d sectors (%d bytes)\n",
    vb.Capacity, vb.Capacity*virtioblk.BlockSize)

// Read sectors 0..3 (4 × 512 B).
data, err := vb.ReadBlocks(0, 4)

// Write two sectors at LBA 100, then flush.
err = vb.WriteBlocks(100, make([]byte, 2*virtioblk.BlockSize))
err = vb.Flush()
```

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue + descriptor-chain impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver (the reference per-device-class driver this
    package mirrors).
  - [`github.com/go-virtio/rng`](https://github.com/go-virtio/rng) —
    pure-Go virtio-rng driver.
  - [`github.com/go-virtio/vsock`](https://github.com/go-virtio/vsock) —
    pure-Go virtio-vsock driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).

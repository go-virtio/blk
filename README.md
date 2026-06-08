# go-virtio/blk

Placeholder for a future pure-Go virtio-blk driver targeting the
[`go-virtio/common`](https://github.com/go-virtio/common) transport
interfaces.

**Status: not yet implemented.** The package exists so `go get`
resolves and so the layout of the `go-virtio` org is symmetric with
the Linux kernel's `<linux/virtio_net.h>` / `<linux/virtio_blk.h>`
per-device-class split. Today, cloud-boot consumers that need block
access during the pre-EBS phase use UEFI's `EFI_BLOCK_IO_PROTOCOL`
directly; the pure-Go driver will be built when there's a concrete
caller that needs it.

When implemented, the driver will:

  - Open a modern virtio-blk PCI device (VID 0x1AF4, DID 0x1042)
    through `go-virtio/common.Transport`.
  - Negotiate `VIRTIO_F_VERSION_1` and the virtio-blk-specific feature
    bits (Virtio 1.1 §5.2.3).
  - Drive the request virtqueue per Virtio 1.1 §5.2.6 (header
    descriptor + data descriptors + status byte descriptor).
  - Expose a Go-friendly read/write API.

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).

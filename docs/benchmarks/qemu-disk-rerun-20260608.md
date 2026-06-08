# QEMU Disk Benchmark Rerun

Generated: `2026-06-08T13:39Z`

Host: `root@178.128.247.74`  
QEMU: `8.2.2`  
Kernel: `/boot/vmlinuz-6.8.0-71-generic`  
Data file: `.tmp/disk-bench/bench-128mib.dat`  
Benchmark: `128 MiB`, sequential `1 MiB` reads, random `32768 x 4 KiB`, `iodepth=1`, buffered, host caches dropped before each mode.

| Runtime | Mode | Throughput | IOPS | Guest Duration | Wall Time |
| --- | --- | ---: | ---: | ---: | ---: |
| QEMU q35 virtio-blk-pci | sequential | 316 MiB/s | 316 | 404 ms | 3764 ms |
| QEMU q35 virtio-blk-pci | random | 7 MiB/s | 1878 | 17441 ms | 20770 ms |
| QEMU microvm virtio-blk-device | sequential | 350 MiB/s | 350 | 365 ms | 6824 ms |
| QEMU microvm virtio-blk-device | random | 7 MiB/s | 1972 | 16612 ms | 22957 ms |

## Read

QEMU is still substantially faster than Firecracker/vhost-user-blk for sequential reads in this benchmark shape, but this rerun is lower than the earlier `445-456 MiB/s` q35/microvm numbers. Random reads in this rerun are also lower than the earlier `4071-4732 IOPS` numbers.

Compared with the fresh Firecracker vhost rerun:

| Runtime | Sequential | Random |
| --- | ---: | ---: |
| Firecracker virtio-blk | 149 MiB/s | 1703 IOPS |
| Firecracker vhost-user-blk via qemu-storage-daemon | 129 MiB/s | 1413 IOPS |
| QEMU q35 virtio-blk-pci | 316 MiB/s | 1878 IOPS |
| QEMU microvm virtio-blk-device | 350 MiB/s | 1972 IOPS |

The fresh signal is: QEMU still improves sequential throughput by about `2.1-2.3x` over Firecracker on the same VM and benchmark shape, while random is only slightly better in this rerun.

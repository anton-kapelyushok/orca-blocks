# Firecracker vhost-user-blk Benchmark

Generated: `2026-06-08T13:31:46Z`

This compares Firecracker's built-in file-backed `virtio-blk` drive with a `vhost-user-blk` drive backed by `qemu-storage-daemon` over a Unix socket. It does not start the Orca stack.

| Backend | Mode | Throughput | IOPS | Guest Duration | Wall Time | Block | IO Depth | Direct | Readahead |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | ---: |
| virtio-blk | sequential | 153 MiB/s | 153 | 1665 ms | 3513 ms | 1048576 | 1 | False | -1 |
| virtio-blk | random | 3.00 MiB/s | 1019 | 4018 ms | 5933 ms | 4096 | 1 | False | -1 |
| vhost-user-blk | sequential | 151 MiB/s | 151 | 1691 ms | 3398 ms | 1048576 | 1 | False | -1 |
| vhost-user-blk | random | 3.00 MiB/s | 925 | 4424 ms | 6267 ms | 4096 | 1 | False | -1 |

## Raw Logs

<details>
<summary>virtio-blk</summary>

```text
using prebuilt disk benchmark at /root/orca-blocks/.tmp/disk-bench/disk-bench-linux-amd64
creating 256 MiB benchmark data file at .tmp/disk-bench/bench-256mib.dat
direct_io=False
read_ahead_kb=default
io_engine=default
backend=virtio-blk
io_depth=1
modes=sequential,random
skip_docker=True
running sequential disk benchmark in Firecracker

[firecracker-sequential] mode=sequential duration_ms=1665 wall_ms=3513 mb_per_sec=153 iops=153 block_bytes=1048576 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=268435456 checksum=4121226589727191916
  serial_log=.tmp/disk-bench/firecracker.efWXlo/serial.log
running random disk benchmark in Firecracker

[firecracker-random] mode=random duration_ms=4018 wall_ms=5933 mb_per_sec=3 iops=1019 block_bytes=4096 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=16777216 checksum=1128448524560105170
  serial_log=.tmp/disk-bench/firecracker.X22WbV/serial.log
SUMMARY_JSON={"random": {"firecracker": {"backend": "virtio-blk", "block_bytes": 4096, "bytes_read": 16777216, "checksum": 1128448524560105170, "direct": false, "duration_ms": 4018, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 1019, "kind": "firecracker-random", "mb_per_sec": 3, "mode": "random", "num_cpu": 1, "ops": 4096, "path": "/dev/vda", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.X22WbV/serial.log", "size_bytes": 268435456, "wall_ms": 5933, "work_dir": ".tmp/disk-bench/firecracker.X22WbV"}}, "sequential": {"firecracker": {"backend": "virtio-blk", "block_bytes": 1048576, "bytes_read": 268435456, "checksum": 4121226589727191916, "direct": false, "duration_ms": 1665, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 153, "kind": "firecracker-sequential", "mb_per_sec": 153, "mode": "sequential", "num_cpu": 1, "ops": 256, "path": "/dev/vda", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.efWXlo/serial.log", "size_bytes": 268435456, "wall_ms": 3513, "work_dir": ".tmp/disk-bench/firecracker.efWXlo"}}}
```

</details>

<details>
<summary>vhost-user-blk</summary>

```text
using prebuilt disk benchmark at /root/orca-blocks/.tmp/disk-bench/disk-bench-linux-amd64
reusing benchmark data file .tmp/disk-bench/bench-256mib.dat
direct_io=False
read_ahead_kb=default
io_engine=default
backend=vhost-user-blk
io_depth=1
modes=sequential,random
skip_docker=True
running sequential disk benchmark in Firecracker

[firecracker-sequential] mode=sequential duration_ms=1691 wall_ms=3398 mb_per_sec=151 iops=151 block_bytes=1048576 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=268435456 checksum=4121226589727191916
  serial_log=.tmp/disk-bench/firecracker.8cYhCl/serial.log
  qemu_log=.tmp/disk-bench/firecracker.8cYhCl/qemu-storage-daemon.log
running random disk benchmark in Firecracker

[firecracker-random] mode=random duration_ms=4424 wall_ms=6267 mb_per_sec=3 iops=925 block_bytes=4096 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=16777216 checksum=1128448524560105170
  serial_log=.tmp/disk-bench/firecracker.TNLHLw/serial.log
  qemu_log=.tmp/disk-bench/firecracker.TNLHLw/qemu-storage-daemon.log
SUMMARY_JSON={"random": {"firecracker": {"backend": "vhost-user-blk", "block_bytes": 4096, "bytes_read": 16777216, "checksum": 1128448524560105170, "direct": false, "duration_ms": 4424, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 925, "kind": "firecracker-random", "mb_per_sec": 3, "mode": "random", "num_cpu": 1, "ops": 4096, "path": "/dev/vda", "qemu_log": ".tmp/disk-bench/firecracker.TNLHLw/qemu-storage-daemon.log", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.TNLHLw/serial.log", "size_bytes": 268435456, "wall_ms": 6267, "work_dir": ".tmp/disk-bench/firecracker.TNLHLw"}}, "sequential": {"firecracker": {"backend": "vhost-user-blk", "block_bytes": 1048576, "bytes_read": 268435456, "checksum": 4121226589727191916, "direct": false, "duration_ms": 1691, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 151, "kind": "firecracker-sequential", "mb_per_sec": 151, "mode": "sequential", "num_cpu": 1, "ops": 256, "path": "/dev/vda", "qemu_log": ".tmp/disk-bench/firecracker.8cYhCl/qemu-storage-daemon.log", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.8cYhCl/serial.log", "size_bytes": 268435456, "wall_ms": 3398, "work_dir": ".tmp/disk-bench/firecracker.8cYhCl"}}}
```

</details>

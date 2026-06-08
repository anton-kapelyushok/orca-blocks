# Firecracker vhost-user-blk Benchmark

Generated: `2026-06-08T13:34:20Z`

This compares Firecracker's built-in file-backed `virtio-blk` drive with a `vhost-user-blk` drive backed by `qemu-storage-daemon` over a Unix socket. It does not start the Orca stack.

| Backend | Mode | Throughput | IOPS | Guest Duration | Wall Time | Block | IO Depth | Direct | Readahead |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- | ---: |
| virtio-blk | sequential | 149 MiB/s | 149 | 854 ms | 2745 ms | 1048576 | 1 | False | -1 |
| virtio-blk | random | 6.00 MiB/s | 1703 | 19233 ms | 20982 ms | 4096 | 1 | False | -1 |
| vhost-user-blk | sequential | 129 MiB/s | 129 | 988 ms | 2944 ms | 1048576 | 1 | False | -1 |
| vhost-user-blk | random | 5.00 MiB/s | 1413 | 23187 ms | 24873 ms | 4096 | 1 | False | -1 |

## Raw Logs

<details>
<summary>virtio-blk</summary>

```text
using prebuilt disk benchmark at /root/orca-blocks/.tmp/disk-bench/disk-bench-linux-amd64
creating 128 MiB benchmark data file at .tmp/disk-bench/bench-128mib.dat
direct_io=False
read_ahead_kb=default
io_engine=default
backend=virtio-blk
io_depth=1
modes=sequential,random
skip_docker=True
running sequential disk benchmark in Firecracker

[firecracker-sequential] mode=sequential duration_ms=854 wall_ms=2745 mb_per_sec=149 iops=149 block_bytes=1048576 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=134217728 checksum=12649089230117382405
  serial_log=.tmp/disk-bench/firecracker.b1rMKj/serial.log
running random disk benchmark in Firecracker

[firecracker-random] mode=random duration_ms=19233 wall_ms=20982 mb_per_sec=6 iops=1703 block_bytes=4096 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=134217728 checksum=18344845538656812094
  serial_log=.tmp/disk-bench/firecracker.kqQ9eK/serial.log
SUMMARY_JSON={"random": {"firecracker": {"backend": "virtio-blk", "block_bytes": 4096, "bytes_read": 134217728, "checksum": 18344845538656812094, "direct": false, "duration_ms": 19233, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 1703, "kind": "firecracker-random", "mb_per_sec": 6, "mode": "random", "num_cpu": 1, "ops": 32768, "path": "/dev/vda", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.kqQ9eK/serial.log", "size_bytes": 134217728, "wall_ms": 20982, "work_dir": ".tmp/disk-bench/firecracker.kqQ9eK"}}, "sequential": {"firecracker": {"backend": "virtio-blk", "block_bytes": 1048576, "bytes_read": 134217728, "checksum": 12649089230117382405, "direct": false, "duration_ms": 854, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 149, "kind": "firecracker-sequential", "mb_per_sec": 149, "mode": "sequential", "num_cpu": 1, "ops": 128, "path": "/dev/vda", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.b1rMKj/serial.log", "size_bytes": 134217728, "wall_ms": 2745, "work_dir": ".tmp/disk-bench/firecracker.b1rMKj"}}}
```

</details>

<details>
<summary>vhost-user-blk</summary>

```text
using prebuilt disk benchmark at /root/orca-blocks/.tmp/disk-bench/disk-bench-linux-amd64
reusing benchmark data file .tmp/disk-bench/bench-128mib.dat
direct_io=False
read_ahead_kb=default
io_engine=default
backend=vhost-user-blk
io_depth=1
modes=sequential,random
skip_docker=True
running sequential disk benchmark in Firecracker

[firecracker-sequential] mode=sequential duration_ms=988 wall_ms=2944 mb_per_sec=129 iops=129 block_bytes=1048576 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=134217728 checksum=12649089230117382405
  serial_log=.tmp/disk-bench/firecracker.ErOand/serial.log
  qemu_log=.tmp/disk-bench/firecracker.ErOand/qemu-storage-daemon.log
running random disk benchmark in Firecracker

[firecracker-random] mode=random duration_ms=23187 wall_ms=24873 mb_per_sec=5 iops=1413 block_bytes=4096 direct=False read_ahead_kb=-1 io_depth=1 bytes_read=134217728 checksum=18344845538656812094
  serial_log=.tmp/disk-bench/firecracker.UaA54K/serial.log
  qemu_log=.tmp/disk-bench/firecracker.UaA54K/qemu-storage-daemon.log
SUMMARY_JSON={"random": {"firecracker": {"backend": "vhost-user-blk", "block_bytes": 4096, "bytes_read": 134217728, "checksum": 18344845538656812094, "direct": false, "duration_ms": 23187, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 1413, "kind": "firecracker-random", "mb_per_sec": 5, "mode": "random", "num_cpu": 1, "ops": 32768, "path": "/dev/vda", "qemu_log": ".tmp/disk-bench/firecracker.UaA54K/qemu-storage-daemon.log", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.UaA54K/serial.log", "size_bytes": 134217728, "wall_ms": 24873, "work_dir": ".tmp/disk-bench/firecracker.UaA54K"}}, "sequential": {"firecracker": {"backend": "vhost-user-blk", "block_bytes": 1048576, "bytes_read": 134217728, "checksum": 12649089230117382405, "direct": false, "duration_ms": 988, "go_version": "go1.25.1", "gomaxprocs": 1, "io_depth": 1, "iops": 129, "kind": "firecracker-sequential", "mb_per_sec": 129, "mode": "sequential", "num_cpu": 1, "ops": 128, "path": "/dev/vda", "qemu_log": ".tmp/disk-bench/firecracker.ErOand/qemu-storage-daemon.log", "read_ahead_kb": -1, "serial_log": ".tmp/disk-bench/firecracker.ErOand/serial.log", "size_bytes": 134217728, "wall_ms": 2944, "work_dir": ".tmp/disk-bench/firecracker.ErOand"}}}
```

</details>

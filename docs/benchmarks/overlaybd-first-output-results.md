# OverlayBD first-output and read benchmark

Generated: `2026-06-08T15:02:15.861Z`

The image contains a `256 MiB` `/data/bench.dat` blob. `Prefetch` is `ctr rpull --snapshotter overlaybd` with blob download disabled, then the command is timed separately. Byte columns are local content-store plus OverlayBD cache/snapshotter growth.

| Case | Operation | Prefetch | Rpull | Command | Throughput | IOPS | Prefetch bytes | Command bytes | Total bytes |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| cold | first_output |  | 83 ms | 324 ms |  |  | 0 B | 269.8 MiB | 269.9 MiB |
| metadata_prefetched | first_output | 53 ms |  | 345 ms |  |  | 136.0 KiB | 269.8 MiB | 269.9 MiB |
| metadata_prefetched | sequential | 56 ms |  | 4281 ms | 64 MiB/s | 64 | 140.0 KiB | 526.8 MiB | 527.1 MiB |
| metadata_prefetched | random | 128 ms |  | 7350 ms | 36 MiB/s | 9463 | 148.0 KiB | 526.8 MiB | 527.1 MiB |

```json
{
  "built": true,
  "converted": true,
  "io_depth": 1,
  "normal_image": "localhost:15003/orca-overlaybd-disk-bench-normal:size256m-1780930935",
  "overlaybd_image": "localhost:15003/orca-overlaybd-disk-bench-obd:size256m-1780930935",
  "random_ops": 65536,
  "rows": [
    {
      "bench": {},
      "command_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 282853376
      },
      "command_ms": 324,
      "measured_rpull_ms": 83,
      "operation": "first_output",
      "prefetch": false,
      "prefetch_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 0
      },
      "prefetch_ms": "",
      "rpull_delta": {
        "content_bytes": 8192,
        "overlaybd_bytes": 143360
      },
      "total_delta": {
        "content_bytes": 8192,
        "overlaybd_bytes": 282996736
      }
    },
    {
      "bench": {},
      "command_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 282853376
      },
      "command_ms": 345,
      "measured_rpull_ms": "",
      "operation": "first_output",
      "prefetch": true,
      "prefetch_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 139264
      },
      "prefetch_ms": 53,
      "rpull_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 0
      },
      "total_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 282996736
      }
    },
    {
      "bench": {
        "block_bytes": 1048576,
        "bytes_read": 268435456,
        "checksum": 11299922703934946345,
        "direct": false,
        "duration_ms": 3965,
        "go_version": "go1.25.1",
        "gomaxprocs": 1,
        "io_depth": 1,
        "iops": 64,
        "kind": "overlaybd-sequential",
        "mb_per_sec": 64,
        "mode": "sequential",
        "num_cpu": 2,
        "ops": 256,
        "path": "/data/bench.dat",
        "read_ahead_kb": -1,
        "size_bytes": 268435456
      },
      "command_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 552341504
      },
      "command_ms": 4281,
      "measured_rpull_ms": "",
      "operation": "sequential",
      "prefetch": true,
      "prefetch_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 143360
      },
      "prefetch_ms": 56,
      "rpull_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 0
      },
      "total_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 552747008
      }
    },
    {
      "bench": {
        "block_bytes": 4096,
        "bytes_read": 268435456,
        "checksum": 7691740342868081970,
        "direct": false,
        "duration_ms": 6925,
        "go_version": "go1.25.1",
        "gomaxprocs": 1,
        "io_depth": 1,
        "iops": 9463,
        "kind": "overlaybd-random",
        "mb_per_sec": 36,
        "mode": "random",
        "num_cpu": 2,
        "ops": 65536,
        "path": "/data/bench.dat",
        "read_ahead_kb": -1,
        "size_bytes": 268435456
      },
      "command_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 552341504
      },
      "command_ms": 7350,
      "measured_rpull_ms": "",
      "operation": "random",
      "prefetch": true,
      "prefetch_delta": {
        "content_bytes": 8192,
        "overlaybd_bytes": 143360
      },
      "prefetch_ms": 128,
      "rpull_delta": {
        "content_bytes": 0,
        "overlaybd_bytes": 0
      },
      "total_delta": {
        "content_bytes": 8192,
        "overlaybd_bytes": 552747008
      }
    }
  ],
  "size_mb": 256,
  "started": "2026-06-08T15:02:15.861Z",
  "work_dir": "/root/orca-blocks/.tmp/overlaybd-first-output/20260608T150215Z"
}
```

# Sysbox stargz IOPS

Generated: `2026-06-08T14:10:23.346Z`

Each operation starts a fresh Sysbox container, starts containerd plus `containerd-stargz-grpc` inside it, then measures `ctr-remote images rpull` plus the container action. The local registry and eStargz image are prepared on the host.

| Runtime | Operation | Wall | prefetch | rpull | Bench | Throughput | IOPS |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| sysbox-stargz-partial-base | first_user_output | 9954 ms | 2377 ms | 9147 ms |  |  |  |
| sysbox-stargz-partial-base | sequential_read | 12406 ms | 1911 ms | 11530 ms | 99 ms | 2583 MiB/s | 2583 |
| sysbox-stargz-partial-base | random_read | 10473 ms | 2440 ms | 9581 ms | 189 ms | 1354 MiB/s | 346740 |

```json
{
  "base_built": true,
  "base_esgz_image": "127.0.0.1:15002/orca-sysbox-base-esgz:alpine-3.22",
  "base_normal_image": "127.0.0.1:15002/orca-sysbox-base-normal:alpine-3.22",
  "base_optimized": true,
  "built": false,
  "esgz_image": "127.0.0.1:15002/orca-sysbox-disk-bench-esgz:size256m-rand65536-qd1",
  "normal_image": "127.0.0.1:15002/orca-sysbox-disk-bench-normal:size256m-rand65536-qd1",
  "optimized": false,
  "partial_base_cache": true,
  "rows": [
    {
      "bench_duration_ms": "",
      "iops": "",
      "mb_per_sec": "",
      "operation": "first_user_output",
      "prefetch_ms": 2377,
      "rpull_ms": 9147,
      "runtime": "sysbox-stargz-partial-base",
      "wall_ms": 9954
    },
    {
      "bench_duration_ms": 99,
      "iops": 2583,
      "mb_per_sec": 2583,
      "operation": "sequential_read",
      "prefetch_ms": 1911,
      "rpull_ms": 11530,
      "runtime": "sysbox-stargz-partial-base",
      "wall_ms": 12406
    },
    {
      "bench_duration_ms": 189,
      "iops": 346740,
      "mb_per_sec": 1354,
      "operation": "random_read",
      "prefetch_ms": 2440,
      "rpull_ms": 9581,
      "runtime": "sysbox-stargz-partial-base",
      "wall_ms": 10473
    }
  ],
  "started": "2026-06-08T14:10:23.346Z",
  "work_dir": "/root/orca-blocks/.tmp/sysbox-stargz-iops/20260608T141023Z"
}
```

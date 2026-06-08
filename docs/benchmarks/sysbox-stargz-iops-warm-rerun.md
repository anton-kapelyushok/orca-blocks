# Sysbox stargz IOPS

Generated: `2026-06-08T14:08:39.721Z`

Each operation starts a fresh Sysbox container, starts containerd plus `containerd-stargz-grpc` inside it, then measures `ctr-remote images rpull` plus the container action. The local registry and eStargz image are prepared on the host.

| Runtime | Operation | Wall | rpull | Bench | Throughput | IOPS |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| sysbox-stargz | first_user_output | 10931 ms | 9886 ms |  |  |  |
| sysbox-stargz | sequential_read | 11634 ms | 10538 ms | 121 ms | 2110 MiB/s | 2110 |
| sysbox-stargz | random_read | 13709 ms | 11385 ms | 1550 ms | 165 MiB/s | 42257 |

```json
{
  "built": false,
  "esgz_image": "127.0.0.1:15002/orca-sysbox-disk-bench-esgz:size256m-rand65536-qd1",
  "normal_image": "127.0.0.1:15002/orca-sysbox-disk-bench-normal:size256m-rand65536-qd1",
  "optimized": false,
  "rows": [
    {
      "bench_duration_ms": "",
      "iops": "",
      "mb_per_sec": "",
      "operation": "first_user_output",
      "rpull_ms": 9886,
      "runtime": "sysbox-stargz",
      "wall_ms": 10931
    },
    {
      "bench_duration_ms": 121,
      "iops": 2110,
      "mb_per_sec": 2110,
      "operation": "sequential_read",
      "rpull_ms": 10538,
      "runtime": "sysbox-stargz",
      "wall_ms": 11634
    },
    {
      "bench_duration_ms": 1550,
      "iops": 42257,
      "mb_per_sec": 165,
      "operation": "random_read",
      "rpull_ms": 11385,
      "runtime": "sysbox-stargz",
      "wall_ms": 13709
    }
  ],
  "started": "2026-06-08T14:08:39.721Z",
  "work_dir": "/root/orca-blocks/.tmp/sysbox-stargz-iops/20260608T140839Z"
}
```

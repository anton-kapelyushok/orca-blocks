# Sysbox stargz IOPS

Generated: `2026-06-08T14:03:56.993Z`

Each operation starts a fresh Sysbox container, starts containerd plus `containerd-stargz-grpc` inside it, then measures `ctr-remote images rpull` plus the container action. The local registry and eStargz image are prepared on the host.

| Runtime | Operation | Wall | rpull | Bench | Throughput | IOPS |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| sysbox-stargz | first_user_output | 10735 ms | 9855 ms |  |  |  |
| sysbox-stargz | sequential_read | 10410 ms | 9552 ms | 98 ms | 2597 MiB/s | 2597 |
| sysbox-stargz | random_read | 10776 ms | 9371 ms | 669 ms | 382 MiB/s | 97870 |

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
      "rpull_ms": 9855,
      "runtime": "sysbox-stargz",
      "wall_ms": 10735
    },
    {
      "bench_duration_ms": 98,
      "iops": 2597,
      "mb_per_sec": 2597,
      "operation": "sequential_read",
      "rpull_ms": 9552,
      "runtime": "sysbox-stargz",
      "wall_ms": 10410
    },
    {
      "bench_duration_ms": 669,
      "iops": 97870,
      "mb_per_sec": 382,
      "operation": "random_read",
      "rpull_ms": 9371,
      "runtime": "sysbox-stargz",
      "wall_ms": 10776
    }
  ],
  "started": "2026-06-08T14:03:56.993Z",
  "work_dir": "/root/orca-blocks/.tmp/sysbox-stargz-iops/20260608T140356Z"
}
```

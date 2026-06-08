# Docker vs Orca Disk Benchmark

Run: 2026-06-08T11:19:13Z

| Field | Value |
| --- | --- |
| Image | `orca-disk-bench:fair-1780917429` |
| Size | 128 MiB |
| Sequential block | 1048576 bytes |
| Random block | 4096 bytes |
| Random ops | 4096 |
| IO depth | 1 |
| Orca CPU/RAM | 1 vCPU / 3072 MiB |
| Orca base image | `base-ed6cb803-1f1f-4fbc-9097-77f1f2027b07` |
| Orca env | `env-da6a67ab-3c13-44a6-8859-f26270ff4f50` |

Docker warm/page-cache results are intentionally excluded.

## Results

| Target | Cache state | Sequential | Random | Remote fetches | Cache misses |
| --- | --- | ---: | ---: | ---: | ---: |
| docker-cold | host page cache dropped | 490 MiB/s | 1638 IOPS | n/a | n/a |
| orca-node-1 | node-1 local cache | 46 MiB/s | 1243 IOPS | 39/26 | 39/26 |
| orca-node-2 | node-2 cold local cache | 49 MiB/s | 1014 IOPS | 40/26 | 40/26 |
| orca-node-2 | node-2 local cache after first read | 51 MiB/s | 1241 IOPS | 39/20 | 39/20 |

## Raw Rows

| Target | Mode | Cache state | MiB/s | IOPS | Duration | Remote fetches | Cache misses |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |
| docker-cold | sequential | host page cache dropped | 490 | 490 | 261 ms | n/a | n/a |
| docker-cold | random | host page cache dropped | 6 | 1638 | 2500 ms | n/a | n/a |
| orca-node-1 | sequential | node-1 local cache | 46 | 46 | 2752 ms | 39 | 39 |
| orca-node-2 | sequential | node-2 cold local cache | 49 | 49 | 2565 ms | 40 | 40 |
| orca-node-2 | sequential | node-2 local cache after first read | 51 | 51 | 2495 ms | 39 | 39 |
| orca-node-1 | random | node-1 local cache | 4 | 1243 | 3293 ms | 26 | 26 |
| orca-node-2 | random | node-2 cold local cache | 3 | 1014 | 4037 ms | 26 | 26 |
| orca-node-2 | random | node-2 local cache after first read | 4 | 1241 | 3299 ms | 20 | 20 |

## Takeaway

Sequential reads remain the main Orca gap in this run. Random reads are in the same order of magnitude as Docker cold-cache, while Docker warm/page-cache numbers are not used for comparison.

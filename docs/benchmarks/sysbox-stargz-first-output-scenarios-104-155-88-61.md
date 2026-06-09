# Sysbox stargz first-output scenarios

Generated: `2026-06-09T11:07:16.603Z`

Each scenario starts a fresh Sysbox container and waits until containerd plus `containerd-stargz-grpc` are online. The timer for `First output` starts only after the node is online and after any scenario prefetch is complete.

| Scenario | Node online prep | Prefetch | Measured rpull | First output |
| --- | ---: | ---: | ---: | ---: |
| no_image_present | 1642 ms |  | 8864 ms | 9476 ms |
| parent_present | 579 ms | 2069 ms | 9132 ms | 9672 ms |
| actual_present | 571 ms | 8642 ms |  | 566 ms |

```json
{
  "actual_esgz": "127.0.0.1:15002/orca-sysbox-actual-esgz:size256m",
  "actual_normal": "127.0.0.1:15002/orca-sysbox-actual-normal:size256m",
  "base_esgz": "127.0.0.1:15002/orca-sysbox-base-esgz:alpine-3.22",
  "base_normal": "127.0.0.1:15002/orca-sysbox-base-normal:alpine-3.22",
  "built_actual": true,
  "built_base": true,
  "optimized_actual": true,
  "optimized_base": true,
  "rows": [
    {
      "first_output_ms": 9476,
      "measured_rpull_ms": 8864,
      "node_online_ms": 1642,
      "scenario": "no_image_present"
    },
    {
      "first_output_ms": 9672,
      "measured_rpull_ms": 9132,
      "node_online_ms": 579,
      "parent_prefetch_ms": 2069,
      "scenario": "parent_present"
    },
    {
      "actual_prefetch_ms": 8642,
      "first_output_ms": 566,
      "measured_rpull_ms": "",
      "node_online_ms": 571,
      "scenario": "actual_present"
    }
  ],
  "started": "2026-06-09T11:07:16.603Z",
  "work_dir": "/tmp/sgfo/20260609T110716Z"
}
```

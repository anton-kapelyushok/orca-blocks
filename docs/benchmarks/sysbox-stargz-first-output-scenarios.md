# Sysbox stargz first-output scenarios

Generated: `2026-06-08T14:20:51.842Z`

Each scenario starts a fresh Sysbox container and waits until containerd plus `containerd-stargz-grpc` are online. The timer for `First output` starts only after the node is online and after any scenario prefetch is complete.

| Scenario | Node online prep | Prefetch | Measured rpull | First output |
| --- | ---: | ---: | ---: | ---: |
| no_image_present | 1993 ms |  | 9623 ms | 10506 ms |
| parent_present | 1891 ms | 2035 ms | 9425 ms | 10173 ms |
| actual_present | 811 ms | 9579 ms |  | 1312 ms |

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
      "first_output_ms": 10506,
      "measured_rpull_ms": 9623,
      "node_online_ms": 1993,
      "scenario": "no_image_present"
    },
    {
      "first_output_ms": 10173,
      "measured_rpull_ms": 9425,
      "node_online_ms": 1891,
      "parent_prefetch_ms": 2035,
      "scenario": "parent_present"
    },
    {
      "actual_prefetch_ms": 9579,
      "first_output_ms": 1312,
      "measured_rpull_ms": "",
      "node_online_ms": 811,
      "scenario": "actual_present"
    }
  ],
  "started": "2026-06-08T14:20:51.842Z",
  "work_dir": "/root/orca-blocks/.tmp/sysbox-stargz-first-output/20260608T142051Z"
}
```

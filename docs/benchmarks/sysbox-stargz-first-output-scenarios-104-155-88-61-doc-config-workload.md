# Sysbox stargz first-output scenarios

Generated: `2026-06-09T11:25:26.231Z`

Each scenario starts a fresh Sysbox container and waits until containerd plus `containerd-stargz-grpc` are online. The timer for `First output` starts only after the node is online and after any scenario prefetch is complete.

| Scenario | Node online prep | Prefetch | Measured rpull | First output |
| --- | ---: | ---: | ---: | ---: |
| no_image_present | 1649 ms |  | 8847 ms | 9467 ms |
| parent_present | 550 ms | 1770 ms | 7451 ms | 7982 ms |
| actual_present | 573 ms | 8656 ms |  | 558 ms |

```json
{
  "actual_esgz": "127.0.0.1:15003/orca-sysbox-actual-esgz:size256m",
  "actual_normal": "127.0.0.1:15003/orca-sysbox-actual-normal:size256m",
  "base_esgz": "127.0.0.1:15003/orca-sysbox-base-esgz:alpine-3.22",
  "base_normal": "127.0.0.1:15003/orca-sysbox-base-normal:alpine-3.22",
  "built_actual": true,
  "built_base": true,
  "optimize_args_json": "[\"echo ORCA_FIRST_USER_OUTPUT\"]",
  "optimize_entrypoint_json": "[\"/bin/sh\",\"-lc\"]",
  "optimized_actual": true,
  "optimized_base": true,
  "rows": [
    {
      "first_output_ms": 9467,
      "measured_rpull_ms": 8847,
      "node_online_ms": 1649,
      "scenario": "no_image_present"
    },
    {
      "first_output_ms": 7982,
      "measured_rpull_ms": 7451,
      "node_online_ms": 550,
      "parent_prefetch_ms": 1770,
      "scenario": "parent_present"
    },
    {
      "actual_prefetch_ms": 8656,
      "first_output_ms": 558,
      "measured_rpull_ms": "",
      "node_online_ms": 573,
      "scenario": "actual_present"
    }
  ],
  "stargz_doc_config": true,
  "started": "2026-06-09T11:25:26.231Z",
  "work_dir": "/tmp/sgfo-doc/20260609T112526Z"
}
```

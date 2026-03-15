# Stratum Protocol

## Default ports

- `v1`: `:5555`
- `v2`: `:5556` (only if `stratum_v2_enabled: true`)

## Worker formatz

- Use `wallet` or `wallet.worker`.
- Password is usually ignored (`x` is fine).

## v1 flow (JSON-RPC)

1. `mining.subscribe`
2. `mining.authorize`
3. Receive work via:
   - `mining.set_difficulty`
   - `mining.notify`
4. Submit shares via `mining.submit`.

If `extranonce_size > 0`, bridge can send `set_extranonce`.

## v2 flow (binary SV2 mining subset)

1. `SetupConnection`
2. `OpenStandardMiningChannel`
3. Receive:
   - `OpenStandardMiningChannelSuccess` (channel + initial target)
   - `SetTarget`
   - `NewMiningJob`
   - `SetNewPrevHash`
4. Submit with `SubmitSharesStandard`.

If `stratum_v2_fallback_to_v1: true`, JSON-RPC clients on the v2 port can fall back to v1 handling.

## VarDiff knobs (shared behavior for v1 and v2)

- `min_share_diff`
- `var_diff`
- `shares_per_min`
- `var_diff_retarget_time` (for example `30s`)
- `var_diff_stats`

## config keys

- `stratum_port`
- `stratum_v2_enabled`
- `stratum_v2_port`
- `stratum_v2_fallback_to_v1`
- `cryptix_address`
- `min_share_diff`
- `var_diff*`

## Errors

- `connection refused`: wrong port or listener disabled.
- `stale-share`: your job is old.
- `low-difficulty-share`: share does not meet current target.
- `wrong channel` (v2): wrong channel ID in submit.


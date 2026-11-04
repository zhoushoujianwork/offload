# Execution model evidence

Execution results and history expose `requested_model` and `observed_model`.
The existing `model` field retains its compatibility behavior.

`requested_model` records the effective request/profile model. `observed_model`
records the Claude assistant event model or the verified Codex session turn-context
model. A Claude `unrecognized_model` diagnostic invalidates its observation.
Missing observations remain empty; profile/configuration values are never used as
observational evidence. Native metadata identifies what the native client reports,
not independent verification of a gateway's upstream routing.

Embedding clients such as Agent-Host should display `observed_model` as the native
model. When unavailable, label `requested_model` as requested/unverified or omit
the actual-model label. These fields do not establish execution acceptance.

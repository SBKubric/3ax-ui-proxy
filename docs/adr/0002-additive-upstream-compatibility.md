---
status: accepted
---

# Additive-only changes to stay mergeable with upstream

The fork carries the full history of [coinman-dev/3ax-ui](https://github.com/coinman-dev/3ax-ui) and keeps merging its `main`, so every fork feature is judged by how it survives the next merge, not by how elegant it would be in a standalone codebase. Starting with the tunnel subscription (AWG/WG configs in the subscription, [spec](../spec/tunnel-subscription.md)), we decided that fork features are **additive only**: new files and functions instead of edits to existing ones, no renames, at most one small insertion point per upstream file (preferably inside a hunk the fork already owns), an additive database schema (new tables or nullable columns via AutoMigrate, never new fields on upstream models, so the fork's database still opens under an upstream binary), and unchanged behaviour and headers of existing public routes such as `/sub`, `/json`, `/clash`. Contributing the feature back upstream is explicitly not a goal; compatibility is for our own merges.

## Considered options

- **Add `SubId` to the upstream `TunnelClient` model.** Rejected: upstream's legacy↔merged field-coverage tests (`tunnel_migrate_test.go`, `awgwg_baseline_test.go`) would break after a merge, and every upstream edit to the model would conflict. A link table with an `ON DELETE CASCADE` foreign key gives the same semantics without touching the model.
- **Extend `SUBController`/`NewSUBController` with the new route.** Rejected: a 21-parameter constructor where every added parameter conflicts with the next upstream change. A separate controller registered in one block after it costs nothing.
- **Fold tunnel configs into `/sub`.** Rejected: it changes the behaviour of a public route every existing client app depends on; a new route under a new configurable prefix is invisible to them.

## Consequences

- Some duplication is accepted on purpose (own TTL cache instead of hooking `datagen`, own settings getters file, a DTO wrapper in the controller instead of a model field).
- Where the owner prefers a small edit in a quiet upstream file over a fully additive alternative (the `subId` field in the existing add/update client handlers), the spec records the additive fallback to switch to if merges start conflicting.
- The research note `docs/research/upstream-tunnel-subscription.md` (branch `research/upstream-tunnel-subscription`) lists upstream file churn and the files to avoid; refresh it before the next large fork feature.

## Exceptions

- **`inbounds.follow_chain`** (`model.Inbound.FollowChain`, [#139](https://github.com/SBKubric/sane-3x-ui/issues/139), [ADR 0005](0005-only-443-on-every-hop.md)): the flag that marks a chain-following inbound is a new column on the upstream `inbounds` table, not a table of its own. The additive option was a join table keyed by inbound id with `ON DELETE CASCADE`, as for `SubId` above. The owner chose the column: the fork has diverged from upstream far enough that a column on the model costs little at the next merge, and the flag belongs to the inbound — it travels with the inbound's API and export, and its lifetime is the inbound's, which a side table would have to imitate. The column is `NOT NULL DEFAULT false`, so an upstream binary opening the fork's database ignores it, and `UpdateInbound` copies it like the other form fields. If merges start to conflict on the model, the join table is the fallback.

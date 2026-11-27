# Sling and claimant exclusions

- **Claimant exclusions survive graph materialization.** Sling unions
  `gc.exclude_claimants` from the input convoy and its tracked beads onto the
  workflow root before creation. Every authored or synthesized step inherits
  the root's exclusions; attachments, fan-out fragments, and drain item roots
  inherit them before creation too. Preserve additional step-local exclusions.
  Both sequential and batch writes must carry exclusions at creation, and
  metadata decoders reject non-string `gc.exclude_claimants` values.

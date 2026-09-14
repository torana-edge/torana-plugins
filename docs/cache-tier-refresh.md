# Sticky cache-tier refreshes

Refresh-on-read decisions checkpoint at most once per tenth of their tier TTL.
Each checkpoint covers the next interval conservatively: an idle choice can
remain sticky for up to TTL/10 longer, but a live prefix never expires early
because its last read was between checkpoints. Refresh-write failures are
logged and the existing marker remains usable. Changing the provider cache
policy, including `refresh_on_read`, changes the state scope; absolute-TTL
policies cannot inherit refreshed decisions.

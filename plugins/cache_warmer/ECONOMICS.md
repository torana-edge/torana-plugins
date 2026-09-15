# When cache warming is worth a request

Warming can spend money without another user request. Use a bounded policy,
not an always-on assumption. Start with the [cache warmer setup](README.md);
[the tier selector](../cache_tier_selector/README.md) is a separate plugin that
changes an existing marker without sending refresh requests.

## The arithmetic that limits warming

A refresh costs one cache read over the prefix. Letting the entry lapse costs the
difference between a write and a read on the next turn. So refreshing is cheaper
than lapsing only while:

```
refreshes_spent  <  (write_rate / read_rate) - 1
```

**The prefix size cancels out.** This is a pure price ratio, independent of how
large the conversation is. For an illustrative 5-minute tier at a 12.5x
write-to-read ratio, that is about **11 refreshes — roughly 45 minutes** at a
240-second interval. Past that, warming has cost more than the cache miss it was
avoiding.

The consequence is worth stating plainly: **keeping a conversation warm
indefinitely does not converge on break-even, it diverges.** Warming must be
opt-in per conversation and bounded by a deadline or a refresh budget, never left
on as a global default.

For gaps longer than roughly half an hour, buying a longer tier once is cheaper
than holding a short one open. With illustrative write multipliers of 1.25 and 2.0, the 1-hour tier costs
`(2.0 - 1.25) = 0.75x` base extra to write, while refreshing the 5-minute tier
for that same hour costs about `1.5x` base — around twice as much.

The comparison below assumes a read multiplier of 0.1, those write
multipliers, and a return within the stated gap. It is not a current price
recommendation or a guarantee: if the user never returns, warming adds cost.

| Idle gap in this example | Lower-cost option |
|---|---|
| under 5 min | nothing — the native TTL covers it |
| 5–30 min | refresh the short tier |
| 30–60 min | buy the 1-hour tier once |
| over 60 min | accept the miss |

## Measure the result

Include every refresh's provider-reported input, output and cache usage in the
workload cost. A cache read alone is not evidence that a warmer caused it.
Compare with leaving the prefix alone under similar traffic and idle gaps.

The plugin stops at its deadline, break-even count, a cache-write response, or
an unresolved refresh outcome. An unknown outcome is not permission to retry
spending automatically. Configure the provider/model policy resource from
verified rates and refresh semantics; provider configuration alone does not
approve that resource. See [the setup guide](README.md#data-and-failure-behavior).

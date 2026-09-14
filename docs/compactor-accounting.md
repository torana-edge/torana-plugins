# Compactor accounting

Every successful model-service response is included in the batch's cost, even
if its content is empty or too long to use. Unknown usage declines the batch.
If no candidate is applied, the plugin emits no savings report: that endpoint
counts applied compactions and cannot price a zero-token-removal report.
The host independently records each model-service call and its reported usage
in the `plugin-egress` request feed, including paid unusable completions and
batches declined by the economic gate. Model-service input/cache usage must be
normalized into disjoint billable buckets by the host.

`keyword_compactor` also reports savings against model cost. Its manifest
therefore declares the required pricing resource named `target`. Operators must
bind that slot to the provider and model pricing for the request path where the
plugin runs. The host resolves the binding through `env.model_pricing`; plugin
activation fails when the required slot is absent. This binding is attribution
data only: the plugin remains deterministic and does not call a model service.

## Workload-level accounting

Summing applied-compaction net savings is not the net result for a workload:
that sum omits spend on batches which never applied. Any dashboard or experiment
claiming workload savings must reconcile the applied reports with **all**
summarizer egress for the same workload and time window:

```
workload net = applied gross savings - applied rewrite premiums
               - all summarizer egress costs
```

Use gross savings in that calculation. Subtracting all egress from the existing
applied **net** figure would charge successful summaries twice. Preserve the
provider, model, plugin/service attribution and pricing used for each attempt;
missing usage, pricing, or incomplete event retention makes the workload total
unknown. The bounded request feed alone is not a durable accounting ledger.

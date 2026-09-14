# Compactor accounting

Every successful model-service response is included in the batch's cost, even
if its content is empty or too long to use. Unknown usage declines the batch.
If no candidate is applied, the plugin emits no savings report: that endpoint
counts applied compactions and cannot price a zero-token-removal report.
The host independently records each model-service call and its reported usage
in the `plugin-egress` request feed, including paid unusable completions and
batches declined by the economic gate. Model-service input/cache usage must be
normalized into disjoint billable buckets by the host.

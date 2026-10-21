# Code standards

## Observability

- Instrument with the standard OpenTelemetry API rather than backend-specific clients. Configure the SDK, exporters, resources, and their lifecycle in startup/composition code.
- Centralize HTTP request observations in middleware. Record semantic business metrics at the actual operation boundary: count successful state changes only after transaction commit, and avoid double-counting retries or replays.
- Keep pure business rules and deterministic Temporal Workflow code free of telemetry side effects. Instrument Activities and execution boundaries instead.
- Use small, explicit instrumentation calls. Do not introduce speculative telemetry interfaces, provider/adapter frameworks, or an event bus to hide them.
- Use low-cardinality attributes such as route templates and bounded outcome values. Do not use task, run, workspace, user, or request IDs as metric attributes. Never include secrets, credentials, authorization headers, or user payloads in telemetry.
- Export asynchronously with bounded timeouts. Recording a metric must not wait for network export. Runtime observation or export failures must not change business outcomes; report failures through safe logs.
- Fail fast on explicitly invalid startup configuration. With no export endpoint configured, use no-op behavior without background collection or outbound traffic.
- Bound database sampling with a timeout and publish only successful samples. Omit failed or canceled samples rather than reusing stale values. Treat shared database backlog as an installation-wide snapshot, not an additive per-process count.
- Stop requests and workers before flushing and shutting down telemetry. Use a fresh, bounded shutdown context and keep dependencies needed by final collection available until it completes.
- Test metric meaning, units, buckets, attributes, and successful-operation boundaries. Cover retries/replays, no-op configuration, sampling and export failures, cancellation, and shutdown. Use in-memory readers or local receivers rather than public services.

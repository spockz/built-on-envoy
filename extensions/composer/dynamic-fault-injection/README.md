# Envoy Latency and Fault Distribution Simulation

An Envoy [dynamic module](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/advanced/dynamic_modules) **upstream HTTP filter** written in Go that injects latency and fault responses based on configurable percentile distributions.

This is similar to Envoy's built-in [fault injection filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/fault_filter.html) but adds support for **percentile-based latency distributions** with **per-status-code weighting** — allowing you to simulate realistic endpoint behavior including error rates, latency profiles, and load-dependent degradation.

**Key differentiator**: 
By operating as an **upstream filter on the cluster**, this module measures the actual upstream response time and only injects the *remaining* delay needed to reach the target distribution value. 
If the upstream is already slower than the target, no additional delay is added.

## Features

- **Percentile-based latency injection**: Configure latency distributions using flexible percentile notation (`p0.0`, `p50.0`, `p99.9`, `p100.0`)
- **Per-status-code distributions**: Define different latency profiles for different HTTP status codes (e.g., 200s are fast, 503s are slow)
- **Resolution-based weighting**: Use `resolution` as both the sampling accuracy and the relative weight for status code selection
- **Load-based behavior**: Configure different response profiles based on current RPS with smooth grey-zone transitions
- **Grey zone penalties**: Model degradation with spike detection, penalty multipliers, and recovery rates
- **Route matching**: Apply different fault configurations via prefix/exact path matching and header matching
- **First-match routing**: Endpoints are evaluated in order; first match wins
- **Upstream-aware timing**: Measures actual upstream latency and only adds the remaining delay to reach the target — avoids over-delaying when the upstream is naturally slow

## Comparison with Envoy's Built-in Fault Filter

| Feature | Built-in Fault Filter | This Module |
|---------|----------------------|-------------|
| Fixed delay | ✅ | ✅ (flat distribution) |
| Percentile distributions | ❌ | ✅ |
| Per-status-code distributions | ❌ | ✅ |
| Load-based degradation | ❌ | ✅ |
| HTTP abort | ✅ | ✅ |
| gRPC abort | ✅ | 🚧 (planned) |
| Header-controlled faults | ✅ | ❌ (route-based instead) |
| Response rate limiting | ✅ | ❌ |
| Per-route configuration | Via Envoy per-route config | Supported |
| Runtime configuration | ✅ | ❌ |
| Exact distribution over N requests | ❌ | ✅ (stateful distribution) |

## Usage 

### Delay all success responses and introduce a 1% failure rate that returns quickly

```sh
CONFIG=$(cat <<-END
{
	"endpoints": [
		{
			"match": {"prefix": "/"},
			"responses": [
				{
					"status": 200,
					"resolution": 1000,
					"distribution": {
						"p0.0": "30ms",
						"p100.0": "500ms"
					}
				},
				{
				  "status": 503,
				  "resolution": 10,
				  "distribution": {
			      "p0.0": "3ms",
					  "p100.0": "5ms"
					}
				}
			]
		}
	]
}
END
)
boe run \
    --extension dynamic-fault-injection \
    --log-level dynamic_modules:debug \
    --config "${CONFIG}"

❯ curl -v http://localhost:10000/status/200
> GET /status/200 HTTP/1.1
> Host: localhost:10000
> User-Agent: curl/8.21.0
> Accept: */*
>
* Request completely sent off
< HTTP/1.1 200 OK
< server: envoy
< date: Mon, 03 Aug 2026 12:22:42 GMT
< content-type: text/html; charset=utf-8
< access-control-allow-origin: *
< access-control-allow-credentials: true
< content-length: 0
< x-envoy-upstream-service-time: 0
< x-fault-injected-delay: 213.77ms
< x-fault-actual-upstream: 953.946µs
< x-fault-status: 200
< x-fault-added-delay: 212.816054ms

$> curl -v http://localhost:10000/status/503
> GET /status/503 HTTP/1.1
> Host: localhost:10000
> User-Agent: curl/8.21.0
> Accept: */*
>
* Request completely sent off
< HTTP/1.1 503 Service Unavailable
< server: envoy
< date: Mon, 03 Aug 2026 12:20:30 GMT
< content-type: text/html; charset=utf-8
< access-control-allow-origin: *
< access-control-allow-credentials: true
< content-length: 0
< x-envoy-upstream-service-time: 0
< x-fault-injected-delay: 414.14ms
< x-fault-actual-upstream: 896.86µs
< x-fault-status: 503
< x-fault-added-delay: 413ms
```

## Configuration

The filter supports two configuration shapes. Filter-level configuration supports a
`endpoints[]` selector for launchers that cannot express Envoy route-level filter configuration.
When configuration is supplied through Envoy's `typed_per_filter_config`, use the direct behavior
shape (`responses` or `load_based`) and let Envoy perform route matching. `endpoints[]` is rejected
in per-route configuration.

For example, a per-route configuration is:

```yaml
responses:
  - status: 200
    resolution: 1000
    distribution:
      p0.0: "5ms"
      p100.0: "200ms"
```

The route-specific value is placed under the filter name in Envoy:

```yaml
typed_per_filter_config:
  dynamic-fault-injection:
    "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilterPerRoute
    dynamic_module_config:
      name: composer
    filter_name: dynamic-fault-injection
    filter_config:
      "@type": type.googleapis.com/google.protobuf.StringValue
      value: |
        responses:
          - status: 200
            resolution: 1000
            distribution:
              p0.0: "5ms"
              p100.0: "200ms"
```

The filter-level `endpoints[]` configuration and the per-route direct configuration are separate
mechanisms. A route-level configuration replaces the filter-level behavior for that route.

If the upstream responds with a status code that has no configured behavior for the selected
endpoint/per-route config, the filter passes the response through without injecting latency and
still sets fault metadata headers: `x-fault-injected-delay: 0s`, `x-fault-added-delay: 0s`,
`x-fault-status: <upstream status>`, and `x-fault-actual-upstream: <measured duration>`.

The filter is configured as native YAML in the Envoy config using `google.protobuf.StringValue` as the `filter_config` type.
Envoy parses the YAML natively and serializes it as JSON to the module — no string escaping or `value: |` indirection needed.
Please find an overview of the possible fields below, followed by an actual example of how to configure the filter as a per cluster http filter.

### Configuration Fields

| Field | Description |
|-------|-------------|
| `endpoints` | Array of endpoint configurations. First match wins. |
| `endpoints[].match.prefix` | Match requests whose path starts with this prefix |
| `endpoints[].match.exact` | Match requests with exactly this path |
| `endpoints[].match.headers` | Array of header match conditions (all must match) |
| `endpoints[].responses` | Array of status-code distributions (weighted by resolution) |
| `endpoints[].responses[].status` | HTTP status code (100-599) |
| `endpoints[].responses[].resolution` | Weight for status selection AND number of pre-computed samples |
| `endpoints[].responses[].distribution` | Percentile-to-duration mapping |
| `endpoints[].load_based` | Load-sensitive behavior configuration |
| `endpoints[].load_based.healthy` | Behavior below the healthy RPS threshold |
| `endpoints[].load_based.tipping_point` | Behavior above the tipping point RPS |
| `endpoints[].load_based.grey_zone` | Transition parameters between healthy and tipping |
| `diagnostic` | Include the diagnostic `x-fault-worker-index` response header; defaults to `false` |

### Matching a Virtual Host, Method, and Path Template

#### Filter-Level (`endpoints[]`, not per-route)

The `endpoints[]` matcher supports only `prefix` and `exact` path matching.
It does not support URI templates directly (for example, `/foo/bar/{id}`).

The following `endpoints[]` example approximates `/foo/bar/{id}` by matching
the `/foo/bar/` prefix and adding host/method header checks:

```yaml
endpoints:
  - match:
      prefix: "/foo/bar/"
      headers:
        - name: "host"
          exact_match: "api.example.com"
        - name: ":method"
          exact_match: "GET"
    responses:
      - status: 200
        resolution: 100
        distribution:
          p0.0: "5ms"
          p100.0: "20ms"
```

If you need richer routing semantics, match at the Envoy route layer and pass
direct per-route behavior to this filter.

#### Per-Route (`typed_per_filter_config`)

Important details for Envoy setup:

- Configure this extension as an HTTP filter in the same
  `HttpConnectionManager` that owns the route.
- The key under `typed_per_filter_config` must match that HTTP filter
  instance `name`.

The following `HttpConnectionManager` excerpt contains the required filter
chain and route override. Its listener and cluster definitions are omitted; see
the [complete E2E-derived bootstrap](#appendix-complete-per-route-bootstrap)
for the full configuration.

```yaml
"@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
http_filters:
  - name: dynamic-fault-injection
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
      dynamic_module_config:
        name: composer
      filter_name: dynamic-fault-injection
      filter_config:
        "@type": type.googleapis.com/google.protobuf.StringValue
        value: "endpoints: []"
  - name: envoy.filters.http.router
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
route_config:
  virtual_hosts:
    - name: api
      domains: ["api.example.com"]
      routes:
        - match:
            prefix: "/foo/bar/"
            headers:
              - name: ":method"
                exact_match: "GET"
          typed_per_filter_config:
            dynamic-fault-injection:
              "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilterPerRoute
              dynamic_module_config:
                name: composer
              filter_name: dynamic-fault-injection
              filter_config:
                "@type": type.googleapis.com/google.protobuf.StringValue
                value: |
                  responses:
                    - status: 200
                      resolution: 100
                      distribution:
                        p0.0: "5ms"
                        p100.0: "20ms"
          route:
            cluster: upstream_service
```

### Percentile Keys

Percentile keys use the format `p<value>` where value is between 0 and 100:

| Key | Quantile |
|-----|----------|
| `p0.0` | 0th percentile (minimum) |
| `p25.0` | 25th percentile |
| `p50.0` | 50th percentile (median) |
| `p75.0` | 75th percentile |
| `p90.0` | 90th percentile |
| `p95.0` | 95th percentile |
| `p99.0` | 99th percentile |
| `p99.9` | 99.9th percentile |
| `p99.99` | 99.99th percentile |
| `p100.0` | 100th percentile (maximum) |

Distribution values must be non-decreasing (a higher percentile cannot have a shorter duration).

### Grey Zone Configuration

| Field | Description |
|-------|-------------|
| `penalty_base` | Base latency penalty at full grey zone position (e.g., "50ms") |
| `spike_threshold` | Grey zone position (0-1) above which spike behavior activates |
| `spike_penalty_duration` | How long a spike penalty persists (e.g., "2s") |
| `spike_penalty_multiplier` | Multiplier applied to base penalty during spikes |
| `recovery_rate` | Rate at which spike penalty decays (0-1) |

### Envoy Configuration Example

Tyically, the filter is configured as an **upstream HTTP filter** on the cluster, not on the listener.
The filter would be configured on each cluster as the behaviours for each cluster could differ even on serving the same resource.
However, for straightforward testing setups where each resource is uniquely served by a single cluster the configuration can be simplified by 
adding the filter to the listener and configurig all behaviour using specific matchers.

```yaml
static_resources:
  listeners:
    - address:
        socket_address: { address: 0.0.0.0, port_value: 10000 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress_http
                route_config:
                  virtual_hosts:
                    - name: local_route
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: simulated_backend }
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
    - name: simulated_backend
      connect_timeout: 5s
      type: strict_dns
      load_assignment:
        cluster_name: simulated_backend
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: 8080 }
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
          upstream_http_filters:
            - name: dynamic_modules/latency_fault
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                dynamic_module_config:
                  name: latency_fault_module
                  do_not_close: true
                filter_name: latency_fault
                filter_config:
                  "@type": type.googleapis.com/google.protobuf.StringValue
                  value: |
                     endpoints:
                       - match:
                           prefix: "/api/v1/slow-endpoint"
                         responses:
                           - status: 200
                             resolution: 900
                             distribution:
                               p0.0: "5ms"
                               p50.0: "10ms"
                               p90.0: "50ms"
                               p99.0: "200ms"
                               p100.0: "1s"
                           - status: 503
                             resolution: 100
                             distribution:
                               p0.0: "100ms"
                               p100.0: "500ms"
            - name: envoy.filters.http.upstream_codec
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec
```

> **Note**: The `envoy.filters.http.upstream_codec` filter must always be the last filter in the upstream HTTP filter chain.

## How It Works

### Upstream Filter Architecture

Unlike a traditional downstream HTTP filter that injects delay *before* the request reaches the upstream, this filter operates in the **upstream position**:

1. **On request** (`OnRequestHeaders`): Samples from the distribution, records the start time, and lets the request proceed to the upstream unmodified.
2. **On response** (`OnResponseHeaders`): Measures how long the upstream actually took, calculates `remaining = target - elapsed`, and:
    - If `remaining > 0`: delays the response by that amount before forwarding to the client
    - If `remaining <= 0`: the upstream was already slow enough — no additional delay
    - If sampled status is 4xx/5xx: overrides the response with a local error response

This means the client observes a total latency that matches the configured distribution, regardless of how fast or slow the actual upstream is.

For matched requests, the filter reads a process-global, atomically synchronized in-flight request
counter when request headers are processed and uses that request-entry value for load-based
sampling and response metadata. It then increments the counter. The counter is decremented when
the stream completes, with filter destruction as an idempotent cleanup fallback. Envoy runs its
workers as threads in one process, so the counter is shared across workers.

### Status Code Selection

Each endpoint has one or more response entries with a `resolution` that serves as both:
1. **Weight**: The probability of selecting that status code (proportional to total resolution)
2. **Accuracy**: The number of pre-computed latency samples for that status code's distribution

For example, with `resolution: 900` for status 200 and `resolution: 100` for status 503:
- 90% of requests will get a 200 response with latency from the 200 distribution
- 10% of requests will get a 503 abort with latency from the 503 distribution

This status code rewriting assumes that the upstream responses are always "good" (e.g. 200) responses and will overwrite them with one of the  bad responses (e.g. 503). 

### Latency Distribution

The stateful probability distribution is inspired by [distribution-calculator](https://github.com/spockz/distribution-calculator). Given a set of percentiles, it:

1. Pre-computes exactly `resolution` samples by interpolating between percentile boundaries
2. Shuffles and serves them in random order
3. Over a full cycle of `resolution` requests, the actual percentile distribution exactly matches the configured one

### Load-Based Behavior

When `load_based` is configured, the process-global active-request count is
passed as the current load value:
- Below `healthy.threshold_rps`: Uses the healthy response distribution
- Above `tipping_point.threshold_rps`: Uses the tipping point distribution
- Between the two (**grey zone**): Probabilistically mixes between healthy and tipping based on position, with optional penalty

### Grey Zone Transitions

In the grey zone, the filter:
1. Calculates position as `(currentRPS - healthyRPS) / (tippingRPS - healthyRPS)` (0.0 to 1.0)
2. Selects healthy or tipping distribution proportionally to position
3. Adds a base latency penalty scaled by position
4. If position exceeds `spike_threshold`, applies the spike multiplier for `spike_penalty_duration`
5. Decays the spike penalty at `recovery_rate` when position drops below threshold

## Response Headers

The filter adds response headers to indicate what was injected:

| Header | Description |
|--------|-------------|
| `x-fault-injected-delay` | Target duration from the distribution (e.g., "52.3ms") |
| `x-fault-actual-upstream` | Actual time the upstream took to respond |
| `x-fault-added-delay` | Additional delay injected (target - upstream), or `0s` when none was added |
| `x-fault-requests-in-flight` | Process-global in-flight matched request count observed at request entry, before the request is added |
| `x-fault-worker-index` | Envoy worker index that made the fault decision; only included when `diagnostic` is `true` |
| `x-fault-injected` | Set to "abort" when a non-2xx status was injected |
| `x-fault-status` | The status code selected by the distribution |

### Multi-worker active request test

The E2E test in `extensions/tests/e2e/dynamic_fault_injection_test.go` starts Envoy with
the `--concurrency 4` startup option, which creates four worker threads. It admits requests one at a
time through a held upstream gate while the filter holds every response for three seconds. Diagnostic
mode is enabled so the test can verify that requests reach at least two workers. The sorted request-entry
counts must contain exactly `0..N-1`, proving that requests handled by different workers observe the same
global counter before their own registration.

The scaled variant sends 2,000 requests to the fast `/status/200` endpoint and applies the same exact
request-entry count assertion while keeping all requests active during the three-second response hold.
The request upper bound can be overridden with
`TEST_DYNAMIC_FAULT_INJECTION_ACTIVE_REQUEST_COUNT_UPPER_BOUND`; it defaults to 2,000.

## Appendix: Complete Per-Route Bootstrap

The following full bootstrap is based on the per-route E2E test. Replace the
listener/admin ports, the upstream address, and the dynamic module search path
for your environment. The `ENVOY_DYNAMIC_MODULES_SEARCH_PATH` environment
variable must contain `libcomposer.so`.

```yaml
admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 9901 }
static_resources:
  listeners:
    - name: main
      address:
        socket_address: { address: 0.0.0.0, port_value: 10000 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress_http
                http_filters:
                  - name: dynamic-fault-injection
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config: { name: composer }
                      filter_name: dynamic-fault-injection
                      filter_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: "endpoints: []"
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: routes
                  virtual_hosts:
                    - name: default
                      domains: ["*"]
                      routes:
                        - match:
                            prefix: "/anything/"
                            headers:
                              - name: ":method"
                                exact_match: "GET"
                          typed_per_filter_config:
                            dynamic-fault-injection:
                              "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilterPerRoute
                              dynamic_module_config: { name: composer }
                              filter_name: dynamic-fault-injection
                              filter_config:
                                "@type": type.googleapis.com/google.protobuf.StringValue
                                value: |
                                  responses:
                                    - status: 200
                                      resolution: 100
                                      distribution:
                                        p0.0: "20ms"
                                        p100.0: "20ms"
                          route:
                            cluster: upstream_service
  clusters:
    - name: upstream_service
      type: STATIC
      load_assignment:
        cluster_name: upstream_service
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: 8080 }
```

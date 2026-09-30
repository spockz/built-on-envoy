// Copyright Built On Envoy
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package fault

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
)

// FilterConfig is the top-level configuration for the latency/fault filter.
type FilterConfig struct {
	Endpoints               []EndpointConfig     `yaml:"endpoints"`
	ProbabilityDistribution string               `yaml:"probability_distribution,omitempty"`
	Diagnostic              bool                 `yaml:"diagnostic,omitempty"`
	Responses               []StatusDistribution `yaml:"responses,omitempty"`
	LoadBased               *LoadBasedConfig     `yaml:"load_based,omitempty"`
}

// ConfigSource identifies where a configuration was supplied.
type ConfigSource int

const (
	// FilterConfigSource identifies filter-level configuration.
	FilterConfigSource ConfigSource = iota
	// PerRouteConfigSource identifies configuration attached to an Envoy route.
	PerRouteConfigSource
)

const (
	// ProbabilityDistributionStateful uses StatefulProbabilityDistribution.
	ProbabilityDistributionStateful = "stateful"
	// ProbabilityDistributionStateless uses ProbabilityDistribution.
	ProbabilityDistributionStateless = "stateless"
)

// EndpointConfig defines fault behavior for a matched endpoint.
type EndpointConfig struct {
	Match     MatchConfig          `yaml:"match"`
	Responses []StatusDistribution `yaml:"responses"`
	LoadBased *LoadBasedConfig     `yaml:"load_based,omitempty"`
}

// StatusDistribution defines a latency distribution for a specific HTTP status code.
// Resolution acts as both the relative weight for selecting this status code
// and the number of pre-computed samples in the stateful distribution.
type StatusDistribution struct {
	Status        int                  `yaml:"status"`
	Resolution    int                  `yaml:"resolution"`
	Distribution  map[string]string    `yaml:"distribution"`
	LocalResponse *LocalResponseConfig `yaml:"local_response,omitempty"`
}

// LocalResponseConfig customizes the body and headers of a forced response.
type LocalResponseConfig struct {
	Body    *string               `yaml:"body,omitempty"`
	Headers []LocalResponseHeader `yaml:"headers,omitempty"`
}

// LocalResponseHeader is one header pair on a forced response.
type LocalResponseHeader struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// LoadBasedConfig enables different response distributions based on in-flight request count.
type LoadBasedConfig struct {
	Healthy      *LoadTier       `yaml:"healthy"`
	TippingPoint *LoadTier       `yaml:"tipping_point"`
	GreyZone     *GreyZoneConfig `yaml:"grey_zone,omitempty"`
}

// LoadTier defines the response behavior at a specific load level.
type LoadTier struct {
	ThresholdInFlight float64              `yaml:"threshold_in_flight"`
	Responses         []StatusDistribution `yaml:"responses"`
}

// GreyZoneConfig controls behavior in the transition zone between healthy and tipping point.
type GreyZoneConfig struct {
	PenaltyBase            string  `yaml:"penalty_base"`
	SpikeThreshold         float64 `yaml:"spike_threshold"`
	SpikePenaltyDuration   string  `yaml:"spike_penalty_duration"`
	SpikePenaltyMultiplier float64 `yaml:"spike_penalty_multiplier"`
	RecoveryRate           float64 `yaml:"recovery_rate"`
}

// ParseConfig parses a filter configuration into a FilterConfig.
// Accepts both YAML and JSON input. When using google.protobuf.Struct as the
// filter_config type in Envoy, the config is received as JSON (which is valid YAML).
func ParseConfig(data []byte) (*FilterConfig, error) {
	return parseConfig(data, FilterConfigSource)
}

// ParsePerRouteConfig parses direct behavior configuration for an Envoy route.
// Route matching belongs to Envoy, so endpoint selectors are not accepted.
func ParsePerRouteConfig(data []byte) (*FilterConfig, error) {
	return parseConfig(data, PerRouteConfigSource)
}

func parseConfig(data []byte, source ConfigSource) (*FilterConfig, error) {
	var cfg FilterConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse filter config: %w", err)
	}
	if err := validateLocalResponseNodes(data); err != nil {
		return nil, fmt.Errorf("failed to parse filter config: %w", err)
	}
	if source == PerRouteConfigSource && hasTopLevelKey(data, "endpoints") {
		return nil, fmt.Errorf("endpoints cannot be used in per-route configuration; configure matching in Envoy routes")
	}

	// Default to stateful sampling when not explicitly configured.
	if cfg.ProbabilityDistribution == "" {
		cfg.ProbabilityDistribution = ProbabilityDistributionStateful
	}

	// Validate top-level options.
	validationErrors := []error{}
	if cfg.ProbabilityDistribution != ProbabilityDistributionStateful && cfg.ProbabilityDistribution != ProbabilityDistributionStateless {
		validationErrors = append(validationErrors, fmt.Errorf("probability_distribution must be one of %q or %q, got %q", ProbabilityDistributionStateful, ProbabilityDistributionStateless, cfg.ProbabilityDistribution))
	}

	if source == PerRouteConfigSource || len(cfg.Responses) > 0 || cfg.LoadBased != nil {
		if err := validateBehavior(cfg.Responses, cfg.LoadBased, "configuration"); err != nil {
			validationErrors = append(validationErrors, err)
		}
	}

	// Validate endpoints.
	for i, ep := range cfg.Endpoints {
		if err := validateBehavior(ep.Responses, ep.LoadBased, fmt.Sprintf("endpoint %d", i)); err != nil {
			validationErrors = append(validationErrors, err)
		}
	}

	if len(validationErrors) > 0 {
		return nil, errors.Join(validationErrors...)
	}

	return &cfg, nil
}

func validateBehavior(responses []StatusDistribution, loadBased *LoadBasedConfig, context string) error {
	validationErrors := []error{}
	if len(responses) == 0 && loadBased == nil {
		validationErrors = append(validationErrors, fmt.Errorf("%s: must have at least 'responses' or 'load_based' configured", context))
	}
	if len(responses) > 0 && loadBased != nil {
		validationErrors = append(validationErrors, fmt.Errorf("%s: must have *either one* 'responses' or 'load_based' configured", context))
	}
	for j, resp := range responses {
		if err := validateStatusDistribution(resp, fmt.Sprintf("%s response %d", context, j)); err != nil {
			validationErrors = append(validationErrors, err)
		}
	}
	if loadBased != nil {
		if err := validateLoadBased(loadBased, context); err != nil {
			validationErrors = append(validationErrors, err)
		}
	}
	return errors.Join(validationErrors...)
}

func hasTopLevelKey(data []byte, key string) bool {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil || len(node.Content) == 0 {
		return false
	}
	root := node.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return true
		}
	}
	return false
}

func validateStatusDistribution(sd StatusDistribution, context string) error {
	validationErrors := []error{}
	if sd.Status < 200 || sd.Status > 599 {
		validationErrors = append(validationErrors, fmt.Errorf("%s: invalid HTTP status code %d; supported status codes are 200-599", context, sd.Status))
	}
	if sd.Resolution <= 0 {
		validationErrors = append(validationErrors, fmt.Errorf("%s: resolution must be positive, got %d", context, sd.Resolution))
	}
	if len(sd.Distribution) == 0 {
		validationErrors = append(validationErrors, fmt.Errorf("%s: distribution must have at least one entry", context))
	}
	if _, err := ParsePercentileDistribution(sd.Distribution); err != nil {
		validationErrors = append(validationErrors, fmt.Errorf("%s: %w", context, err))
	}
	if sd.LocalResponse != nil {
		if sd.Status == 204 || sd.Status == 205 || sd.Status == 304 {
			if sd.LocalResponse.Body != nil && *sd.LocalResponse.Body != "" {
				validationErrors = append(validationErrors, fmt.Errorf("%s: local_response.body must be empty for status %d", context, sd.Status))
			}
		}
		contentTypes := 0
		for i, header := range sd.LocalResponse.Headers {
			if !httpguts.ValidHeaderFieldName(header.Name) || strings.HasPrefix(header.Name, ":") {
				validationErrors = append(validationErrors, fmt.Errorf("%s local_response header %d: invalid header name %q", context, i, header.Name))
				continue
			}
			name := strings.ToLower(header.Name)
			if name == "content-length" || name == "transfer-encoding" || name == "connection" || name == "keep-alive" || name == "proxy-connection" || name == "upgrade" || name == "trailer" || name == "te" || strings.HasPrefix(name, "x-fault-") {
				validationErrors = append(validationErrors, fmt.Errorf("%s local_response header %d: header %q is reserved", context, i, header.Name))
			}
			if !httpguts.ValidHeaderFieldValue(header.Value) {
				validationErrors = append(validationErrors, fmt.Errorf("%s local_response header %d: invalid value", context, i))
			}
			if name == "content-type" {
				contentTypes++
				if strings.TrimSpace(header.Value) == "" {
					validationErrors = append(validationErrors, fmt.Errorf("%s local_response content-type must not be empty", context))
				}
			}
		}
		if contentTypes > 1 {
			validationErrors = append(validationErrors, fmt.Errorf("%s local_response may configure content-type only once", context))
		}
	}
	return errors.Join(validationErrors...)
}

func validateLocalResponseNodes(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return nil
	}
	root := dereferenceYAMLNode(document.Content[0])
	var validationErrors []error
	visitResponses := func(node *yaml.Node) {
		node = dereferenceYAMLNode(node)
		if node == nil || node.Kind != yaml.SequenceNode {
			return
		}
		items, err := effectiveSequence(node)
		if err != nil {
			validationErrors = append(validationErrors, err)
			return
		}
		for _, response := range items {
			fields, err := effectiveMapping(response)
			if err != nil {
				validationErrors = append(validationErrors, err)
				continue
			}
			localResponse, exists := fields["local_response"]
			if !exists {
				continue
			}
			localFields, err := effectiveMapping(localResponse)
			if err != nil {
				validationErrors = append(validationErrors, fmt.Errorf("local_response: %w", err))
				continue
			}
			if body, bodyExists := localFields["body"]; bodyExists && !isYAMLString(body) {
				validationErrors = append(validationErrors, fmt.Errorf("line %d local_response.body must be a string", body.Line))
			}
			headers, exists := localFields["headers"]
			if !exists {
				continue
			}
			headerNodes, err := effectiveSequence(headers)
			if err != nil {
				validationErrors = append(validationErrors, fmt.Errorf("local_response.headers: %w", err))
				continue
			}
			for _, headerNode := range headerNodes {
				headerFields, err := effectiveMapping(headerNode)
				if err != nil {
					validationErrors = append(validationErrors, fmt.Errorf("line %d local_response.headers entry: %w", headerNode.Line, err))
					continue
				}
				for _, required := range []string{"name", "value"} {
					value, exists := headerFields[required]
					if !exists || !isYAMLString(value) {
						validationErrors = append(validationErrors, fmt.Errorf("line %d local_response.headers entries require string %s", headerNode.Line, required))
					}
				}
			}
		}
	}
	rootFields, err := effectiveMapping(root)
	if err != nil {
		return err
	}
	if responses, exists := rootFields["responses"]; exists {
		visitResponses(responses)
	}
	if loadBased, exists := rootFields["load_based"]; exists {
		visitLoadResponses(loadBased, visitResponses, &validationErrors)
	}
	if endpoints, exists := rootFields["endpoints"]; exists {
		endpointNodes, err := effectiveSequence(endpoints)
		if err != nil {
			validationErrors = append(validationErrors, err)
		} else {
			for _, endpoint := range endpointNodes {
				endpointFields, err := effectiveMapping(endpoint)
				if err != nil {
					validationErrors = append(validationErrors, err)
					continue
				}
				if responses, exists := endpointFields["responses"]; exists {
					visitResponses(responses)
				}
				if loadBased, exists := endpointFields["load_based"]; exists {
					visitLoadResponses(loadBased, visitResponses, &validationErrors)
				}
			}
		}
	}
	return errors.Join(validationErrors...)
}

func visitLoadResponses(node *yaml.Node, visit func(*yaml.Node), validationErrors *[]error) {
	fields, err := effectiveMapping(node)
	if err != nil {
		*validationErrors = append(*validationErrors, err)
		return
	}
	for _, tierName := range []string{"healthy", "tipping_point"} {
		tier, exists := fields[tierName]
		if !exists {
			continue
		}
		tierFields, err := effectiveMapping(tier)
		if err != nil {
			*validationErrors = append(*validationErrors, err)
			continue
		}
		if responses, exists := tierFields["responses"]; exists {
			visit(responses)
		}
	}
}

func effectiveMapping(node *yaml.Node) (map[string]*yaml.Node, error) {
	node = dereferenceYAMLNode(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("must be an object")
	}
	var decoded map[string]yaml.Node
	if err := node.Decode(&decoded); err != nil {
		return nil, err
	}
	fields := make(map[string]*yaml.Node, len(decoded))
	for key := range decoded {
		value := decoded[key]
		fields[key] = &value
	}
	return fields, nil
}

func effectiveSequence(node *yaml.Node) ([]*yaml.Node, error) {
	node = dereferenceYAMLNode(node)
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("must be an array")
	}
	var decoded []yaml.Node
	if err := node.Decode(&decoded); err != nil {
		return nil, err
	}
	items := make([]*yaml.Node, len(decoded))
	for i := range decoded {
		items[i] = &decoded[i]
	}
	return items, nil
}

func isYAMLString(node *yaml.Node) bool {
	node = dereferenceYAMLNode(node)
	return node != nil && node.Kind == yaml.ScalarNode && node.ShortTag() == "!!str"
}

func dereferenceYAMLNode(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func validateLoadBased(lb *LoadBasedConfig, context string) error {
	validationErrors := []error{}
	if lb.Healthy == nil {
		validationErrors = append(validationErrors, fmt.Errorf("%s: load_based.healthy is required", context))
	} else if lb.Healthy.ThresholdInFlight <= 0 {
		validationErrors = append(validationErrors, fmt.Errorf("%s: load_based.healthy.threshold_in_flight must be positive", context))
	}

	if lb.TippingPoint == nil {
		validationErrors = append(validationErrors, fmt.Errorf("%s: load_based.tipping_point is required", context))
	}

	if lb.TippingPoint != nil && lb.Healthy != nil {

		if lb.TippingPoint.ThresholdInFlight <= lb.Healthy.ThresholdInFlight {
			validationErrors = append(validationErrors, fmt.Errorf("%s: load_based.tipping_point.threshold_in_flight must be greater than healthy.threshold_in_flight", context))
		}
		if len(lb.Healthy.Responses) == 0 {
			validationErrors = append(validationErrors, fmt.Errorf("%s: load_based.healthy.responses must have at least one entry", context))
		}
		if len(lb.TippingPoint.Responses) == 0 {
			validationErrors = append(validationErrors, fmt.Errorf("%s: load_based.tipping_point.responses must have at least one entry", context))
		}
		for j, resp := range lb.Healthy.Responses {
			if err := validateStatusDistribution(resp, fmt.Sprintf("%s healthy response %d", context, j)); err != nil {
				validationErrors = append(validationErrors, err)
			}
		}
		for j, resp := range lb.TippingPoint.Responses {
			if err := validateStatusDistribution(resp, fmt.Sprintf("%s tipping_point response %d", context, j)); err != nil {
				validationErrors = append(validationErrors, err)
			}
		}
	}

	if lb.GreyZone != nil {
		if _, err := time.ParseDuration(lb.GreyZone.PenaltyBase); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("%s: grey_zone.penalty_base: %w", context, err))
		}
		if _, err := time.ParseDuration(lb.GreyZone.SpikePenaltyDuration); err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("%s: grey_zone.spike_penalty_duration: %w", context, err))
		}
		if lb.GreyZone.SpikeThreshold <= 0 || lb.GreyZone.SpikeThreshold >= 1 {
			validationErrors = append(validationErrors, fmt.Errorf("%s: grey_zone.spike_threshold must be between 0 and 1 exclusive", context))
		}
		if lb.GreyZone.SpikePenaltyMultiplier <= 0 {
			validationErrors = append(validationErrors, fmt.Errorf("%s: grey_zone.spike_penalty_multiplier must be positive", context))
		}
		if lb.GreyZone.RecoveryRate <= 0 || lb.GreyZone.RecoveryRate > 1 {
			validationErrors = append(validationErrors, fmt.Errorf("%s: grey_zone.recovery_rate must be between 0 exclusive and 1 inclusive", context))
		}
	}
	return errors.Join(validationErrors...)
}

// Percentile represents a quantile-duration pair in a distribution.
type Percentile struct {
	Quantile float64
	Duration time.Duration
}

// ParsePercentileDistribution parses a map of percentile keys (e.g., "p0.0", "p50.0", "p99.9")
// to duration strings into a sorted slice of Percentiles.
func ParsePercentileDistribution(dist map[string]string) ([]Percentile, error) {
	if len(dist) == 0 {
		return nil, fmt.Errorf("distribution must have at least one entry")
	}

	var result []Percentile
	for key, durStr := range dist {
		quantile, err := parsePercentileKey(key)
		if err != nil {
			return nil, err
		}
		dur, err := time.ParseDuration(durStr)
		if err != nil {
			return nil, fmt.Errorf("invalid duration for %q: %w", key, err)
		}
		if dur < 0 {
			return nil, fmt.Errorf("negative duration for %q: %v", key, dur)
		}
		result = append(result, Percentile{Quantile: quantile, Duration: dur})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Quantile < result[j].Quantile
	})

	// Validate that durations are non-decreasing.
	for i := 1; i < len(result); i++ {
		if result[i].Duration < result[i-1].Duration {
			return nil, fmt.Errorf("distribution values must be non-decreasing: p%.1f (%v) < p%.1f (%v)",
				result[i].Quantile*100, result[i].Duration,
				result[i-1].Quantile*100, result[i-1].Duration)
		}
	}

	return result, nil
}

// parsePercentileKey parses keys like "p0.0", "p50.0", "p99.9", "p100.0"
// into a quantile value between 0.0 and 1.0.
func parsePercentileKey(key string) (float64, error) {
	if !strings.HasPrefix(key, "p") {
		return 0, fmt.Errorf("percentile key must start with 'p', got %q", key)
	}
	numStr := key[1:]
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid percentile key %q: %w", key, err)
	}
	if val < 0 || val > 100 {
		return 0, fmt.Errorf("percentile key %q: value must be between 0 and 100", key)
	}
	return val / 100.0, nil
}

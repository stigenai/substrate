// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apivalidation

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/agent-substrate/substrate/internal/egresspolicy"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"k8s.io/apimachinery/pkg/api/operation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func ValidateCreateActorEgressPolicyRequest(ctx context.Context, req *ateapipb.CreateActorEgressPolicyRequest) field.ErrorList {
	return Validate_CreateActorEgressPolicyRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

func ValidateGetActorEgressPolicyRequest(ctx context.Context, req *ateapipb.GetActorEgressPolicyRequest) field.ErrorList {
	return Validate_GetActorEgressPolicyRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

func ValidateUpdateActorEgressPolicyRequest(ctx context.Context, req *ateapipb.UpdateActorEgressPolicyRequest) field.ErrorList {
	return Validate_UpdateActorEgressPolicyRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

func ValidateDeleteActorEgressPolicyRequest(ctx context.Context, req *ateapipb.DeleteActorEgressPolicyRequest) field.ErrorList {
	return Validate_DeleteActorEgressPolicyRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

func ValidateEgressPolicyUpdate(ctx context.Context, p *field.Path, newVal, oldVal *ateapipb.EgressPolicy) field.ErrorList {
	return Validate_EgressPolicy(ctx, operation.Operation{Type: operation.Update}, p, newVal, oldVal)
}

func ValidateCustom_CreateActorEgressPolicyRequest(_ context.Context, _ operation.Operation, p *field.Path, req, _ *ateapipb.CreateActorEgressPolicyRequest) field.ErrorList {
	return validateEgressPolicyParentAtespace(req.GetActor(), req.GetEgressPolicy(), p)
}

func ValidateCustom_UpdateActorEgressPolicyRequest(_ context.Context, _ operation.Operation, p *field.Path, req, _ *ateapipb.UpdateActorEgressPolicyRequest) field.ErrorList {
	return validateEgressPolicyParentAtespace(req.GetActor(), req.GetEgressPolicy(), p)
}

func validateEgressPolicyParentAtespace(actor *ateapipb.ObjectRef, policy *ateapipb.EgressPolicy, p *field.Path) field.ErrorList {
	if actor == nil || actor.Atespace == "" {
		return nil // regular DV will handle it
	}
	actorAtespace := actor.GetAtespace()
	if policy == nil || policy.Metadata == nil || policy.Metadata.Atespace == "" {
		return nil // regular DV will handle it
	}
	policyAtespace := policy.GetMetadata().GetAtespace()
	if actorAtespace != policyAtespace {
		return field.ErrorList{
			field.Invalid(p.Child("egress_policy", "metadata", "atespace"), policyAtespace, "must match actor.atespace"),
		}
	}
	return nil
}

func ValidateCustom_EgressPolicy_Metadata(_ context.Context, _ operation.Operation, root *field.Path, meta, _ *ateapipb.ResourceMetadata) field.ErrorList {
	if meta == nil || meta.Name == "" {
		return nil // regular DV will handle it
	}
	if meta.Name != "default" {
		return field.ErrorList{field.Invalid(root.Child("name"), meta.Name, `must be "default"`).WithOrigin("custom=default")}
	}
	return nil
}

// ValidateCustom_EgressPolicy_Rules rejects two rules that tie on a pattern
// and a port, whatever their protocols: the gateway would have no way to pick
// one. Two https rules tie only when they also share a path prefix or an exact
// path, and a rule with neither holds the empty prefix, so it ties with
// another such rule but not with a scoped one. An http or tls_passthrough rule has no
// path, so it ties with any other-protocol rule on the pattern and port.
// Defaults are applied before validation, so an http rule left on 80 and an
// https rule left on 443 never tie.
func ValidateCustom_EgressPolicy_Rules(_ context.Context, _ operation.Operation, p *field.Path, rules, _ []*ateapipb.EgressRule) field.ErrorList {
	type hostKey struct {
		pattern string
		port    portKey
	}
	type key struct {
		hostKey
		path pathKey
	}
	type match struct {
		rule int
		path *field.Path
	}
	var errs field.ErrorList
	httpsSeen := map[key]match{}     // https rules, by prefix
	httpsHost := map[hostKey]match{} // first https rule, whatever its prefix
	otherHost := map[hostKey]match{} // first http or tls_passthrough rule
	tie := func(path *field.Path, pattern string, prior match, port portKey, pk pathKey) *field.Error {
		return field.Invalid(path, pattern, fmt.Sprintf("ties with %s on %s%s", prior.path, port, pk.suffix()))
	}
	for i, rule := range rules {
		member, patterns, ports, pathKeys := ruleMatchFields(rule)
		if member == "" {
			continue // handled by the union check
		}
		for j, pattern := range patterns {
			path := p.Index(i).Child(member, "hostnames").Index(j)
			for _, port := range portKeysOf(ports) {
				hk := hostKey{pattern, port}
				cur := match{rule: i, path: path}
				// A repeat within one rule is reported by the set check.
				if member != "https" {
					if prior, ok := httpsHost[hk]; ok && prior.rule != i {
						errs = append(errs, tie(path, pattern, prior, port, pathKey{}))
					}
					if prior, ok := otherHost[hk]; !ok {
						otherHost[hk] = cur
					} else if prior.rule != i {
						errs = append(errs, tie(path, pattern, prior, port, pathKey{}))
					}
					continue
				}
				if prior, ok := otherHost[hk]; ok && prior.rule != i {
					errs = append(errs, tie(path, pattern, prior, port, pathKey{}))
				}
				if _, ok := httpsHost[hk]; !ok {
					httpsHost[hk] = cur
				}
				for _, pk := range pathKeys {
					k := key{hk, pk}
					if prior, ok := httpsSeen[k]; !ok {
						httpsSeen[k] = cur
					} else if prior.rule != i {
						errs = append(errs, tie(path, pattern, prior, port, pk))
					}
				}
			}
		}
	}
	return errs
}

// pathKey is one thing a rule matches a path on: all paths (the zero value),
// a prefix, or an exact path. Keys of different kinds never tie.
type pathKey struct {
	kind  pathKind
	value string
}

type pathKind int

const (
	pathAll pathKind = iota
	pathPrefix
	pathExact
)

func (k pathKey) suffix() string {
	switch k.kind {
	case pathPrefix:
		return fmt.Sprintf(" for path prefix %q", k.value)
	case pathExact:
		return fmt.Sprintf(" for path %q", k.value)
	}
	return ""
}

// ruleMatchFields is what a rule matches on: the union member, its hostnames,
// its ports, and its path keys, which are the zero key for a rule that matches
// every path. Everything is empty for a rule that sets no member.
func ruleMatchFields(rule *ateapipb.EgressRule) (member string, hostnames []string, ports *ateapipb.Ports, keys []pathKey) {
	allPaths := []pathKey{{kind: pathAll}}
	switch {
	case rule.GetHttp() != nil:
		return "http", rule.GetHttp().GetHostnames(), rule.GetHttp().GetPorts(), allPaths
	case rule.GetHttps() != nil:
		for _, prefix := range rule.GetHttps().GetPathPrefixes() {
			keys = append(keys, pathKey{pathPrefix, prefix})
		}
		for _, path := range rule.GetHttps().GetPaths() {
			keys = append(keys, pathKey{pathExact, path})
		}
		if len(keys) == 0 {
			keys = allPaths
		}
		return "https", rule.GetHttps().GetHostnames(), rule.GetHttps().GetPorts(), keys
	case rule.GetTlsPassthrough() != nil:
		return "tls_passthrough", rule.GetTlsPassthrough().GetHostnames(), rule.GetTlsPassthrough().GetPorts(), allPaths
	}
	return "", nil, nil, nil
}

// portKey is one thing a Ports matches: a port number, or every port. Its
// String reads as "port 443" or "every port" in messages.
type portKey struct {
	number int32
	all    bool
}

func portKeysOf(ports *ateapipb.Ports) []portKey {
	if ports.GetAll() != nil {
		return []portKey{{all: true}}
	}
	keys := make([]portKey, 0, len(ports.GetNumbers()))
	for _, n := range ports.GetNumbers() {
		keys = append(keys, portKey{number: n})
	}
	return keys
}

func (k portKey) String() string {
	if k.all {
		return "every port"
	}
	return fmt.Sprintf("port %d", k.number)
}

func ValidateCustom_HTTPRule_Hostnames(_ context.Context, _ operation.Operation, p *field.Path, patterns, _ []string) field.ErrorList {
	return validateHostnamePatterns(patterns, p)
}

func ValidateCustom_HTTPSRule_Hostnames(_ context.Context, _ operation.Operation, p *field.Path, patterns, _ []string) field.ErrorList {
	return validateHostnamePatterns(patterns, p)
}

// Each prefix is matched against the path before any "?", so it must be an
// absolute path with no query or fragment. It ends with "/" so that "/zach/"
// cannot match "/zachary/x".
func ValidateCustom_HTTPSRule_PathPrefixes(_ context.Context, _ operation.Operation, p *field.Path, prefixes, _ []string) field.ErrorList {
	var errs field.ErrorList
	for i, prefix := range prefixes {
		if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") || strings.ContainsAny(prefix, "?#") {
			errs = append(errs, field.Invalid(p.Index(i), prefix, `must start and end with "/" and contain no "?" or "#"`))
		}
	}
	return errs
}

// ValidateCustom_HTTPSRule_Paths checks each entry with the rule the gateway
// compiles with.
func ValidateCustom_HTTPSRule_Paths(_ context.Context, _ operation.Operation, p *field.Path, paths, _ []string) field.ErrorList {
	var errs field.ErrorList
	for i, path := range paths {
		if err := egresspolicy.CheckExactPath(path); err != nil {
			errs = append(errs, field.Invalid(p.Index(i), path, err.Error()))
		}
	}
	return errs
}

func ValidateCustom_TLSPassthroughRule_Hostnames(_ context.Context, _ operation.Operation, p *field.Path, patterns, _ []string) field.ErrorList {
	return validateHostnamePatterns(patterns, p)
}

func ValidateCustom_HttpRuleEffects(_ context.Context, _ operation.Operation, p *field.Path, effects, _ *ateapipb.HttpRuleEffects) field.ErrorList {
	var errs field.ErrorList
	if len(effects.GetReplaceHeaders()) == 0 {
		errs = append(errs, field.Required(p, "at least one effect must be specified"))
	}
	return errs
}

func ValidateCustom_HttpRuleEffects_ReplaceHeaders(_ context.Context, _ operation.Operation, p *field.Path, injections, _ []*ateapipb.CredentialHeader) field.ErrorList {
	var errs field.ErrorList
	seenHeaders := map[string]bool{}
	for i, inj := range injections {
		if inj == nil {
			continue // handled by DV
		}
		norm := strings.ToLower(inj.Header)
		if seenHeaders[norm] {
			errs = append(errs, field.Duplicate(p.Index(i).Child("header"), inj.Header))
		}
		seenHeaders[norm] = true
	}
	return errs
}

// Validation uses the parsers the egress gateway matches with, so what the
// API accepts and what the gateway can evaluate cannot drift apart.
func validateHostnamePatterns(patterns []string, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i, raw := range patterns {
		errs = append(errs, validateHostnamePattern(raw, p.Index(i))...)
	}
	return errs
}

func validateHostnamePattern(raw string, p *field.Path) field.ErrorList {
	if raw == "" {
		return field.ErrorList{field.Required(p, "")}
	}
	if _, err := egresspolicy.ParseHostnamePattern(raw); err != nil {
		return field.ErrorList{
			field.Invalid(p, raw, `must be a DNS hostname, optionally with a complete leftmost-label wildcard, or "*"`),
		}
	}
	return nil
}

func ValidateCustom_CredentialHeader_Header(_ context.Context, _ operation.Operation, p *field.Path, header, _ *string) field.ErrorList {
	if !validHeaderName(*header) {
		return field.ErrorList{
			field.Invalid(p, *header, "must be an HTTP header name"),
		}
	}
	return nil
}

func ValidateCustom_CredentialHeader_Prefix(_ context.Context, _ operation.Operation, p *field.Path, prefix, _ *string) field.ErrorList {
	if !validHeaderValue(*prefix) {
		return field.ErrorList{
			field.Invalid(p, *prefix, "must be a valid HTTP field value prefix"),
		}
	}
	return nil
}

func ValidateCustom_CredentialHeader_CredentialUri(_ context.Context, _ operation.Operation, p *field.Path, uri, _ *string) field.ErrorList {
	if !validCredentialURI(*uri) {
		return field.ErrorList{
			field.Invalid(p, *uri, "must be ate-secret://<provider-class>/<provider-name>/<provider-specific-tail>"),
		}
	}
	return nil
}

func validCredentialURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ate-secret" || u.Host == "" || u.Host != u.Hostname() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(validation.IsDNS1123Subdomain(u.Host)) != 0 {
		return false
	}
	escapedPath := u.EscapedPath()
	// Reject percent-encoding in the path of secret uri.
	if escapedPath != u.Path {
		return false
	}
	if !strings.HasPrefix(escapedPath, "/") || strings.HasSuffix(escapedPath, "/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	return true
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range []byte(value) {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, c := range []byte(value) {
		if c != '\t' && (c < ' ' || c == 0x7f) {
			return false
		}
	}
	return true
}

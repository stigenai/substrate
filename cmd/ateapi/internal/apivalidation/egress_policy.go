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

// ValidateCustom_EgressPolicy_Rules rejects two rules that tie on a pattern,
// a port and a path prefix, whatever their protocols: the gateway would have
// no way to pick one. A rule with no path prefixes holds the empty prefix, so
// it ties with another such rule but not with a prefixed one. Defaults are applied before validation, so an http rule left on 80
// and an https rule left on 443 never tie.
func ValidateCustom_EgressPolicy_Rules(_ context.Context, _ operation.Operation, p *field.Path, rules, _ []*ateapipb.EgressRule) field.ErrorList {
	type key struct {
		pattern string
		port    portKey
		prefix  string
	}
	type match struct {
		rule int
		path *field.Path
	}
	var errs field.ErrorList
	seen := map[key]match{}
	for i, rule := range rules {
		member, patterns, ports, prefixes := ruleMatchFields(rule)
		if member == "" {
			continue // handled by the union check
		}
		for j, pattern := range patterns {
			path := p.Index(i).Child(member, "hostnames").Index(j)
			for _, port := range portKeysOf(ports) {
				for _, prefix := range prefixes {
					k := key{pattern, port, prefix}
					prior, ok := seen[k]
					switch {
					case !ok:
						seen[k] = match{rule: i, path: path}
					case prior.rule != i: // a repeat within one rule is reported by the set check
						errs = append(errs, field.Invalid(path, pattern, fmt.Sprintf("ties with %s on %s%s", prior.path, port, prefixSuffix(prefix))))
					}
				}
			}
		}
	}
	return errs
}

// ruleMatchFields is what a rule matches on: the union member, its hostnames,
// its ports, and its path prefixes, which are [""] for a rule that matches
// every path. Everything is empty for a rule that sets no member.
func ruleMatchFields(rule *ateapipb.EgressRule) (member string, hostnames []string, ports *ateapipb.Ports, prefixes []string) {
	allPaths := []string{""}
	switch {
	case rule.GetHttp() != nil:
		return "http", rule.GetHttp().GetHostnames(), rule.GetHttp().GetPorts(), allPaths
	case rule.GetHttps() != nil:
		prefixes = rule.GetHttps().GetPathPrefixes()
		if len(prefixes) == 0 {
			prefixes = allPaths
		}
		return "https", rule.GetHttps().GetHostnames(), rule.GetHttps().GetPorts(), prefixes
	case rule.GetTlsPassthrough() != nil:
		return "tls_passthrough", rule.GetTlsPassthrough().GetHostnames(), rule.GetTlsPassthrough().GetPorts(), allPaths
	}
	return "", nil, nil, nil
}

func prefixSuffix(prefix string) string {
	if prefix == "" {
		return ""
	}
	return fmt.Sprintf(" for path prefix %q", prefix)
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
// absolute path with no query or fragment.
func ValidateCustom_HTTPSRule_PathPrefixes(_ context.Context, _ operation.Operation, p *field.Path, prefixes, _ []string) field.ErrorList {
	var errs field.ErrorList
	for i, prefix := range prefixes {
		if !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#") {
			errs = append(errs, field.Invalid(p.Index(i), prefix, `must start with "/" and contain no "?" or "#"`))
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

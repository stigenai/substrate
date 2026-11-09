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

package controlapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"

	"github.com/agent-substrate/substrate/internal/atelet"
	"github.com/agent-substrate/substrate/internal/credbundle"
	"github.com/agent-substrate/substrate/internal/installdefaults"
	"github.com/agent-substrate/substrate/internal/substratex509"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/lru"
)

// ErrNoAteletOnNode reports that the informer cache holds no atelet pod for
// the requested node — e.g. the atelet is restarting, or the node is gone.
// Retryable.
var ErrNoAteletOnNode = errors.New("no atelet pod found on node")

// AteletDialer handles gRPC connections to Atelet pods.
type AteletDialer struct {
	ateletIndexer cache.Indexer
	// mu serializes DialForAteletOnNode.
	mu sync.Mutex
	// ateletConns maps a node name to its cachedAteletConn.
	ateletConns *lru.Cache
	// dialCredentials builds the transport credentials used to dial a given
	// atelet, keyed on the atelet's expected pod UID. Production wires this to
	// per-atelet mTLS; tests can override it with insecure credentials.
	dialCredentials func(expectedPodUID string) (credentials.TransportCredentials, error)
}

// DialerOption customizes an AteletDialer built by NewAteletDialer.
type DialerOption func(*AteletDialer)

// WithDialCredentials overrides how transport credentials are built for a given
// atelet pod UID. Tests use it to reach a fake atelet over insecure transport
// while still exercising the real lookup, dial and connection-cache path.
func WithDialCredentials(build func(expectedPodUID string) (credentials.TransportCredentials, error)) DialerOption {
	return func(d *AteletDialer) { d.dialCredentials = build }
}

// NewAteletDialer creates a new AteletDialer. clientBundlePath and serverCAPath
// are used to build the per-atelet mTLS credentials used for every atelet
// connection, and ateletSPIFFEID is the identity those credentials expect on
// the atelet serving cert.
func NewAteletDialer(ateletIndexer cache.Indexer, ateletSPIFFEID, clientBundlePath, serverCAPath string, opts ...DialerOption) *AteletDialer {
	d := &AteletDialer{
		ateletIndexer: ateletIndexer,
		ateletConns:   newAteletConnCache(1024),
		dialCredentials: func(expectedPodUID string) (credentials.TransportCredentials, error) {
			tlsConfig, err := buildTLSConfig(ateletSPIFFEID, clientBundlePath, serverCAPath, expectedPodUID)
			if err != nil {
				return nil, err
			}
			return credentials.NewTLS(tlsConfig), nil
		},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// newAteletConnCache builds the atelet conn cache, which closes the
// connections it evicts. A conn pushed out of the LRU without Close is not
// reclaimed: grpc keeps most of its goroutines and buffers alive for the
// life of the process. Closing on eviction can fail an RPC still in flight
// on a conn that aged to the LRU tail, but that failure is visible and
// retryable, unlike the leak.
//
// TODO: Consider pool semantics instead of a cache: a conn evicted for
// capacity would drain — close only once its last in-flight RPC finishes
// (e.g. refcounted checkout/release) — rather than being closed out from
// under a caller. Worth revisiting if the retryable eviction failures show
// up in practice.
func newAteletConnCache(size int) *lru.Cache {
	return lru.NewWithEvictionFunc(size, func(_ lru.Key, value interface{}) {
		_ = value.(cachedAteletConn).conn.Close()
	})
}

// cachedAteletConn is a conn together with the atelet pod UID and IP it was
// dialed for.
type cachedAteletConn struct {
	podUID types.UID
	ip     string
	conn   *grpc.ClientConn
}

// DialForAteletOnNode resolves the single atelet pod on nodeName and dials it
// with per-atelet pod-UID-pinned credentials. The connection cache is keyed by
// node. An entry is valid only for the pod UID and IP it was dialed for: when
// either changes, or the atelet is gone, the old connection is closed.
func (d *AteletDialer) DialForAteletOnNode(nodeName string) (*grpc.ClientConn, error) {
	// The lock makes read, compare, replace and dial atomic, so concurrent
	// callers share one conn and never overwrite an entry without closing it.
	d.mu.Lock()
	defer d.mu.Unlock()

	matchingAtelets, err := d.ateletIndexer.ByIndex(byNode, nodeName)
	if err != nil {
		return nil, fmt.Errorf("while finding atelet on node %q: %w", nodeName, err)
	}

	if len(matchingAtelets) == 0 {
		d.closeEntry(nodeName, "no atelet pod")
		return nil, fmt.Errorf("%w: %q", ErrNoAteletOnNode, nodeName)
	}
	// During a pod replacement the old conn can still be valid, so keep it.
	if len(matchingAtelets) > 1 {
		return nil, fmt.Errorf("found %d atelet pods on node %q, expected 1", len(matchingAtelets), nodeName)
	}

	selectedAtelet := matchingAtelets[0].(*corev1.Pod)
	if len(selectedAtelet.Status.PodIPs) == 0 {
		d.closeEntry(nodeName, "atelet pod has no IP")
		return nil, fmt.Errorf("selected atelet %q has no assigned IPs", selectedAtelet.ObjectMeta.Namespace+"/"+selectedAtelet.ObjectMeta.Name)
	}
	podUID := selectedAtelet.ObjectMeta.UID
	ip := selectedAtelet.Status.PodIPs[0].IP

	if entryAny, ok := d.ateletConns.Get(nodeName); ok {
		entry := entryAny.(cachedAteletConn)
		if entry.podUID == podUID && entry.ip == ip {
			return entry.conn, nil
		}
		slog.Info("Replacing atelet connection",
			slog.String("node", nodeName),
			slog.String("old_pod_uid", string(entry.podUID)),
			slog.String("new_pod_uid", string(podUID)),
			slog.String("old_ip", entry.ip),
			slog.String("new_ip", ip))
		d.ateletConns.Remove(nodeName)
	}

	creds, err := d.dialCredentials(string(podUID))
	if err != nil {
		return nil, fmt.Errorf("while building atelet credentials: %w", err)
	}

	ateletConn, err := grpc.NewClient(
		net.JoinHostPort(ip, strconv.Itoa(atelet.DefaultPort)),
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, fmt.Errorf("while creating atelet gRPC client connection: %w", err)
	}

	d.ateletConns.Add(nodeName, cachedAteletConn{podUID: podUID, ip: ip, conn: ateletConn})

	return ateletConn, nil
}

// closeEntry removes and closes the cached conn for nodeName, if any. The
// caller must hold d.mu.
func (d *AteletDialer) closeEntry(nodeName, reason string) {
	entryAny, ok := d.ateletConns.Get(nodeName)
	if !ok {
		return
	}
	entry := entryAny.(cachedAteletConn)
	slog.Info("Closing atelet connection",
		slog.String("node", nodeName),
		slog.String("pod_uid", string(entry.podUID)),
		slog.String("ip", entry.ip),
		slog.String("reason", reason))
	d.ateletConns.Remove(nodeName)
}

func buildTLSConfig(ateletSPIFFEID, clientBundlePath, serverCAPath, expectedPodUID string) (*tls.Config, error) {
	trustDomain, err := spiffeid.TrustDomainFromString(installdefaults.AteletTrustDomain)
	if err != nil {
		return nil, fmt.Errorf("while parsing trust domain %q: %w", installdefaults.AteletTrustDomain, err)
	}
	bundle, err := x509bundle.Load(trustDomain, serverCAPath)
	if err != nil {
		return nil, fmt.Errorf("while loading CA bundle from %s: %w", serverCAPath, err)
	}
	expectedID, err := spiffeid.FromString(ateletSPIFFEID)
	if err != nil {
		return nil, fmt.Errorf("while parsing expected atelet SPIFFE ID %q: %w", ateletSPIFFEID, err)
	}

	verify, err := verifyAteletServerCert(bundle, expectedID, expectedPodUID)
	if err != nil {
		return nil, fmt.Errorf("while creating atelet server cert verifier: %w", err)
	}

	tlsConfig := tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: credbundle.ClientLoader(clientBundlePath),
		// Skip the default verification because the peer is dialed by IP and its
		// certificate has no DNS/IP SAN.
		InsecureSkipVerify: true,
		VerifyConnection:   verify,
	}

	return &tlsConfig, nil
}

func verifyAteletServerCert(bundle *x509bundle.Bundle, expectedID spiffeid.ID, expectedPodUID string) (func(tls.ConnectionState) error, error) {
	if expectedPodUID == "" {
		return nil, fmt.Errorf("expected pod UID must not be empty")
	}
	if expectedID.IsZero() {
		return nil, fmt.Errorf("expected pod spiffe ID must not be empty")
	}
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("server presented no certificate")
		}
		id, _, err := x509svid.Verify(cs.PeerCertificates, bundle)
		if err != nil {
			return fmt.Errorf("verifying server certificate chain: %w", err)
		}
		if id != expectedID {
			return fmt.Errorf("server SPIFFE ID %q does not match expected %q", id, expectedID)
		}

		leaf := cs.PeerCertificates[0]
		if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
			return fmt.Errorf("server certificate lacks the serverAuth extended key usage")
		}

		identity, err := substratex509.PodIdentityFromCertificate(leaf)
		if err != nil {
			return fmt.Errorf("failed to parse PodIdentity extension: %w", err)
		}
		if identity == nil {
			return fmt.Errorf("server certificate has no PodIdentity extension, expected pod UID %q", expectedPodUID)
		}
		if identity.PodUID != expectedPodUID {
			return fmt.Errorf("pod UID %q does not match expected %q", identity.PodUID, expectedPodUID)
		}

		return nil
	}, nil
}

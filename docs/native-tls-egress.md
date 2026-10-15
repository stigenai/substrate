# Private native TLS egress

This opt-in serves one operator-pinned DNS hostname and explicit port while
preserving the origin TLS certificate. It is intended for the isolated native
worker plane; it does not enable passthrough on ordinary gateways.

Configure atenet with `--mode=egress --atenet-dataplane=envoy
--egress-policy-cache-ttl=0 --native-tls-destination=HOST:PORT`. Do not configure
a credential provider. Configure the private Envoy inner listener with only a
`passthrough` verdict mapped to one TCP cluster whose static DNS address is the
same HOST:PORT. There must be no default chain, MITM, cleartext, ORIGINAL_DST or
dynamic forwarding cluster. The checked-in integration fixture supplies the
complete minimal listener and policy bridge; deployment inventory validation
must pin the address and restrict its network egress independently.

Each CONNECT verifies the actor certificate against the configured actor CA,
requires the name-based SPIFFE identity and the immutable `ActorIdentity` UID
extension to agree with the current RUNNING actor, and reads its current policy.
Only an explicit TLS_PASSTHROUGH rule with exactly that hostname and sole port
allows the tunnel. A certificate issued before the UID extension was added is
denied by this opt-in. Existing ordinary egress does not require the extension.
The CONNECT IP never controls the static upstream. The listener then requires
the allowed SNI and preserves the TLS bytes. No HTTP credentials are injected.

Authority is checked when a connection opens; this does not revoke an already
open TCP tunnel. The native API must independently authenticate and authorize
every request, and cleanup must terminate the actor. No new issuer, CA, global
RBAC scope, or shared gateway activation is included.

## Local verification

Run `hack/test-native-tls.sh` with Docker and the Go toolchain. It builds the
pinned Envoy image and a Linux test binary, then runs with no external network
and all capabilities dropped. It fails if the binary/module is missing.

The fixture uses actual TLS, Envoy, the Rust listener filter and production
ext_proc handler; only actor/policy control data is synthetic. It asserts exact
origin certificate bytes and response, then denies wrong actor, same-name UID
replacement, stopped actor, revoked/broad policy, wrong/missing SNI, wrong port,
cleartext and forged XFCC. A changed CONNECT IP still reaches only the fixed
origin. This is local transport evidence, not a hosted gVisor or cluster policy
qualification. Live image pins, isolated identities, Kubernetes network denials
and the actual native API trust chain remain separate release checks.

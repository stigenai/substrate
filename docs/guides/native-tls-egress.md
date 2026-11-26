# Native TLS egress

`--native-tls-destination=HOST:PORT` turns an egress gateway into a gateway for
one pinned TLS destination. The origin certificate reaches the actor unchanged,
and the gateway never terminates TLS or injects credentials.

Ordinary `tls_passthrough` relays to the name in the SNI. This mode instead
relays to a static address configured on the gateway, so an actor can name only
the one destination.

## Configure the gateway

Run atenet with `--mode=egress --atenet-dataplane=envoy
--egress-policy-cache-ttl=0 --native-tls-destination=HOST:PORT` and no
credential provider. HOST is a canonical DNS name, and the port is explicit.

Configure the inner listener with one `passthrough` chain that proxies to a TCP
cluster whose static DNS address is the same HOST:PORT. Leave out the default,
MITM, cleartext, dynamic forward proxy and original-destination chains.
`TestNativeTLSEnvoy` holds a complete minimal listener. Restrict the pod's
network egress to the same address as well.

## What each CONNECT checks

- The actor certificate chains to the actor-identity CA.
- The UID in the certificate's actor identity extension matches the current
  `RUNNING` actor, so a reused actor name is denied.
- The actor has a `tls_passthrough` rule with exactly that hostname and that one
  port. Wildcards, all ports and extra names deny.
- The CONNECT address is not used to pick the upstream.

Policy is read on every CONNECT. An open tunnel stays open after its policy
changes, so the origin API must authenticate each request itself.

## Test

`hack/test-native-tls.sh` builds the Envoy image and runs `TestNativeTLSEnvoy`
with no network access. The test checks that the origin certificate bytes arrive
unchanged and that wrong actors, stale UIDs, stopped actors, broad policies,
wrong or missing SNI, wrong ports, cleartext and forged XFCC headers are denied.

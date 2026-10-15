# Static isolated native worker registration

The isolated Fabriek candidate plane uses one Deployment with no WorkerPool CR
and no `ate.dev/worker-pool` label. Shared worker discovery therefore does not
register it. The operator must register its observed Pod identity in the private
API after checking the reviewed image, placement, Ready state and node handoff.

```sh
ATE_NAMESPACE=fabriek-native ATE_API_SERVICE_NAME=api \
  kubectl-ate --context gtr admin register-static-worker POD \
  --expected-pod-uid UID --expected-image IMAGE@sha256:DIGEST \
  --expected-version VERSION
```

The bounded command reads Kubernetes only and creates or replays that exact
worker in the private API. An endpoint override, unexpected existing inventory,
changed Pod, changed image or shared discovery label denies. The one Ready,
Running container must report the exact pinned manifest in its runtime ImageID;
the runtime's display Image may instead contain its configuration digest.
A changed container ID or image observation during registration also denies.
Use a direct platform manifest pin, not an unverified multi-platform index. The private API
must already have the matching service identity and private state. The command
does not create the namespace, Pods, capacity, actors or tasks, and never updates
or deletes registrations. Capacity is reported by the actual worker through its
node-bound atelet. The receipt only confirms registration, not availability.

An unknown create response or Pod change can leave a retained registration.
Inspect the exact retained worker and Pod before retrying; do not register a
replacement while an old worker or actor remains unresolved. A replay with the
same identity performs no second create. Ordinary controllers and registrations
are unchanged.

Local tests use the Kubernetes fake client and a typed Control test double to
exercise exact registration, replay, racing creation, retained lost responses,
Pod replacement, inventory conflicts, API failures and explicit private binding.
Actual private API identity, capacity reporting, PodIP TLS and the hosted sandbox
remain deployment qualification steps.

// Copyright 2026 The Agent Substrate Authors
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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/installdefaults"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const staticWorkerLabel = "fabriek.dev/static-worker"
const staticProfileLabel = "fabriek.dev/supervisor-profile"

var staticImagePattern = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)

// Static registration is an operator step, never a replacement controller. The
// worker reports its own capacity, runtimes and IPs through WorkerService, and
// the registration stays UNAVAILABLE until it is marked available.
type staticWorkerRegistration struct {
	namespace, pod, uid, node, image, version, pool string
}

type staticWorkerClient interface {
	WorkerGetter
	ListWorkers(context.Context, *ateapi.ListWorkersRequest, ...grpc.CallOption) (*ateapi.ListWorkersResponse, error)
	CreateWorker(context.Context, *ateapi.CreateWorkerRequest, ...grpc.CallOption) (*ateapi.Worker, error)
}

func (r staticWorkerRegistration) desired(pod *corev1.Pod) (*ateapi.Worker, error) {
	if r.namespace != "fabriek-native" || r.pool != "fabriek-native" || r.node != "gtr-150" || r.uid == "" || r.version == "" || !staticImagePattern.MatchString(r.image) {
		return nil, fmt.Errorf("static worker requires exact Fabriek namespace, pool, node, Pod UID, version and image digest")
	}
	if pod.Namespace != r.namespace || pod.Name != r.pod || string(pod.UID) != r.uid || pod.Spec.NodeName != r.node || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("static worker Pod identity or running state differs")
	}
	if _, ok := pod.Labels["ate.dev/worker-pool"]; ok {
		return nil, fmt.Errorf("static worker must not be discovered by the shared worker controller")
	}
	if pod.Labels[staticWorkerLabel] != r.pool || pod.Labels[staticProfileLabel] != "fabriek-native-v1" || pod.Spec.NodeSelector["ate.dev/substrate-version"] != r.version || pod.Spec.NodeSelector["kubernetes.io/hostname"] != r.node {
		return nil, fmt.Errorf("static worker placement or profile differs")
	}
	if pod.Spec.ServiceAccountName == "" {
		return nil, fmt.Errorf("static worker Pod has no service account")
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || pod.Spec.Containers[0].Name != "ateom" || pod.Spec.Containers[0].Image != r.image || pod.Spec.HostNetwork || pod.Spec.HostPID {
		return nil, fmt.Errorf("static worker container shape differs")
	}
	if len(pod.Status.ContainerStatuses) != 1 {
		return nil, fmt.Errorf("static worker has no unique container observation")
	}
	observed := pod.Status.ContainerStatuses[0]
	_, digest, _ := strings.Cut(r.image, "@")
	imageID := strings.TrimPrefix(strings.TrimPrefix(observed.ImageID, "docker-pullable://"), "containerd://")
	if observed.Name != "ateom" || !observed.Ready || observed.State.Running == nil || observed.ContainerID == "" || (imageID != r.image && imageID != digest) {
		return nil, fmt.Errorf("static worker running image or container identity differs")
	}
	if _, err := netip.ParseAddr(pod.Status.PodIP); err != nil {
		return nil, fmt.Errorf("static worker Pod has no valid IP")
	}
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return nil, fmt.Errorf("static worker Pod is not Ready")
	}
	return &ateapi.Worker{
		Metadata: &ateapi.ResourceMetadata{Name: r.uid}, WorkerNamespace: r.namespace, WorkerPool: r.pool,
		WorkerPod: r.pod, WorkerPodUid: r.uid, NodeName: r.node,
		WorkerSpiffeId: installdefaults.SPIFFEID(pod.Namespace, pod.Spec.ServiceAccountName),
		Labels:         map[string]string{staticProfileLabel: "fabriek-native-v1", staticWorkerLabel: r.pool},
	}, nil
}

// sameStaticWorker compares the identity a registration fixes. Status is the
// worker's own and is not compared.
func sameStaticWorker(a, b *ateapi.Worker) bool {
	return a.GetMetadata().GetName() == b.GetMetadata().GetName() && a.GetWorkerNamespace() == b.GetWorkerNamespace() && a.GetWorkerPool() == b.GetWorkerPool() && a.GetWorkerPod() == b.GetWorkerPod() && a.GetWorkerPodUid() == b.GetWorkerPodUid() && a.GetNodeName() == b.GetNodeName() && a.GetWorkerSpiffeId() == b.GetWorkerSpiffeId() && maps.Equal(a.GetLabels(), b.GetLabels())
}

func registerStaticWorker(ctx context.Context, pods typedcorev1.PodInterface, client staticWorkerClient, r staticWorkerRegistration) (*ateapi.Worker, error) {
	pod, err := pods.Get(ctx, r.pod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("observe static worker Pod: %w", err)
	}
	desired, err := r.desired(pod)
	if err != nil {
		return nil, err
	}
	workers, err := client.ListWorkers(ctx, &ateapi.ListWorkersRequest{Options: &ateapi.ListOptions{PageSize: 100}})
	if err != nil {
		return nil, fmt.Errorf("observe private worker inventory: %w", err)
	}
	if workers.GetNextPageToken() != "" || len(workers.GetWorkers()) > 1 {
		return nil, fmt.Errorf("private API contains an unexpected worker inventory")
	}
	for _, existing := range workers.GetWorkers() {
		if !sameStaticWorker(existing, desired) {
			return nil, fmt.Errorf("private API already retains a different worker; replacement requires reconciliation")
		}
	}
	existing, err := client.GetWorker(ctx, &ateapi.GetWorkerRequest{Worker: &ateapi.ObjectRef{Name: r.uid}})
	if status.Code(err) == codes.NotFound {
		existing, err = client.CreateWorker(ctx, &ateapi.CreateWorkerRequest{Worker: desired})
		if status.Code(err) == codes.AlreadyExists {
			existing, err = client.GetWorker(ctx, &ateapi.GetWorkerRequest{Worker: &ateapi.ObjectRef{Name: r.uid}})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("register static worker; inspect retained state before retry: %w", err)
	}
	if !sameStaticWorker(existing, desired) {
		return nil, fmt.Errorf("retained worker differs from exact observed Pod")
	}
	after, err := pods.Get(ctx, r.pod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reobserve registered worker Pod; registration may be retained: %w", err)
	}
	observed, err := r.desired(after)
	if err != nil || !sameStaticWorker(observed, desired) || after.Status.ContainerStatuses[0].ContainerID != pod.Status.ContainerStatuses[0].ContainerID || after.Status.ContainerStatuses[0].ImageID != pod.Status.ContainerStatuses[0].ImageID {
		return nil, fmt.Errorf("pod changed during registration; retained worker requires reconciliation")
	}
	return existing, nil
}

func newRegisterStaticWorkerCmd() *cobra.Command {
	r := staticWorkerRegistration{namespace: "fabriek-native", node: "gtr-150", pool: "fabriek-native"}
	cmd := &cobra.Command{Use: "register-static-worker POD", Short: "Register one observed Fabriek worker without creating capacity", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&r.uid, "expected-pod-uid", "", "Exact observed Pod UID")
	cmd.Flags().StringVar(&r.image, "expected-image", "", "Exact reviewed worker image digest")
	cmd.Flags().StringVar(&r.version, "expected-version", "", "Exact reviewed node version label")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		r.pod = args[0]
		if os.Getenv(ateclient.NamespaceEnv) != r.namespace || os.Getenv(ateclient.APIServiceEnv) != "api" || endpoint != "" {
			return usageErrorf("static registration requires explicit ATE_NAMESPACE=fabriek-native and ATE_API_SERVICE_NAME=api with no endpoint override")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		cmd.SetContext(ctx)
		kube, err := newKubeClient(cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		defer client.Close()
		worker, err := registerStaticWorker(ctx, kube.CoreV1().Pods(r.namespace), client, r)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"worker": worker.GetMetadata().GetName(), "pod_uid": worker.GetWorkerPodUid(), "node": worker.GetNodeName(), "capacity": "must be reported by the worker", "registration": "retained"})
	}
	return cmd
}

func init() { adminCmd.AddCommand(newRegisterStaticWorkerCmd()) }

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

package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type staticRegistry struct {
	worker                     *ateapipb.Worker
	listErr, getErr, createErr error
	other                      bool
	created                    int
	race                       bool
	beforeCreate               func()
}

func (f *staticRegistry) ListWorkers(context.Context, *ateapipb.ListWorkersRequest, ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	out := &ateapipb.ListWorkersResponse{}
	if f.worker != nil {
		out.Workers = append(out.Workers, proto.CloneOf(f.worker))
	}
	if f.other {
		out.Workers = append(out.Workers, &ateapipb.Worker{Metadata: &ateapipb.ResourceMetadata{Name: "other"}})
	}
	return out, f.listErr
}
func (f *staticRegistry) GetWorker(context.Context, *ateapipb.GetWorkerRequest, ...grpc.CallOption) (*ateapipb.Worker, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.worker == nil {
		return nil, status.Error(codes.NotFound, "absent")
	}
	return proto.CloneOf(f.worker), nil
}
func (f *staticRegistry) CreateWorker(_ context.Context, req *ateapipb.CreateWorkerRequest, _ ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.created++
	if f.beforeCreate != nil {
		f.beforeCreate()
	}
	f.worker = proto.CloneOf(req.Worker)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.race {
		return nil, status.Error(codes.AlreadyExists, "raced")
	}
	return proto.CloneOf(f.worker), nil
}
func staticFixture() (staticWorkerRegistration, *corev1.Pod) {
	r := staticWorkerRegistration{namespace: "fabriek-native", pool: "fabriek-native", pod: "candidate-abc", uid: "172842af-179a-4a00-a90f-45a672555986", node: "gtr-150", version: "fabriek-18a54c37", image: "registry/ateom@sha256:" + strings.Repeat("a", 64)}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.namespace, Name: r.pod, UID: types.UID(r.uid), Labels: map[string]string{staticWorkerLabel: r.pool, staticProfileLabel: "fabriek-native-v1"}}, Spec: corev1.PodSpec{NodeName: r.node, NodeSelector: map[string]string{"ate.dev/substrate-version": r.version, "kubernetes.io/hostname": r.node}, Containers: []corev1.Container{{Name: "ateom", Image: r.image}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ateom", Ready: true, Image: r.image, ImageID: r.image, ContainerID: "containerd://observed", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	return r, p
}
func TestStaticRegistrationCreatesOnlyExactWorkerAndReplays(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "racing_create"}[race], func(t *testing.T) {
			r, p := staticFixture()
			k := fake.NewClientset(p)
			f := &staticRegistry{race: race}
			for range 2 {
				w, err := registerStaticWorker(t.Context(), k.CoreV1().Pods(r.namespace), f, r)
				if err != nil {
					t.Fatal(err)
				}
				if w.GetWorkerPodUid() != r.uid || w.GetStatus() != nil {
					t.Fatalf("unexpected capacity or identity: %v", w)
				}
			}
			if f.created != 1 {
				t.Fatalf("created %d times", f.created)
			}
			for _, a := range k.Actions() {
				if a.GetVerb() != "get" {
					t.Fatalf("unexpected Kubernetes mutation: %v", a)
				}
			}
		})
	}
}

func TestStaticRegistrationAcceptsRuntimeConfigDisplayWithExactManifest(t *testing.T) {
	r, p := staticFixture()
	// containerd reports the config digest as Image while ImageID retains the
	// reviewed manifest reference. Only the latter identifies the running image.
	p.Status.ContainerStatuses[0].Image = "sha256:" + strings.Repeat("b", 64)
	k := fake.NewClientset(p)
	f := &staticRegistry{}
	for range 2 {
		w, err := registerStaticWorker(t.Context(), k.CoreV1().Pods(r.namespace), f, r)
		if err != nil {
			t.Fatal(err)
		}
		if w.GetWorkerPodUid() != r.uid || w.GetStatus() != nil {
			t.Fatalf("unexpected capacity or identity: %v", w)
		}
	}
	if f.created != 1 {
		t.Fatalf("created %d times", f.created)
	}
}

func TestStaticRegistrationRejectsPodDriftBeforeCreate(t *testing.T) {
	cases := map[string]func(*corev1.Pod){
		"stale_ready_image": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].Image = "old-image"
			p.Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("b", 64)
		},
		"wrong_runtime_digest": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("b", 64) },
		"no_container_status":  func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"unready_container":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = false },
		"stopped_container":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running = nil },
		"missing_container_id": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" },
		"uid":                  func(p *corev1.Pod) { p.UID = "replacement" }, "node": func(p *corev1.Pod) { p.Spec.NodeName = "gtr-151" },
		"unready": func(p *corev1.Pod) { p.Status.Conditions = nil }, "ip": func(p *corev1.Pod) { p.Status.PodIP = "" },
		"deleting":   func(p *corev1.Pod) { n := metav1.Now(); p.DeletionTimestamp = &n },
		"discovered": func(p *corev1.Pod) { p.Labels["ate.dev/worker-pool"] = "old" },
		"profile":    func(p *corev1.Pod) { delete(p.Labels, staticProfileLabel) }, "image": func(p *corev1.Pod) { p.Spec.Containers[0].Image = "latest" },
		"sidecar":    func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "extra"}) },
		"oldversion": func(p *corev1.Pod) { p.Spec.NodeSelector["ate.dev/substrate-version"] = "old" },
		"hostnet":    func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "hostpid": func(p *corev1.Pod) { p.Spec.HostPID = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, p := staticFixture()
			mutate(p)
			f := &staticRegistry{}
			k := fake.NewClientset(p)
			if _, err := registerStaticWorker(t.Context(), k.CoreV1().Pods(r.namespace), f, r); err == nil {
				t.Fatal("accepted drift")
			}
			if f.created != 0 {
				t.Fatal("created worker after failed observation")
			}
		})
	}
}
func TestStaticRegistrationDoesNotCreateAfterReadFailureOrReplacement(t *testing.T) {
	for _, name := range []string{"list_denied", "get_unavailable", "replacement"} {
		t.Run(name, func(t *testing.T) {
			r, p := staticFixture()
			k := fake.NewClientset(p)
			f := &staticRegistry{}
			switch name {
			case "list_denied":
				f.listErr = status.Error(codes.PermissionDenied, "denied")
			case "get_unavailable":
				f.getErr = status.Error(codes.Unavailable, "unavailable")
			case "replacement":
				f.other = true
			}
			if _, err := registerStaticWorker(t.Context(), k.CoreV1().Pods(r.namespace), f, r); err == nil {
				t.Fatal("accepted failure")
			}
			if f.created != 0 {
				t.Fatal("created worker")
			}
		})
	}
}
func TestStaticRegistrationRetainsUnknownCreateAndChangedPod(t *testing.T) {
	for _, name := range []string{"lost_response", "changed_pod", "changed_container", "changed_running_image"} {
		t.Run(name, func(t *testing.T) {
			r, p := staticFixture()
			k := fake.NewClientset(p)
			f := &staticRegistry{}
			if name == "lost_response" {
				f.createErr = status.Error(codes.DeadlineExceeded, "unknown")
			} else {
				f.beforeCreate = func() {
					k.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
						q := p.DeepCopy()
						switch name {
						case "changed_container":
							q.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
						case "changed_running_image":
							q.Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("c", 64)
						default:
							q.UID = "replacement"
						}
						return true, q, nil
					})
				}
			}
			if _, err := registerStaticWorker(t.Context(), k.CoreV1().Pods(r.namespace), f, r); err == nil {
				t.Fatal("reported successful registration")
			}
			if f.created != 1 {
				t.Fatalf("create calls=%d", f.created)
			}
			if f.worker == nil {
				t.Fatal("unknown creation was not retained")
			}
			if name == "lost_response" {
				f.createErr = nil
				if _, err := registerStaticWorker(t.Context(), k.CoreV1().Pods(r.namespace), f, r); err != nil {
					t.Fatal(err)
				}
				if f.created != 1 {
					t.Fatal("replayed uncertain create")
				}
			}
			for _, a := range k.Actions() {
				if a.GetVerb() != "get" {
					t.Fatal("automatic cleanup attempted")
				}
			}
		})
	}
}
func TestStaticRegistrationRequiresExplicitPrivateAPI(t *testing.T) {
	t.Setenv(ateclient.NamespaceEnv, "ate-system")
	t.Setenv(ateclient.APIServiceEnv, "api")
	cmd := newRegisterStaticWorkerCmd()
	cmd.SetArgs([]string{"candidate"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "explicit ATE_NAMESPACE") {
		t.Fatalf("private binding not enforced: %v", err)
	}
}

func TestStaticRegistrationRejectsEndpointOverride(t *testing.T) {
	t.Setenv(ateclient.NamespaceEnv, "fabriek-native")
	t.Setenv(ateclient.APIServiceEnv, "api")
	original := endpoint
	endpoint = "shared-api:443"
	defer func() { endpoint = original }()
	cmd := newRegisterStaticWorkerCmd()
	cmd.SetArgs([]string{"candidate"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no endpoint override") {
		t.Fatalf("endpoint override not denied: %v", err)
	}
}

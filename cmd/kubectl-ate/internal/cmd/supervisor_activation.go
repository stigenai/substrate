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
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/supervisorbootstrap"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/api/resource"
)

var supervisorAtespace string

var getSupervisorActivationCmd = &cobra.Command{
	Use:   "supervisor-activation NAME",
	Short: "Read a protected supervisor's current public identity as JSON",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if supervisorAtespace == "" || outputFmt != "json" {
			return errors.New("supervisor-activation requires --atespace and --output=json")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		client, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return errors.New("supervisor activation connection unavailable")
		}
		defer client.Close()
		return writeSupervisorObservation(ctx, client, &ateapipb.ObjectRef{Atespace: supervisorAtespace, Name: args[0]}, cmd.OutOrStdout())
	},
}

type supervisorReader interface {
	GetActor(context.Context, *ateapipb.GetActorRequest, ...grpc.CallOption) (*ateapipb.Actor, error)
	GetActorTemplate(context.Context, *ateapipb.GetActorTemplateRequest, ...grpc.CallOption) (*ateapipb.ActorTemplate, error)
}

type supervisorObservation struct {
	SchemaVersion string                         `json:"schema_version"`
	Atespace      string                         `json:"atespace"`
	ActorName     string                         `json:"actor_name"`
	ActorUID      string                         `json:"actor_uid"`
	State         string                         `json:"state"`
	Assignment    supervisorAssignment           `json:"assignment"`
	Activation    *ateapipb.SupervisorActivation `json:"activation"`
	Template      supervisorTemplate             `json:"template"`
}

type supervisorAssignment struct {
	WorkerUID string `json:"worker_uid"`
	NodeName  string `json:"node_name"`
}

type supervisorTemplate struct {
	Atespace       string            `json:"atespace"`
	Name           string            `json:"name"`
	UID            string            `json:"uid"`
	Digest         string            `json:"digest"`
	ResourceLimits map[string]string `json:"resource_limits"`
}

// Only public identity is emitted. RPC and validation errors deliberately omit
// remote messages and template values, which may contain credentials.
func writeSupervisorObservation(ctx context.Context, client supervisorReader, ref *ateapipb.ObjectRef, out io.Writer) error {
	request := &ateapipb.GetActorRequest{Actor: ref}
	first, err := client.GetActor(ctx, request)
	if err != nil || !matchesSupervisorActor(first, ref) {
		return errors.New("supervisor actor unavailable")
	}
	templateRef := first.GetActorTemplate()
	template, err := client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: templateRef})
	if err != nil || template.GetMetadata().GetAtespace() != templateRef.GetAtespace() || template.GetMetadata().GetName() != templateRef.GetName() {
		return errors.New("supervisor template unavailable")
	}
	observation, err := describeSupervisor(first, template)
	if err != nil {
		return err
	}
	last, err := client.GetActor(ctx, request)
	if err != nil || !proto.Equal(first, last) {
		return errors.New("supervisor actor changed during observation")
	}
	encoded, err := json.Marshal(observation)
	if err != nil || len(encoded) > 16384 {
		return errors.New("supervisor observation unavailable")
	}
	_, err = out.Write(append(encoded, '\n'))
	return err
}

func matchesSupervisorActor(actor *ateapipb.Actor, ref *ateapipb.ObjectRef) bool {
	m := actor.GetMetadata()
	t := actor.GetActorTemplate()
	return ref.GetAtespace() != "" && ref.GetName() != "" && m.GetAtespace() == ref.GetAtespace() && m.GetName() == ref.GetName() && m.GetUid() != "" && m.GetVersion() > 0 && t.GetAtespace() != "" && t.GetName() != ""
}

func describeSupervisor(actor *ateapipb.Actor, template *ateapipb.ActorTemplate) (*supervisorObservation, error) {
	expected, err := supervisorbootstrap.Describe(template)
	actual := actor.GetStatus().GetSupervisorActivation()
	assignment := actor.GetStatus().GetWorkerAssignment()
	if err != nil || expected == nil || expected.GetTemplateUid() == "" || actual.GetActivationId() == "" || actual.GetWorkerUid() == "" || assignment.GetWorkerPodUid() != actual.GetWorkerUid() || assignment.GetNodeName() == "" {
		return nil, errors.New("protected supervisor identity unavailable")
	}
	expected.ActivationId, expected.WorkerUid = actual.ActivationId, actual.WorkerUid
	if !proto.Equal(expected, actual) {
		return nil, errors.New("supervisor activation differs from current template")
	}
	limits, err := supervisorLimits(template.GetContainers()[0].GetResources())
	if err != nil {
		return nil, err
	}
	m := actor.GetMetadata()
	t := template.GetMetadata()
	return &supervisorObservation{
		SchemaVersion: "substrate.supervisor-observation/v1",
		Atespace:      m.GetAtespace(), ActorName: m.GetName(), ActorUID: m.GetUid(),
		State:      actor.GetStatus().GetState().String(),
		Assignment: supervisorAssignment{WorkerUID: assignment.GetWorkerPodUid(), NodeName: assignment.GetNodeName()},
		Activation: actual,
		Template:   supervisorTemplate{Atespace: t.GetAtespace(), Name: t.GetName(), UID: t.GetUid(), Digest: expected.GetTemplateDigest(), ResourceLimits: limits},
	}, nil
}

// Quantities are positive canonical Kubernetes strings, e.g. 500m and 512Mi.
func supervisorLimits(resources *ateapipb.Resources) (map[string]string, error) {
	limits := map[string]string{}
	for _, limit := range resources.GetLimits() {
		name := limit.GetName()
		if name != "cpu" && name != "memory" {
			continue
		}
		quantity, err := resource.ParseQuantity(limit.GetQuantity())
		if err != nil || quantity.Sign() <= 0 || limits[name] != "" {
			return nil, errors.New("supervisor resource limits invalid")
		}
		limits[name] = quantity.String()
	}
	if len(limits) != 2 {
		return nil, errors.New("supervisor resource limits unavailable")
	}
	return limits, nil
}

func init() {
	getSupervisorActivationCmd.Flags().StringVarP(&supervisorAtespace, "atespace", "a", "", "Atespace of the protected actor")
	getCmd.AddCommand(getSupervisorActivationCmd)
}

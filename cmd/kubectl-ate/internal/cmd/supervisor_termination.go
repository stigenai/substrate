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
	"errors"
	"io"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/supervisortermination"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

var terminationAtespace, terminationActorUID, terminationActivationID string
var getSupervisorTerminationCmd = &cobra.Command{
	Use:   "supervisor-termination NAME",
	Short: "Read immutable host-stop and deletion proof for one exact activation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if terminationAtespace == "" || terminationActorUID == "" || terminationActivationID == "" || outputFmt != "json" {
			return errors.New("supervisor-termination requires --atespace, --actor-uid, --activation-id and --output=json")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		client, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return errors.New("supervisor termination connection unavailable")
		}
		defer client.Close()
		return writeSupervisorTermination(ctx, client, &ateapipb.GetSupervisorTerminationRequest{Actor: &ateapipb.ObjectRef{Atespace: terminationAtespace, Name: args[0]}, ActorUid: terminationActorUID, ActivationId: terminationActivationID}, cmd.OutOrStdout())
	},
}

type supervisorTerminationReader interface {
	GetSupervisorTermination(context.Context, *ateapipb.GetSupervisorTerminationRequest, ...grpc.CallOption) (*ateapipb.SupervisorTerminationReceipt, error)
}

func writeSupervisorTermination(ctx context.Context, client supervisorTerminationReader, req *ateapipb.GetSupervisorTerminationRequest, out io.Writer) error {
	receipt, err := client.GetSupervisorTermination(ctx, req)
	if err != nil {
		return errors.New("supervisor termination proof unavailable")
	}
	if err := supervisortermination.ValidateReceipt(req.GetActor(), req.GetActorUid(), req.GetActivationId(), receipt); err != nil {
		return errors.New("supervisor termination proof differs")
	}
	encoded, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(receipt)
	if err != nil || len(encoded) > 16384 {
		return errors.New("supervisor termination proof unavailable")
	}
	_, err = out.Write(append(encoded, '\n'))
	return err
}

func init() {
	getSupervisorTerminationCmd.Flags().StringVarP(&terminationAtespace, "atespace", "a", "", "Atespace of the deleted actor")
	getSupervisorTerminationCmd.Flags().StringVar(&terminationActorUID, "actor-uid", "", "Exact retained actor UID")
	getSupervisorTerminationCmd.Flags().StringVar(&terminationActivationID, "activation-id", "", "Exact retained activation ID")
	getCmd.AddCommand(getSupervisorTerminationCmd)
}

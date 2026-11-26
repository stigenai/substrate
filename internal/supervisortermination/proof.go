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

// Package supervisortermination validates exact, public host-stop evidence.
package supervisortermination

import (
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/supervisorbootstrap"
	"google.golang.org/protobuf/proto"
)

const AckSchema = "substrate.supervisor-stop/v1"
const ReceiptSchema = "substrate.supervisor-termination/v1"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidateAck never converts missing workload state into positive stop evidence.
func ValidateAck(actorUID string, activation *ateapi.SupervisorActivation, ack *ateapi.SupervisorTerminationAck) error {
	if actorUID == "" || activation == nil || activation.GetProfile() != supervisorbootstrap.Profile || activation.GetWorkerUid() == "" || activation.GetTemplateUid() == "" || !digestPattern.MatchString(activation.GetTemplateDigest()) || !digestPattern.MatchString(activation.GetImageDigest()) || !digestPattern.MatchString(activation.GetLaunchDigest()) {
		return fmt.Errorf("protected activation binding missing")
	}
	if _, err := uuid.Parse(activation.GetActivationId()); err != nil {
		return fmt.Errorf("activation identity invalid")
	}
	if ack.GetSchemaVersion() != AckSchema || ack.GetActorUid() != actorUID || !proto.Equal(activation, ack.GetActivation()) {
		return fmt.Errorf("stop acknowledgement identity differs")
	}
	if _, err := uuid.Parse(ack.GetAcknowledgementId()); err != nil {
		return fmt.Errorf("stop acknowledgement identity invalid")
	}
	if ack.GetRuntime() != "gvisor" && ack.GetRuntime() != "microvm" {
		return fmt.Errorf("stop runtime unsupported")
	}
	if ack.GetTerminatedAt() == nil || ack.GetTerminatedAt().CheckValid() != nil || !ack.GetTerminatedAt().AsTime().After(time.Unix(0, 0)) {
		return fmt.Errorf("stop timestamp invalid")
	}
	return nil
}

// ValidateReceipt binds a historical receipt to the requested incarnation.
func ValidateReceipt(ref *ateapi.ObjectRef, actorUID, activationID string, receipt *ateapi.SupervisorTerminationReceipt) error {
	if ref.GetAtespace() == "" || ref.GetName() == "" || receipt.GetSchemaVersion() != ReceiptSchema || receipt.GetAtespace() != ref.GetAtespace() || receipt.GetActorName() != ref.GetName() || receipt.GetActorUid() != actorUID || receipt.GetActivation().GetActivationId() != activationID || receipt.GetNodeName() == "" {
		return fmt.Errorf("termination receipt identity differs")
	}
	if err := ValidateAck(actorUID, receipt.GetActivation(), receipt.GetStop()); err != nil {
		return err
	}
	if _, err := uuid.Parse(receipt.GetDeletionId()); err != nil {
		return fmt.Errorf("deletion identity invalid")
	}
	if receipt.GetDeletedAt() == nil || receipt.GetDeletedAt().CheckValid() != nil || receipt.GetDeletedAt().AsTime().Before(receipt.GetStop().GetTerminatedAt().AsTime()) {
		return fmt.Errorf("deletion timestamp invalid")
	}
	return nil
}

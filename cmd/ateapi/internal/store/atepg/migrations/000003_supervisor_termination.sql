-- Copyright 2026 Google LLC
--
-- Licensed under the Apache License, Version 2.0 (the "License");
-- you may not use this file except in compliance with the License.
-- You may obtain a copy of the License at
--
--     http://www.apache.org/licenses/LICENSE-2.0
--
-- Unless required by applicable law or agreed to in writing, software
-- distributed under the License is distributed on an "AS IS" BASIS,
-- WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
-- See the License for the specific language governing permissions and
-- limitations under the License.

-- +goose Up

-- No actor foreign key: proof survives deletion and later name reuse.
CREATE TABLE supervisor_terminations (
    atespace TEXT NOT NULL,
    actor_name TEXT NOT NULL,
    actor_uid TEXT NOT NULL,
    activation_id TEXT NOT NULL,
    proto BYTEA NOT NULL,
    PRIMARY KEY (atespace, actor_name, actor_uid, activation_id)
);

-- +goose StatementBegin
CREATE FUNCTION reject_supervisor_termination_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'supervisor termination receipts are immutable';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER supervisor_termination_immutable
BEFORE UPDATE OR DELETE ON supervisor_terminations
FOR EACH ROW EXECUTE FUNCTION reject_supervisor_termination_mutation();

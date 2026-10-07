// Copyright 2026 Microsoft Corporation
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

package validationutils

import (
	"context"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
)

// ClusterValidation represents a validation that can be performed on a cluster.
type ClusterValidation interface {
	// Name returns the name of the validation.
	Name() string
	// Validate validates the Cluster and returns a ValidationResult describing the outcome.
	Validate(ctx context.Context, clusterSubscription *coreapi.Subscription, cluster *coreapi.Cluster) ValidationResult
}

// ClusterValidationInputs is an optional interface a ClusterValidation may implement to opt
// into re-validation on customer-initiated change.
//
// A passed validation is held off by its EarliestRetryAfter cooldown, which PassedValidation
// defaults to 12 hours. A customer who edits a validated field would otherwise wait out that
// cooldown before learning the edit broke something. When a validation implements this
// interface, the controller bypasses the cooldown as soon as the declared inputs change, so the
// edit is re-validated promptly. Validations that do not implement it keep the cooldown-only
// behaviour and are unaffected.
//
// Implementing this interface IS the opt-in; there is no separate registry or enablement flag.
type ClusterValidationInputs interface {
	// ValidationInputs returns the customer-controlled values this validation depends on.
	//
	// Only customer-controlled values belong here. Listing a field that backend controllers
	// write would make the validation re-run on internal state churn rather than on customer
	// intent, which is explicitly not what this mechanism is for.
	//
	// The returned values must be stable across calls for an unchanged cluster, must not
	// contain secrets, and must tolerate a partially-populated cluster: this is called on
	// every sync, including before other controllers have filled in their fields.
	ValidationInputs(cluster *coreapi.Cluster) []string
}

package validation

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

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/lru"

	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/validationutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	ClusterValidationContainerRegistryPullCredentialsPermissionValidationControllerName = "ClusterValidationContainerRegistryPullCredentialsPermissionValidation"
	ClusterValidationDataPlaneIdentitiesPermissionsValidationControllerName             = "ClusterValidationDataPlaneIdentitiesPermissionsValidation"
	ClusterValidationControlPlaneIdentitiesPermissionsClusterValidationControllerName   = "ClusterValidationControlPlaneIdentitiesPermissionsClusterValidation"
	ClusterValidationAzureClusterManagedIdentitiesExistenceValidationControllerName     = "ClusterValidationAzureClusterManagedIdentitiesExistenceValidation"
	ClusterValidationAzureClusterResourceGroupExistenceValidationControllerName         = "ClusterValidationAzureClusterResourceGroupExistenceValidation"
	ClusterValidationAzureResourceProvidersRegistrationValidationControllerName         = "ClusterValidationAzureResourceProvidersRegistrationValidation"
	ClusterValidationAlwaysSuccessValidationControllerName                              = "ClusterValidationAlwaysSuccessValidation"
	// consecutiveUnknownCountsCacheCapacity bounds the size of the consecutiveUnknownCounts LRU cache.
	consecutiveUnknownCountsCacheCapacity = 50000

	// lastValidatedInputsDigestCacheCapacity bounds the size of the lastValidatedInputsDigest LRU cache.
	lastValidatedInputsDigestCacheCapacity = 50000

	// maxConsecutiveUnknownsBeforeWrite bounds how many consecutive Unknown validation results are
	// suppressed (i.e. the previously stored condition is kept as-is) before an Unknown condition is
	// allowed to overwrite it. This avoids flapping a cluster's validation status to Unknown on a
	// transient blip while still surfacing a persistent Unknown once it has been observed repeatedly.
	maxConsecutiveUnknownsBeforeWrite = 10
)

// clusterValidationSyncer is a Cluster syncer that performs a Cluster
// validation.
type clusterValidationSyncer struct {
	resourcesDBClient corecosmosstorage.ResourcesDBClient
	// retryCooldownChecker gates re-execution of a key(HCPCluster) that recently had a
	// retry scheduled. Prevents redundant validation runs while the cooldown
	// from a previous EarliestRetryAfter is still active.
	retryCooldownChecker *controllerutil.SettableCooldownChecker
	// enqueueAfter allows the syncer to schedule a delayed re-processing of a
	// key(HCPCluster), bypassing the workqueue's default rate limiter.
	enqueueAfter controllerutils.AfterEnqueuer

	serviceProviderClusterLister corelisters.ServiceProviderClusterLister

	// validation is the validation to perform on the cluster.
	validation validationutils.ClusterValidation

	// consecutiveUnknownCounts tracks, per HCPClusterKey, how many consecutive Unknown validation
	// results have been observed since the last non-Unknown result. It backs the suppression
	// policy in trackConsecutiveUnknowns, which avoids flapping a cluster's validation status
	// to Unknown on a transient blip.
	consecutiveUnknownCounts *lru.Cache

	// clusterLister reads the Cluster from the informer cache to evaluate the validation's
	// declared inputs without a Cosmos read on every sync. Nil when the validation does not
	// implement validationutils.ClusterValidationInputs, since nothing then reads it.
	clusterLister corelisters.ClusterLister

	// lastValidatedInputsDigest records, per HCPClusterKey, the digest of the validation's
	// declared inputs as of the last completed run. A mismatch against the current digest is
	// what lets a customer-initiated edit bypass the retryCooldownChecker. Nil when the
	// validation does not implement validationutils.ClusterValidationInputs.
	//
	// Like retryCooldownChecker this is in-memory and deliberately not persisted: losing it
	// costs at most one extra run of a read-only check, which is cheaper than the Cosmos write
	// and API surface that persisting it would require.
	lastValidatedInputsDigest *lru.Cache
}

var _ controllerutils.ClusterSyncer = (*clusterValidationSyncer)(nil)

// NewClusterValidationController creates a new controller that executes the provided Cluster validation on each cluster.
func NewClusterValidationController(
	validation validationutils.ClusterValidation,
	resourcesDBClient corecosmosstorage.ResourcesDBClient,
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister,
	informers coreinformers.BackendInformers,
) controllerutils.Controller {
	return NewNamedClusterValidationController(fmt.Sprintf("ClusterValidation%s", validation.Name()), validation, resourcesDBClient, serviceProviderClusterLister, informers)
}

func NewNamedClusterValidationController(
	name string,
	validation validationutils.ClusterValidation,
	resourcesDBClient corecosmosstorage.ResourcesDBClient,
	serviceProviderClusterLister corelisters.ServiceProviderClusterLister,
	informers coreinformers.BackendInformers,
) controllerutils.Controller {

	syncer := &clusterValidationSyncer{
		retryCooldownChecker:         controllerutil.NewSettableCooldownChecker(),
		resourcesDBClient:            resourcesDBClient,
		serviceProviderClusterLister: serviceProviderClusterLister,
		validation:                   validation,
		consecutiveUnknownCounts:     lru.New(consecutiveUnknownCountsCacheCapacity),
	}

	// Only validations that declare their customer-controlled inputs opt into bypassing the
	// cooldown on change, so only they need the cluster cache and digest bookkeeping.
	if _, ok := validation.(validationutils.ClusterValidationInputs); ok {
		_, syncer.clusterLister = informers.Clusters()
		syncer.lastValidatedInputsDigest = lru.New(lastValidatedInputsDigestCacheCapacity)
	}

	controller := controllerutils.NewClusterWatchingController(
		name,
		resourcesDBClient,
		informers,
		nil, // as of now, validations do not depend on ReadDesire content
		1*time.Minute,
		syncer,
	)

	// Assert that genericWatchingController implements AfterEnqueuer, which lets the syncer explicitly schedule retries via EnqueueAfter rather than
	// relying on error-based rate-limited requeue. Panics at startup if the interface is not satisfied.
	if enqueuer, ok := controller.(controllerutils.AfterEnqueuer); ok {
		syncer.enqueueAfter = enqueuer
	} else {
		panic("ClusterValidationController must implement AfterEnqueuer")
	}

	return controller
}

func (c *clusterValidationSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	logger := utils.LoggerFromContext(ctx)

	// Skip processing if the key is still within its cooldown window from a previous validation. All outcomes can schedule a cooldown via
	// EarliestRetryAfter so validations run continuously without racing. Re-enqueue so the item is revisited once the cooldown expires.
	//
	// A validation that declares its customer-controlled inputs is exempt from the cooldown once those inputs change, so a customer edit is
	// re-validated promptly rather than after the 12h passed-outcome cooldown.
	if !c.retryCooldownChecker.CanSync(ctx, key) && !c.declaredInputsChanged(ctx, key) {
		if c.enqueueAfter != nil {
			// Add a one-second buffer so the requeue lands strictly after the cooldown expires, avoiding a race where the item fires just before CanSync flips to true.
			c.enqueueAfter.EnqueueAfter(key, c.retryCooldownChecker.TimeUntilReady(key)+time.Second)
		}
		return nil
	}

	existingCluster, err := c.resourcesDBClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Get(ctx, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil // cluster doesn't exist, no work to do
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster: %w", err))
	}
	if existingCluster.ServiceProviderProperties.DeletionTimestamp != nil {
		return nil
	}

	cachedServiceProviderCluster, err := c.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		// CreateServiceProviderCluster will populate it; we'll be re-enqueued via the ServiceProviderCluster informer.
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ServiceProviderCluster: %w", err))
	}

	existingServiceProviderCluster := cachedServiceProviderCluster.DeepCopy()
	subscription, err := c.resourcesDBClient.Subscriptions().Get(ctx, existingCluster.ID.SubscriptionID)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Subscription: %w", err))
	}

	result := c.validation.Validate(ctx, subscription, existingCluster)
	if err := result.Validate(); err != nil {
		return utils.TrackError(fmt.Errorf("validation %s returned invalid ValidationResult: %w", c.validation.Name(), err))
	}

	// Record what was just validated, for every outcome and not only Passed. Leaving a stale digest behind after a Failed or Unknown run would
	// keep declaredInputsChanged true on every subsequent sync, which would bypass the cooldown indefinitely and hammer the external APIs the
	// validation calls.
	c.recordValidatedInputs(key, existingCluster)

	if result.Outcome.Type != validationutils.OutcomeTypePassed {
		logger.Info("Validation outcome", "validation", c.validation.Name(), "result", result)
	}

	replacement := existingServiceProviderCluster.DeepCopy()

	// If the validation was skipped, remove its condition so it doesn't appear in status. Otherwise, reconcile the condition with consecutive-Unknown
	// suppression to avoid flapping on transient errors.
	if result.Outcome.Type == validationutils.OutcomeTypeSkipped {
		meta.RemoveStatusCondition(&replacement.Status.Validations, c.validation.Name())
	} else {
		previousCondition := meta.FindStatusCondition(existingServiceProviderCluster.Status.Validations, c.validation.Name())
		desiredCondition := result.ToCondition(c.validation.Name())

		consecutiveUnknowns := c.trackConsecutiveUnknowns(key, desiredCondition)
		if c.shouldWriteCondition(previousCondition, consecutiveUnknowns) {
			meta.SetStatusCondition(&replacement.Status.Validations, desiredCondition)
		}
	}

	if !equality.Semantic.DeepEqual(existingServiceProviderCluster, replacement) {
		serviceProviderClustersCosmosClient := c.resourcesDBClient.ServiceProviderClusters(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
		_, err = serviceProviderClustersCosmosClient.Replace(ctx, replacement, nil)
		if cosmosstorageutils.IsPreconditionFailedError(err) {
			// if we have a conflict error, then we're guaranteed that our informer will eventually see an update and trigger us again.
			return nil
		}
		if err != nil {
			return utils.TrackError(fmt.Errorf("failed to replace ServiceProviderCluster: %w", err))
		}
	}

	c.handleRequeue(key, result)

	// ControllerReportingPolicy governs only how this Unknown result is reported to the controller
	// machinery (e.g. workqueue error metrics); it has no bearing on the requeue scheduling already
	// handled above by handleRequeue based on EarliestRetryAfter. Keep this as the last step of SyncOnce.
	if result.Outcome.Type == validationutils.OutcomeTypeUnknown && result.Outcome.Unknown.ControllerReportingPolicy == validationutils.ControllerReportingPolicyTypeError {
		return utils.TrackError(fmt.Errorf("validation %s returned an inconclusive (Unknown) result: %s", c.validation.Name(), result.InternalMessage()))
	}

	return nil
}

// declaredInputsChanged reports whether the customer-controlled inputs this validation depends on
// differ from those captured at its last completed run, which is the signal that a customer edit
// warrants re-validating ahead of the cooldown.
//
// It is false whenever the question cannot be answered affirmatively: the validation does not
// declare inputs, the cluster is not in the informer cache yet, or no prior run was recorded for
// this key. Falling back to false leaves the existing cooldown in charge, which is the safe
// direction; the worst case is that re-validation waits for the cooldown to expire as it does today.
func (c *clusterValidationSyncer) declaredInputsChanged(ctx context.Context, key controllerutils.HCPClusterKey) bool {
	inputs, ok := c.validation.(validationutils.ClusterValidationInputs)
	if !ok {
		return false
	}

	cluster, err := c.clusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return false
	}

	previousDigest, seen := c.lastValidatedInputsDigest.Get(key)
	if !seen {
		return false
	}

	return previousDigest.(string) != digestValidationInputs(inputs.ValidationInputs(cluster))
}

// recordValidatedInputs captures the digest of the inputs the validation just ran against. It is a
// no-op for validations that do not declare inputs.
func (c *clusterValidationSyncer) recordValidatedInputs(key controllerutils.HCPClusterKey, cluster *coreapi.Cluster) {
	inputs, ok := c.validation.(validationutils.ClusterValidationInputs)
	if !ok {
		return
	}

	c.lastValidatedInputsDigest.Add(key, digestValidationInputs(inputs.ValidationInputs(cluster)))
}

// digestValidationInputs reduces declared validation inputs to a comparable fixed-size string.
// Values are length-prefixed so that differing splits of the same concatenation, such as
// ["ab", "c"] and ["a", "bc"], do not collide.
func digestValidationInputs(inputs []string) string {
	hash := sha256.New()
	for _, input := range inputs {
		fmt.Fprintf(hash, "%d:%s", len(input), input)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// handleRequeue sets the earliest-retry gate and, for Failed/Unknown outcomes, schedules a
// delayed workqueue requeue. Passed and Skipped outcomes set only the gate (no requeue).
// See EarliestRetryAfter on ValidationResult for the full semantics.
func (c *clusterValidationSyncer) handleRequeue(key controllerutils.HCPClusterKey, result validationutils.ValidationResult) {
	if result.EarliestRetryAfter == nil {
		return
	}

	c.retryCooldownChecker.SetCooldown(key, *result.EarliestRetryAfter)

	if c.enqueueAfter != nil && (result.Outcome.Type == validationutils.OutcomeTypeFailed || result.Outcome.Type == validationutils.OutcomeTypeUnknown) {
		c.enqueueAfter.EnqueueAfter(key, *result.EarliestRetryAfter+time.Second)
	}
}

// shouldWriteCondition reports whether the newly computed validation condition should be written, versus
// suppressed in favor of leaving previousCondition (the condition currently stored for this validation, or
// nil if none is stored yet) untouched.
//
// The write is suppressed only while all of the following hold:
//   - previousCondition is non-nil (there's something worth preserving), and
//   - consecutiveUnknowns is non-zero (the newly computed condition is Unknown; trackConsecutiveUnknowns
//     returns 0 for any non-Unknown result), and
//   - consecutiveUnknowns has not yet exceeded maxConsecutiveUnknownsBeforeWrite.
//
// This backs a suppression policy that avoids flapping a cluster's validation status to Unknown on a
// transient blip: a persistent Unknown streak is still allowed to overwrite the stored condition once it
// exceeds maxConsecutiveUnknownsBeforeWrite, and a Passed/Failed result (consecutiveUnknowns == 0) always
// overwrites immediately, resetting the streak.
func (c *clusterValidationSyncer) shouldWriteCondition(previousCondition *metav1.Condition, consecutiveUnknowns int) bool {
	return previousCondition == nil || consecutiveUnknowns == 0 || consecutiveUnknowns > maxConsecutiveUnknownsBeforeWrite
}

// trackConsecutiveUnknowns maintains the count of consecutive Unknown validation results for the given key. When condition is Unknown it increments and returns the
// running count; otherwise it resets the counter and returns 0.
func (c *clusterValidationSyncer) trackConsecutiveUnknowns(key controllerutils.HCPClusterKey, condition metav1.Condition) int {
	if condition.Status != metav1.ConditionUnknown {
		c.consecutiveUnknownCounts.Remove(key)
		return 0
	}

	count := 1
	if v, ok := c.consecutiveUnknownCounts.Get(key); ok {
		count = v.(int) + 1
	}
	c.consecutiveUnknownCounts.Add(key, count)
	return count
}

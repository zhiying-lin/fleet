/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

// Package clusterresourceplacement features a controller to reconcile the clusterResourcePlacement changes.
package clusterresourceplacement

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/utils"
	"go.goms.io/fleet/pkg/utils/controller"
)

func (r *Reconciler) Reconcile(ctx context.Context, _ controller.QueueKey) (ctrl.Result, error) {
	// TODO workaround to bypass lint check
	return r.handleUpdate(ctx, nil)
}

func (r *Reconciler) handleDelete(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) (ctrl.Result, error) {
	crpKObj := klog.KObj(crp)
	if !controllerutil.ContainsFinalizer(crp, fleetv1beta1.ClusterResourcePlacementCleanupFinalizer) {
		klog.V(4).InfoS("clusterResourcePlacement is being deleted and no cleanup work needs to be done", "clusterResourcePlacement", crpKObj)
		return ctrl.Result{}, nil
	}
	klog.V(2).InfoS("Removing snapshots created by clusterResourcePlacement", "clusterResourcePlacement", crpKObj)
	if err := r.deleteClusterSchedulingPolicySnapshots(ctx, crp); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteClusterResourceSnapshots(ctx, crp); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(crp, fleetv1beta1.ClusterResourcePlacementCleanupFinalizer)
	if err := r.Client.Update(ctx, crp); err != nil {
		klog.ErrorS(err, "Failed to remove crp finalizer", "clusterResourcePlacement", crpKObj)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) deleteClusterSchedulingPolicySnapshots(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) error {
	snapshotList := &fleetv1beta1.ClusterSchedulingPolicySnapshotList{}
	crpKObj := klog.KObj(crp)
	if err := r.UncachedReader.List(ctx, snapshotList, client.MatchingLabels{fleetv1beta1.CRPTrackingLabel: crp.Name}); err != nil {
		klog.ErrorS(err, "Failed to list all clusterSchedulingPolicySnapshots", "clusterResourcePlacement", crpKObj)
		return controller.NewAPIServerError(false, err)
	}
	for i := range snapshotList.Items {
		if err := r.Client.Delete(ctx, &snapshotList.Items[i]); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete clusterSchedulingPolicySnapshot", "clusterResourcePlacement", crpKObj, "clusterSchedulingPolicySnapshot", klog.KObj(&snapshotList.Items[i]))
			return controller.NewAPIServerError(false, err)
		}
	}
	klog.V(2).InfoS("Deleted clusterSchedulingPolicySnapshots", "clusterResourcePlacement", crpKObj, "numberOfSnapshots", len(snapshotList.Items))
	return nil
}

func (r *Reconciler) deleteClusterResourceSnapshots(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) error {
	snapshotList := &fleetv1beta1.ClusterResourceSnapshotList{}
	crpKObj := klog.KObj(crp)
	if err := r.UncachedReader.List(ctx, snapshotList, client.MatchingLabels{fleetv1beta1.CRPTrackingLabel: crp.Name}); err != nil {
		klog.ErrorS(err, "Failed to list all clusterResourceSnapshots", "clusterResourcePlacement", crpKObj)
		return controller.NewAPIServerError(false, err)
	}
	for i := range snapshotList.Items {
		if err := r.Client.Delete(ctx, &snapshotList.Items[i]); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete clusterResourceSnapshots", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", klog.KObj(&snapshotList.Items[i]))
			return controller.NewAPIServerError(false, err)
		}
	}
	klog.V(2).InfoS("Deleted clusterResourceSnapshots", "clusterResourcePlacement", crpKObj, "numberOfSnapshots", len(snapshotList.Items))
	return nil
}

// handleUpdate handles the create/update clusterResourcePlacement event.
// It creates corresponding clusterSchedulingPolicySnapshot and clusterResourceSnapshot if needed and updates the status based on
// clusterSchedulingPolicySnapshot status and work status.
// If the error type is ErrUnexpectedBehavior, the controller will skip the reconciling.
func (r *Reconciler) handleUpdate(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) (ctrl.Result, error) {
	revisionLimit := fleetv1beta1.RevisionHistoryLimitDefaultValue
	if crp.Spec.RevisionHistoryLimit != nil {
		revisionLimit = *crp.Spec.RevisionHistoryLimit
		if revisionLimit <= 0 {
			err := fmt.Errorf("invalid clusterResourcePlacement %s: invalid revisionHistoryLimit %d", crp.Name, revisionLimit)
			klog.ErrorS(controller.NewUnexpectedBehaviorError(err), "Invalid revisionHistoryLimit value and using default value instead", "clusterResourcePlacement", klog.KObj(crp))
			// use the default value instead
			revisionLimit = fleetv1beta1.RevisionHistoryLimitDefaultValue
		}
	}

	_, err := r.getOrCreateClusterSchedulingPolicySnapshot(ctx, crp, int(revisionLimit))
	if err != nil {
		return ctrl.Result{}, err
	}
	selectedResources, err := r.selectResourcesForPlacement(crp)
	if err != nil {
		return ctrl.Result{}, err
	}
	resourceSnapshotSpec := fleetv1beta1.ResourceSnapshotSpec{
		SelectedResources: selectedResources,
	}
	_, err = r.getOrCreateClusterResourceSnapshot(ctx, crp, &resourceSnapshotSpec, int(revisionLimit))
	if err != nil {
		return ctrl.Result{}, err
	}

	// update the status based on the latestPolicySnapshot status
	// update the status based on the work
	return ctrl.Result{}, nil
}

func (r *Reconciler) getOrCreateClusterSchedulingPolicySnapshot(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement, revisionHistoryLimit int) (*fleetv1beta1.ClusterSchedulingPolicySnapshot, error) {
	crpKObj := klog.KObj(crp)
	schedulingPolicy := crp.Spec.Policy.DeepCopy()
	if schedulingPolicy != nil {
		schedulingPolicy.NumberOfClusters = nil // will exclude the numberOfClusters
	}
	policyHash, err := generatePolicyHash(schedulingPolicy)
	if err != nil {
		klog.ErrorS(err, "Failed to generate policy hash of crp", "clusterResourcePlacement", crpKObj)
		return nil, controller.NewUnexpectedBehaviorError(err)
	}

	// latestPolicySnapshotIndex should be -1 when there is no snapshot.
	latestPolicySnapshot, latestPolicySnapshotIndex, err := r.lookupLatestClusterSchedulingPolicySnapshot(ctx, crp)
	if err != nil {
		return nil, err
	}

	if latestPolicySnapshot != nil && string(latestPolicySnapshot.Spec.PolicyHash) == policyHash {
		if err := r.ensureLatestPolicySnapshot(ctx, crp, latestPolicySnapshot); err != nil {
			return nil, err
		}
		return latestPolicySnapshot, nil
	}

	// Need to create new snapshot when 1) there is no snapshots or 2) the latest snapshot hash != current one.
	// mark the last policy snapshot as inactive if it is different from what we have now
	if latestPolicySnapshot != nil &&
		string(latestPolicySnapshot.Spec.PolicyHash) != policyHash &&
		latestPolicySnapshot.Labels[fleetv1beta1.IsLatestSnapshotLabel] == strconv.FormatBool(true) {
		// set the latest label to false first to make sure there is only one or none active policy snapshot
		latestPolicySnapshot.Labels[fleetv1beta1.IsLatestSnapshotLabel] = strconv.FormatBool(false)
		if err := r.Client.Update(ctx, latestPolicySnapshot); err != nil {
			klog.ErrorS(err, "Failed to set the isLatestSnapshot label to false", "clusterResourcePlacement", crpKObj, "clusterSchedulingPolicySnapshot", klog.KObj(latestPolicySnapshot))
			return nil, controller.NewAPIServerError(false, err)
		}
	}

	// delete redundant snapshot revisions before creating a new snapshot to guarantee that the number of snapshots
	// won't exceed the limit.
	if err := r.deleteRedundantSchedulingPolicySnapshots(ctx, crp, revisionHistoryLimit); err != nil {
		return nil, err
	}

	// create a new policy snapshot
	latestPolicySnapshotIndex++
	latestPolicySnapshot = &fleetv1beta1.ClusterSchedulingPolicySnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf(fleetv1beta1.PolicySnapshotNameFmt, crp.Name, latestPolicySnapshotIndex),
			Labels: map[string]string{
				fleetv1beta1.CRPTrackingLabel:      crp.Name,
				fleetv1beta1.IsLatestSnapshotLabel: strconv.FormatBool(true),
				fleetv1beta1.PolicyIndexLabel:      strconv.Itoa(latestPolicySnapshotIndex),
			},
		},
		Spec: fleetv1beta1.SchedulingPolicySnapshotSpec{
			Policy:     schedulingPolicy,
			PolicyHash: []byte(policyHash),
		},
	}
	policySnapshotKObj := klog.KObj(latestPolicySnapshot)
	if err := controllerutil.SetControllerReference(crp, latestPolicySnapshot, r.Scheme); err != nil {
		klog.ErrorS(err, "Failed to set owner reference", "clusterSchedulingPolicySnapshot", policySnapshotKObj)
		// should never happen
		return nil, controller.NewUnexpectedBehaviorError(err)
	}
	// make sure each policySnapshot should always have the annotation if CRP is selectN type
	if crp.Spec.Policy != nil &&
		crp.Spec.Policy.PlacementType == fleetv1beta1.PickNPlacementType &&
		crp.Spec.Policy.NumberOfClusters != nil {
		latestPolicySnapshot.Annotations = map[string]string{
			fleetv1beta1.NumberOfClustersAnnotation: strconv.Itoa(int(*crp.Spec.Policy.NumberOfClusters)),
		}
	}

	if err := r.Client.Create(ctx, latestPolicySnapshot); err != nil {
		klog.ErrorS(err, "Failed to create new clusterSchedulingPolicySnapshot", "clusterSchedulingPolicySnapshot", policySnapshotKObj)
		return nil, controller.NewAPIServerError(false, err)
	}
	return latestPolicySnapshot, nil
}

func (r *Reconciler) deleteRedundantSchedulingPolicySnapshots(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement, revisionHistoryLimit int) error {
	sortedList, err := r.listSortedClusterSchedulingPolicySnapshots(ctx, crp)
	if err != nil {
		return err
	}
	if len(sortedList.Items) < revisionHistoryLimit {
		return nil
	}

	if len(sortedList.Items)-revisionHistoryLimit > 0 {
		// We always delete before creating a new snapshot, the snapshot size should never exceed the limit as there is
		// no finalizer added and object should be deleted immediately.
		klog.Warningf("The number of clusterSchedulingPolicySnapshots exceeds the revisionHistoryLimit and it should never happen", "clusterResourcePlacement", klog.KObj(crp), "numberOfSnapshots", len(sortedList.Items), "revisionHistoryLimit", revisionHistoryLimit)
	}

	// In normal situation, The max of len(sortedList) should be revisionHistoryLimit.
	// We just need to delete one policySnapshot before creating a new one.
	// As a result of defensive programming, it will delete any redundant snapshots which could be more than one.
	for i := 0; i <= len(sortedList.Items)-revisionHistoryLimit; i++ { // need to reserve one slot for the new snapshot
		if err := r.Client.Delete(ctx, &sortedList.Items[i]); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete clusterSchedulingPolicySnapshot", "clusterResourcePlacement", klog.KObj(crp), "clusterSchedulingPolicySnapshot", klog.KObj(&sortedList.Items[i]))
			return controller.NewAPIServerError(false, err)
		}
	}
	return nil
}

// deleteRedundantResourceSnapshots handles multiple snapshots in a group.
func (r *Reconciler) deleteRedundantResourceSnapshots(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement, revisionHistoryLimit int) error {
	sortedList, err := r.listSortedResourceSnapshots(ctx, crp)
	if err != nil {
		return err
	}

	if len(sortedList.Items) < revisionHistoryLimit {
		// If the number of existing snapshots is less than the limit no matter how many snapshots in a group, we don't
		// need to delete any snapshots.
		// Skip the checking and deleting.
		return nil
	}

	crpKObj := klog.KObj(crp)
	lastGroupIndex := -1
	groupCounter := 0

	// delete the snapshots from the end as there are could be multiple snapshots in a group in order to keep the latest
	// snapshots from the end.
	for i := len(sortedList.Items) - 1; i >= 0; i-- {
		snapshotKObj := klog.KObj(&sortedList.Items[i])
		ii, err := parseResourceIndexFromLabel(&sortedList.Items[i])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the resource index label", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", snapshotKObj)
			return controller.NewUnexpectedBehaviorError(err)
		}
		if ii != lastGroupIndex {
			groupCounter++
			lastGroupIndex = ii
		}
		if groupCounter < revisionHistoryLimit { // need to reserve one slot for the new snapshot
			// When the number of group is less than the revision limit, skipping deleting the snapshot.
			continue
		}
		if err := r.Client.Delete(ctx, &sortedList.Items[i]); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete clusterResourceSnapshot", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", snapshotKObj)
			return controller.NewAPIServerError(false, err)
		}
	}
	if groupCounter-revisionHistoryLimit > 0 {
		// We always delete before creating a new snapshot, the snapshot group size should never exceed the limit
		// as there is no finalizer added and the object should be deleted immediately.
		klog.Warningf("The number of clusterResourceSnapshot groups exceeds the revisionHistoryLimit and it should never happen", "clusterResourcePlacement", klog.KObj(crp), "numberOfSnapshotGroups", groupCounter, "revisionHistoryLimit", revisionHistoryLimit)
	}
	return nil
}

// TODO handle all the resources selected by placement larger than 1MB size limit of k8s objects.
func (r *Reconciler) getOrCreateClusterResourceSnapshot(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement, resourceSnapshotSpec *fleetv1beta1.ResourceSnapshotSpec, revisionHistoryLimit int) (*fleetv1beta1.ClusterResourceSnapshot, error) {
	resourceHash, err := generateResourceHash(resourceSnapshotSpec)
	if err != nil {
		klog.ErrorS(err, "Failed to generate resource hash of crp", "clusterResourcePlacement", klog.KObj(crp))
		return nil, controller.NewUnexpectedBehaviorError(err)
	}

	// latestResourceSnapshotIndex should be -1 when there is no snapshot.
	latestResourceSnapshot, latestResourceSnapshotIndex, err := r.lookupLatestResourceSnapshot(ctx, crp)
	if err != nil {
		return nil, err
	}

	latestResourceSnapshotHash := ""
	if latestResourceSnapshot != nil {
		latestResourceSnapshotHash, err = parseResourceGroupHashFromAnnotation(latestResourceSnapshot)
		if err != nil {
			klog.ErrorS(err, "Failed to get the ResourceGroupHashAnnotation", "clusterResourceSnapshot", klog.KObj(latestResourceSnapshot))
			return nil, controller.NewUnexpectedBehaviorError(err)
		}
	}

	if latestResourceSnapshot != nil && latestResourceSnapshotHash == resourceHash {
		if err := r.ensureLatestResourceSnapshot(ctx, latestResourceSnapshot); err != nil {
			return nil, err
		}
		return latestResourceSnapshot, nil
	}

	// Need to create new snapshot when 1) there is no snapshots or 2) the latest snapshot hash != current one.
	// mark the last resource snapshot as inactive if it is different from what we have now
	if latestResourceSnapshot != nil &&
		latestResourceSnapshotHash != resourceHash &&
		latestResourceSnapshot.Labels[fleetv1beta1.IsLatestSnapshotLabel] == strconv.FormatBool(true) {
		// set the latest label to false first to make sure there is only one or none active resource snapshot
		latestResourceSnapshot.Labels[fleetv1beta1.IsLatestSnapshotLabel] = strconv.FormatBool(false)
		if err := r.Client.Update(ctx, latestResourceSnapshot); err != nil {
			klog.ErrorS(err, "Failed to set the isLatestSnapshot label to false", "clusterResourceSnapshot", klog.KObj(latestResourceSnapshot))
			return nil, controller.NewAPIServerError(false, err)
		}
	}
	// delete redundant snapshot revisions before creating a new snapshot to guarantee that the number of snapshots
	// won't exceed the limit.
	if err := r.deleteRedundantResourceSnapshots(ctx, crp, revisionHistoryLimit); err != nil {
		return nil, err
	}

	// create a new resource snapshot
	latestResourceSnapshotIndex++
	latestResourceSnapshot = &fleetv1beta1.ClusterResourceSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf(fleetv1beta1.ResourceSnapshotNameFmt, crp.Name, latestResourceSnapshotIndex),
			Labels: map[string]string{
				fleetv1beta1.CRPTrackingLabel:      crp.Name,
				fleetv1beta1.IsLatestSnapshotLabel: strconv.FormatBool(true),
				fleetv1beta1.ResourceIndexLabel:    strconv.Itoa(latestResourceSnapshotIndex),
			},
			Annotations: map[string]string{
				fleetv1beta1.ResourceGroupHashAnnotation: resourceHash,
				// TODO need to updated once we support multiple snapshots
				fleetv1beta1.NumberOfResourceSnapshotsAnnotation: "1",
			},
		},
		Spec: *resourceSnapshotSpec,
	}
	resourceSnapshotKObj := klog.KObj(latestResourceSnapshot)
	if err := controllerutil.SetControllerReference(crp, latestResourceSnapshot, r.Scheme); err != nil {
		klog.ErrorS(err, "Failed to set owner reference", "clusterResourceSnapshot", resourceSnapshotKObj)
		// should never happen
		return nil, controller.NewUnexpectedBehaviorError(err)
	}

	if err := r.Client.Create(ctx, latestResourceSnapshot); err != nil {
		klog.ErrorS(err, "Failed to create new clusterResourceSnapshot", "clusterResourceSnapshot", resourceSnapshotKObj)
		return nil, controller.NewAPIServerError(false, err)
	}
	return latestResourceSnapshot, nil
}

// ensureLatestPolicySnapshot ensures the latest policySnapshot has the isLatest label and the numberOfClusters are updated.
func (r *Reconciler) ensureLatestPolicySnapshot(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement, latest *fleetv1beta1.ClusterSchedulingPolicySnapshot) error {
	needUpdate := false
	if latest.Labels[fleetv1beta1.IsLatestSnapshotLabel] != strconv.FormatBool(true) {
		// When latestPolicySnapshot.Spec.PolicyHash == policyHash,
		// It could happen when the controller just sets the latest label to false for the old snapshot, and fails to
		// create a new policy snapshot.
		// And then the customers revert back their policy to the old one again.
		// In this case, the "latest" snapshot without isLatest label has the same policy hash as the current policy.

		latest.Labels[fleetv1beta1.IsLatestSnapshotLabel] = strconv.FormatBool(true)
		needUpdate = true
	}

	if crp.Spec.Policy != nil &&
		crp.Spec.Policy.PlacementType == fleetv1beta1.PickNPlacementType &&
		crp.Spec.Policy.NumberOfClusters != nil {
		oldCount, err := utils.ExtractNumOfClustersFromPolicySnapshot(latest)
		if err != nil {
			klog.ErrorS(err, "Failed to parse the numberOfClusterAnnotation", "clusterSchedulingPolicySnapshot", klog.KObj(latest))
			return controller.NewUnexpectedBehaviorError(err)
		}
		newCount := int(*crp.Spec.Policy.NumberOfClusters)
		if oldCount != newCount {
			latest.Annotations[fleetv1beta1.NumberOfClustersAnnotation] = strconv.Itoa(newCount)
			needUpdate = true
		}
	}
	if !needUpdate {
		return nil
	}
	if err := r.Client.Update(ctx, latest); err != nil {
		klog.ErrorS(err, "Failed to update the clusterSchedulingPolicySnapshot", "clusterSchedulingPolicySnapshot", klog.KObj(latest))
		return controller.NewAPIServerError(false, err)
	}
	return nil
}

// ensureLatestResourceSnapshot ensures the latest resourceSnapshot has the isLatest label.
func (r *Reconciler) ensureLatestResourceSnapshot(ctx context.Context, latest *fleetv1beta1.ClusterResourceSnapshot) error {
	if latest.Labels[fleetv1beta1.IsLatestSnapshotLabel] == strconv.FormatBool(true) {
		return nil
	}
	// It could happen when the controller just sets the latest label to false for the old snapshot, and fails to
	// create a new resource snapshot.
	// And then the customers revert back their resource to the old one again.
	// In this case, the "latest" snapshot without isLatest label has the same resource hash as the current one.
	latest.Labels[fleetv1beta1.IsLatestSnapshotLabel] = strconv.FormatBool(true)
	if err := r.Client.Update(ctx, latest); err != nil {
		klog.ErrorS(err, "Failed to update the clusterResourceSnapshot", "ClusterResourceSnapshot", klog.KObj(latest))
		return controller.NewAPIServerError(false, err)
	}
	return nil
}

// lookupLatestClusterSchedulingPolicySnapshot finds the latest snapshots and its policy index.
// There will be only one active policy snapshot if exists.
// It first checks whether there is an active policy snapshot.
// If not, it finds the one whose policyIndex label is the largest.
// The policy index will always start from 0.
// Return error when 1) cannot list the snapshots 2) there are more than one active policy snapshots 3) snapshot has the
// invalid label value.
// 2 & 3 should never happen.
func (r *Reconciler) lookupLatestClusterSchedulingPolicySnapshot(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) (*fleetv1beta1.ClusterSchedulingPolicySnapshot, int, error) {
	snapshotList := &fleetv1beta1.ClusterSchedulingPolicySnapshotList{}
	latestSnapshotLabelMatcher := client.MatchingLabels{
		fleetv1beta1.CRPTrackingLabel:      crp.Name,
		fleetv1beta1.IsLatestSnapshotLabel: strconv.FormatBool(true),
	}
	crpKObj := klog.KObj(crp)
	if err := r.Client.List(ctx, snapshotList, latestSnapshotLabelMatcher); err != nil {
		klog.ErrorS(err, "Failed to list active clusterSchedulingPolicySnapshots", "clusterResourcePlacement", crpKObj)
		// CRP controller needs a scheduling policy snapshot watcher to enqueue the CRP request.
		// So the snapshots should be read from cache.
		return nil, -1, controller.NewAPIServerError(true, err)
	}
	if len(snapshotList.Items) == 1 {
		policyIndex, err := parsePolicyIndexFromLabel(&snapshotList.Items[0])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the policy index label", "clusterResourcePlacement", crpKObj, "clusterSchedulingPolicySnapshot", klog.KObj(&snapshotList.Items[0]))
			return nil, -1, controller.NewUnexpectedBehaviorError(err)
		}
		return &snapshotList.Items[0], policyIndex, nil
	} else if len(snapshotList.Items) > 1 {
		// It means there are multiple active snapshots and should never happen.
		err := fmt.Errorf("there are %d active clusterSchedulingPolicySnapshots owned by clusterResourcePlacement %v", len(snapshotList.Items), crp.Name)
		klog.ErrorS(err, "Invalid clusterSchedulingPolicySnapshots", "clusterResourcePlacement", crpKObj)
		return nil, -1, controller.NewUnexpectedBehaviorError(err)
	}
	// When there are no active snapshots, find the one who has the largest policy index.
	// It should be rare only when CRP is crashed before creating the new active snapshot.
	sortedList, err := r.listSortedClusterSchedulingPolicySnapshots(ctx, crp)
	if err != nil {
		return nil, -1, err
	}

	if len(sortedList.Items) == 0 {
		// The policy index of the first snapshot will start from 0.
		return nil, -1, nil
	}
	latestSnapshot := &sortedList.Items[len(sortedList.Items)-1]
	policyIndex, err := parsePolicyIndexFromLabel(latestSnapshot)
	if err != nil {
		klog.ErrorS(err, "Failed to parse the policy index label", "clusterResourcePlacement", crpKObj, "clusterSchedulingPolicySnapshot", klog.KObj(latestSnapshot))
		return nil, -1, controller.NewUnexpectedBehaviorError(err)
	}
	return latestSnapshot, policyIndex, nil
}

// listSortedClusterSchedulingPolicySnapshots returns the policy snapshots sorted by the policy index.
func (r *Reconciler) listSortedClusterSchedulingPolicySnapshots(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) (*fleetv1beta1.ClusterSchedulingPolicySnapshotList, error) {
	snapshotList := &fleetv1beta1.ClusterSchedulingPolicySnapshotList{}
	crpKObj := klog.KObj(crp)
	if err := r.Client.List(ctx, snapshotList, client.MatchingLabels{fleetv1beta1.CRPTrackingLabel: crp.Name}); err != nil {
		klog.ErrorS(err, "Failed to list all clusterSchedulingPolicySnapshots", "clusterResourcePlacement", crpKObj)
		// CRP controller needs a scheduling policy snapshot watcher to enqueue the CRP request.
		// So the snapshots should be read from cache.
		return nil, controller.NewAPIServerError(true, err)
	}
	var errs []error
	sort.Slice(snapshotList.Items, func(i, j int) bool {
		ii, err := parsePolicyIndexFromLabel(&snapshotList.Items[i])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the policy index label", "clusterResourcePlacement", crpKObj, "clusterSchedulingPolicySnapshot", klog.KObj(&snapshotList.Items[i]))
			errs = append(errs, err)
		}
		ji, err := parsePolicyIndexFromLabel(&snapshotList.Items[j])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the policy index label", "clusterResourcePlacement", crpKObj, "clusterSchedulingPolicySnapshot", klog.KObj(&snapshotList.Items[j]))
			errs = append(errs, err)
		}
		return ii < ji
	})

	if len(errs) > 0 {
		return nil, controller.NewUnexpectedBehaviorError(utilerrors.NewAggregate(errs))
	}

	return snapshotList, nil
}

// lookupLatestResourceSnapshot finds the latest snapshots and.
// There will be only one active resource snapshot if exists.
// It first checks whether there is an active resource snapshot.
// If not, it finds the one whose resourceIndex label is the largest.
// The resource index will always start from 0.
// Return error when 1) cannot list the snapshots 2) there are more than one active resource snapshots 3) snapshot has the
// invalid label value.
// 2 & 3 should never happen.
func (r *Reconciler) lookupLatestResourceSnapshot(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) (*fleetv1beta1.ClusterResourceSnapshot, int, error) {
	snapshotList := &fleetv1beta1.ClusterResourceSnapshotList{}
	latestSnapshotLabelMatcher := client.MatchingLabels{
		fleetv1beta1.CRPTrackingLabel:      crp.Name,
		fleetv1beta1.IsLatestSnapshotLabel: strconv.FormatBool(true),
	}
	crpKObj := klog.KObj(crp)
	if err := r.Client.List(ctx, snapshotList, latestSnapshotLabelMatcher); err != nil {
		klog.ErrorS(err, "Failed to list active clusterResourceSnapshots", "clusterResourcePlacement", crpKObj)
		return nil, -1, controller.NewAPIServerError(false, err)
	}
	if len(snapshotList.Items) == 1 {
		resourceIndex, err := parseResourceIndexFromLabel(&snapshotList.Items[0])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the resource index label", "clusterResourceSnapshot", klog.KObj(&snapshotList.Items[0]))
			return nil, -1, controller.NewUnexpectedBehaviorError(err)
		}
		return &snapshotList.Items[0], resourceIndex, nil
	} else if len(snapshotList.Items) > 1 {
		// It means there are multiple active snapshots and should never happen.
		err := fmt.Errorf("there are %d active clusterResourceSnapshots owned by clusterResourcePlacement %v", len(snapshotList.Items), crp.Name)
		klog.ErrorS(err, "Invalid clusterResourceSnapshots", "clusterResourcePlacement", crpKObj)
		return nil, -1, controller.NewUnexpectedBehaviorError(err)
	}
	// When there are no active snapshots, find the first snapshot who has the largest resource index.
	// It should be rare only when CRP is crashed before creating the new active snapshot.
	sortedList, err := r.listSortedResourceSnapshots(ctx, crp)
	if err != nil {
		return nil, -1, err
	}
	if len(sortedList.Items) == 0 {
		// The resource index of the first snapshot will start from 0.
		return nil, -1, nil
	}
	latestSnapshot := &sortedList.Items[len(sortedList.Items)-1]
	resourceIndex, err := parseResourceIndexFromLabel(latestSnapshot)
	if err != nil {
		klog.ErrorS(err, "Failed to parse the resource index label", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", klog.KObj(latestSnapshot))
		return nil, -1, controller.NewUnexpectedBehaviorError(err)
	}
	return latestSnapshot, resourceIndex, nil
}

// listSortedResourceSnapshots returns the resource snapshots sorted by its index and its subindex.
// The resourceSnapshot is less than the other one when resourceIndex is less.
// When the resourceIndex is equal, then order by the subindex.
// Note: the snapshot does not have subindex is the largest of a group and there should be only one in a group.
func (r *Reconciler) listSortedResourceSnapshots(ctx context.Context, crp *fleetv1beta1.ClusterResourcePlacement) (*fleetv1beta1.ClusterResourceSnapshotList, error) {
	snapshotList := &fleetv1beta1.ClusterResourceSnapshotList{}
	crpKObj := klog.KObj(crp)
	if err := r.Client.List(ctx, snapshotList, client.MatchingLabels{fleetv1beta1.CRPTrackingLabel: crp.Name}); err != nil {
		klog.ErrorS(err, "Failed to list all clusterResourceSnapshots", "clusterResourcePlacement", crpKObj)
		return nil, controller.NewAPIServerError(false, err)
	}
	var errs []error
	sort.Slice(snapshotList.Items, func(i, j int) bool {
		iKObj := klog.KObj(&snapshotList.Items[i])
		jKObj := klog.KObj(&snapshotList.Items[j])
		ii, err := parseResourceIndexFromLabel(&snapshotList.Items[i])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the resource index label", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", iKObj)
			errs = append(errs, err)
		}
		ji, err := parseResourceIndexFromLabel(&snapshotList.Items[j])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the resource index label", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", jKObj)
			errs = append(errs, err)
		}
		if ii != ji {
			return ii < ji
		}

		iDoesExist, iSubindex, err := utils.ExtractSubindexFromClusterResourceSnapshot(&snapshotList.Items[i])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the subindex index", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", iKObj)
			errs = append(errs, err)
		}
		jDoesExist, jSubindex, err := utils.ExtractSubindexFromClusterResourceSnapshot(&snapshotList.Items[j])
		if err != nil {
			klog.ErrorS(err, "Failed to parse the subindex index", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", jKObj)
			errs = append(errs, err)
		}

		// Both of the snapshots do not have subindex, which should not happen.
		if !iDoesExist && !jDoesExist {
			klog.ErrorS(err, "There are more than one resource snapshot which do not have subindex in a group", "clusterResourcePlacement", crpKObj, "clusterResourceSnapshot", iKObj, "clusterResourceSnapshot", jKObj)
			errs = append(errs, err)
		}

		if !iDoesExist { // check if it's the first snapshot
			return false
		}
		if !jDoesExist { // check if it's the first snapshot
			return true
		}
		return iSubindex < jSubindex
	})

	if len(errs) > 0 {
		return nil, controller.NewUnexpectedBehaviorError(utilerrors.NewAggregate(errs))
	}

	return snapshotList, nil
}

// parsePolicyIndexFromLabel returns error when parsing the label which should never return error in production.
func parsePolicyIndexFromLabel(s *fleetv1beta1.ClusterSchedulingPolicySnapshot) (int, error) {
	indexLabel := s.Labels[fleetv1beta1.PolicyIndexLabel]
	v, err := strconv.Atoi(indexLabel)
	if err != nil || v < 0 {
		return -1, fmt.Errorf("invalid policy index %q, error: %w", indexLabel, err)
	}
	return v, nil
}

// parseResourceIndexFromLabel returns error when parsing the label which should never return error in production.
func parseResourceIndexFromLabel(s *fleetv1beta1.ClusterResourceSnapshot) (int, error) {
	indexLabel := s.Labels[fleetv1beta1.ResourceIndexLabel]
	v, err := strconv.Atoi(indexLabel)
	if err != nil || v < 0 {
		return -1, fmt.Errorf("invalid resource index %q, error: %w", indexLabel, err)
	}
	return v, nil
}

func generatePolicyHash(policy *fleetv1beta1.PlacementPolicy) (string, error) {
	jsonBytes, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(jsonBytes)), nil
}

func generateResourceHash(rs *fleetv1beta1.ResourceSnapshotSpec) (string, error) {
	jsonBytes, err := json.Marshal(rs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(jsonBytes)), nil
}

// parseResourceGroupHashFromAnnotation returns error when parsing the annotation which should never return error in production.
func parseResourceGroupHashFromAnnotation(s *fleetv1beta1.ClusterResourceSnapshot) (string, error) {
	v, ok := s.Annotations[fleetv1beta1.ResourceGroupHashAnnotation]
	if !ok {
		return "", fmt.Errorf("ResourceGroupHashAnnotation is not set")
	}
	return v, nil
}

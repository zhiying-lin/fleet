package clusterresourcerollout

import (
	"context"
	"errors"
	"fmt"
	
	"go.goms.io/fleet/pkg/utils/apiretry"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strconv"
	"time"

	fleetv1 "go.goms.io/fleet/apis/v1"
	"go.goms.io/fleet/pkg/utils/controller"
)

// Reconciler reconciles the active ClusterResourceSnapshot object.
type Reconciler struct {
	client.Client
	recorder record.EventRecorder
}

// Reconcile rollouts the resources by updating/deleting the clusterResourceBindings.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	name := req.NamespacedName
	crs := fleetv1.ClusterResourceSnapshot{}
	crsKRef := klog.KRef(name.Namespace, name.Name)

	startTime := time.Now()
	klog.V(2).InfoS("Reconciliation starts", "clusterResourceSnapshot", crsKRef)
	defer func() {
		latency := time.Since(startTime).Milliseconds()
		klog.V(2).InfoS("Reconciliation ends", "clusterResourceSnapshot", crsKRef, "latency", latency)
	}()

	if err := r.Client.Get(ctx, name, &crs); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).InfoS("Ignoring NotFound clusterResourceSnapshot", "clusterResourceSnapshot", crsKRef)
			return ctrl.Result{}, nil
		}
		klog.ErrorS(err, "Failed to get clusterResourceSnapshot", "clusterResourceSnapshot", crsKRef)
		return ctrl.Result{}, err
	}

	if crs.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDelete(ctx, &crs)
	}

	// register finalizer
	if !controllerutil.ContainsFinalizer(&crs, fleetv1.ClusterResourceSnapshotFinalizer) {
		controllerutil.AddFinalizer(&crs, fleetv1.ClusterResourceSnapshotFinalizer)
		if err := r.Update(ctx, &crs); err != nil {
			klog.ErrorS(err, "Failed to add mcs finalizer", "clusterResourceSnapshot", crsKRef)
			return ctrl.Result{}, err
		}
	}
	res, err := r.handleUpdate(ctx, &crs)
	// skip the reconciling when the system is in the unexpected situation
	if err != nil && errors.Is(err, controller.ErrUnexpectedBehavior) {
		return ctrl.Result{}, nil
	}
	return res, err
}

func (r *Reconciler) handleDelete(ctx context.Context, crs *fleetv1.ClusterResourceSnapshot) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *Reconciler) handleUpdate(ctx context.Context, crs *fleetv1.ClusterResourceSnapshot) (ctrl.Result, error) {
	if crs.Labels[fleetv1.IsLatestSnapshotLabel] != strconv.FormatBool(true) {
		return ctrl.Result{}, nil // skip the reconciling if the resourceSnapshot is not active
	}

	crsKObj := klog.KObj(crs)
	if crs.Labels[fleetv1.CRPTrackingLabel] == "" {
		err := fmt.Errorf("empty CRPTrackingLabel in clusterResourceSnapshot %v", crs.Name)
		klog.ErrorS(err, "Failed to get clusterResourceSnapshot", "clusterResourceSnapshot", crsKObj)
		// should never hit this
		// TODO(zhiyinglin) emit metrics or well defined log
		return ctrl.Result{}, controller.NewUnexpectedBehaviorError(err)
	}
	crp := fleetv1.ClusterResourcePlacement{}
	crpName := types.NamespacedName{Name: crs.Labels[fleetv1.CRPTrackingLabel]}
	if err := r.Client.Get(ctx, crpName, &crp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // the clusterResourceSnapshot should be deleted soon
		}
		klog.ErrorS(err, "Failed to get clusterResourcePlacement", "clusterResourceSnapshot", crsKObj, "clusterResourcePlacement", klog.KRef(crpName.Namespace, crpName.Name))
		return ctrl.Result{}, err
	}

	cps := fleetv1.ClusterPolicySnapshot{}
	cpsName := types.NamespacedName{Name: crs.Spec.PolicySnapshotName}
	if err := r.Client.Get(ctx, cpsName, &cps); err != nil {
		if apierrors.IsNotFound(err) {
			// assuming clusterResourcePlacement and its clusterPolicySnapshot are just deleted and the clusterResourceSnapshot should be deleted soon
			return ctrl.Result{}, nil
		}
		klog.ErrorS(err, "Failed to get clusterPolicySnapshot", "clusterResourceSnapshot", crsKObj, "clusterPolicySnapshot", klog.KRef(cpsName.Namespace, cpsName.Name))
		return ctrl.Result{}, err
	}
	if cps.Labels[fleetv1.IsLatestSnapshotLabel] != strconv.FormatBool(true) {
		// skip the reconciling if the policySnapshot is not active and there will be a new resourceSnapshot created later
		return ctrl.Result{}, nil
	}

	bindingList := &fleetv1.ClusterResourceBindingList{}
	bindingLabelMatcher := client.MatchingLabels{
		fleetv1.CRPTrackingLabel: crpName.Name,
	}
	crpKObj := klog.KObj(&crp)
	if err := r.Client.List(ctx, bindingList, bindingLabelMatcher); err != nil {
		klog.ErrorS(err, "Failed to list clusterResourceBinding", "clusterResourcePlacement", crpKObj)
		return ctrl.Result{}, err
	}
	if len(bindingList.Items) == 0 {
		// it is possible that scheduler is unable to find any cluster meets the requirement
		// skip the reconciling loop
		return ctrl.Result{}, nil
	}

	newBindingCounter := int32(0) // the count of new bindings with the latest resource snapshot
	for i := range bindingList.Items {
		if bindingList.Items[i].ObjectMeta.DeletionTimestamp != nil {
			continue // skipping when binding is in deleting state
		}

		// When the current count of bindings reaches the desired one, we'll delete the redundant bindings and stop updating.
		needCleanupRedundantBindings, err := checkBindingsForPickN(&crp, newBindingCounter)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Delete the bindings when
		// * the bindings is created based on the old policy and crp is configured with recreate rollout strategy.
		// * For the selectN, there are already desired number of bindings with new resource.
		if needCleanupRedundantBindings == true ||
			(isRecreateRolloutStrategyType(&crp) && bindingList.Items[i].Spec.PolicySnapshotName != crs.Spec.PolicySnapshotName) {
			deleteFunc := func() error {
				return r.Client.Delete(ctx, &bindingList.Items[i])
			}
			if err := apiretry.Do(deleteFunc); err != nil && !apierrors.IsNotFound(err) {
				klog.ErrorS(err, "Failed to delete clusterResourceBinding", "ClusterResourceBinding", klog.KObj(&bindingList.Items[i]))
				return ctrl.Result{}, err
			}
			continue
		}

		if bindingList.Items[i].Spec.ResourceSnapshotName == crs.Name {
			newBindingCounter++
			continue // already point to the latest resource snapshot
		}
		// update the resource when the policy has not changed compared with last resource snapshot
		if bindingList.Items[i].Spec.PolicySnapshotName == crs.Spec.PolicySnapshotName {
			bindingList.Items[i].Spec.ResourceSnapshotName = crs.Name
			updateFunc := func() error {
				return r.Client.Update(ctx, &bindingList.Items[i])
			}
			if err := apiretry.Do(updateFunc); err != nil && !apierrors.IsNotFound(err) {
				klog.ErrorS(err, "Failed to update clusterResourceBinding", "ClusterResourceBinding", klog.KObj(&bindingList.Items[i]))
				return ctrl.Result{}, err
			}
			newBindingCounter++
			continue
		}
		// do nothing for bindings with old policy & other strategy types
	}
	return ctrl.Result{}, nil
}

// checkBindingsForPickN validates the count of current bindings for the pickN type.
// Return true if the current count reaches the desired number of clusters.
// Controllers should stop updating the bindings and delete the redundant bindings.
// Return false will be false if crp type is not pickN.
func checkBindingsForPickN(crp *fleetv1.ClusterResourcePlacement, counter int32) (bool, error) {
	if crp.Spec.Policy == nil || crp.Spec.Policy.PlacementType != fleetv1.PickNPlacementType {
		return false, nil
	}
	if crp.Spec.Policy.NumberOfClusters == nil {
		err := fmt.Errorf("numberOfClusters of clusterResourcePlacement %v is not set for the selectN type", crp.Name)
		klog.ErrorS(err, "Invalid clusterResourcePlacement", "clusterResourcePlacement", klog.KObj(crp))
		// should never hit this
		// TODO(zhiyinglin) emit metrics or well defined log
		return false, controller.NewUnexpectedBehaviorError(err)
	}
	return counter == *crp.Spec.Policy.NumberOfClusters, nil
}

func isRecreateRolloutStrategyType(crp *fleetv1.ClusterResourcePlacement) bool {
	return crp.Spec.Policy == nil || crp.Spec.Policy.Strategy == nil || crp.Spec.Policy.Strategy.Type == fleetv1.RecreateRolloutStrategyType
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.recorder = mgr.GetEventRecorderFor("clusterResourceRollout")
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.ClusterResourceSnapshot{}).
		Complete(r)
}

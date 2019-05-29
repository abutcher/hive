/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package syncset

import (
	"context"
	"crypto/md5"
	"fmt"
	"reflect"
	"time"

	log "github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1alpha1"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
	hiveresource "github.com/openshift/hive/pkg/resource"
)

const (
	controllerName              = "syncset"
	adminKubeConfigKey          = "kubeconfig"
	unknownObjectFoundReason    = "UnknownObjectFound"
	unknownObjectNotFoundReason = "UnknownObjectNotFound"
	applySucceededReason        = "ApplySucceeded"
	applyFailedReason           = "ApplyFailed"
	deletionFailedReason        = "DeletionFailed"
	reapplyInterval             = 2 * time.Hour
)

// Applier knows how to Apply, Patch and return Info for []byte arrays describing objects and patches.
type Applier interface {
	Apply(obj []byte) (hiveresource.ApplyResult, error)
	Info(obj []byte) (*hiveresource.Info, error)
	Patch(name types.NamespacedName, kind, apiVersion string, patch []byte, patchType string) error
}

// Add creates a new SyncSet Controller and adds it to the Manager with default RBAC. The Manager will set fields on the
// Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return AddToManager(mgr, NewReconciler(mgr))
}

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	r := &ReconcileSyncSet{
		Client:               mgr.GetClient(),
		scheme:               mgr.GetScheme(),
		logger:               log.WithField("controller", controllerName),
		applierBuilder:       applierBuilderFunc,
		dynamicClientBuilder: controllerutils.BuildDynamicClientFromKubeconfig,
	}
	r.hash = r.resourceHash
	return r
}

// applierBuilderFunc returns an Applier which implements Info, Apply and Patch
func applierBuilderFunc(kubeConfig []byte, logger log.FieldLogger) Applier {
	var helper Applier = hiveresource.NewHelper(kubeConfig, logger)
	return helper
}

// AddToManager adds a new Controller to mgr with r as the reconcile.Reconciler
func AddToManager(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("syncset-controller", mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: controllerutils.GetConcurrentReconciles()})
	if err != nil {
		return err
	}

	// Watch for changes to ClusterDeployment
	err = c.Watch(&source.Kind{Type: &hivev1.ClusterDeployment{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for SyncSet
	err = c.Watch(&source.Kind{Type: &hivev1.SyncSet{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(syncSetHandlerFunc),
	})
	if err != nil {
		return err
	}

	// Watch for SelectorSyncSet
	reconciler := r.(*ReconcileSyncSet)
	err = c.Watch(&source.Kind{Type: &hivev1.SelectorSyncSet{}}, &handler.EnqueueRequestsFromMapFunc{
		ToRequests: handler.ToRequestsFunc(reconciler.selectorSyncSetHandlerFunc),
	})
	if err != nil {
		return err
	}

	return nil
}

func syncSetHandlerFunc(a handler.MapObject) []reconcile.Request {
	syncSet := a.Object.(*hivev1.SyncSet)
	retval := []reconcile.Request{}

	for _, clusterDeploymentRef := range syncSet.Spec.ClusterDeploymentRefs {
		retval = append(retval, reconcile.Request{NamespacedName: types.NamespacedName{
			Name:      clusterDeploymentRef.Name,
			Namespace: syncSet.Namespace,
		}})
	}

	return retval
}

func (r *ReconcileSyncSet) selectorSyncSetHandlerFunc(a handler.MapObject) []reconcile.Request {
	selectorSyncSet := a.Object.(*hivev1.SelectorSyncSet)
	clusterDeployments := &hivev1.ClusterDeploymentList{}

	err := r.List(context.TODO(), &client.ListOptions{}, clusterDeployments)
	if err != nil {
		r.logger.WithError(err)
		return []reconcile.Request{}
	}

	labelSelector, err := metav1.LabelSelectorAsSelector(&selectorSyncSet.Spec.ClusterDeploymentSelector)
	if err != nil {
		r.logger.WithError(err)
		return []reconcile.Request{}
	}

	retval := []reconcile.Request{}
	for _, clusterDeployment := range clusterDeployments.Items {
		if labelSelector.Matches(labels.Set(clusterDeployment.Labels)) {
			retval = append(retval, reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      clusterDeployment.Name,
				Namespace: clusterDeployment.Namespace,
			}})
		}
	}

	return retval
}

var _ reconcile.Reconciler = &ReconcileSyncSet{}

// ReconcileSyncSet reconciles a ClusterDeployment and the SyncSets associated with it
type ReconcileSyncSet struct {
	client.Client
	scheme *runtime.Scheme

	logger               log.FieldLogger
	applierBuilder       func([]byte, log.FieldLogger) Applier
	hash                 func([]byte) string
	dynamicClientBuilder func(string) (dynamic.Interface, error)
}

// Reconcile lists SyncSets and SelectorSyncSets which apply to a ClusterDeployment object and applies resources and patches
// found in each SyncSet object
// +kubebuilder:rbac:groups=hive.openshift.io,resources=selectorsyncsets,verbs=get;create;update;delete;patch;list;watch
func (r *ReconcileSyncSet) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the ClusterDeployment instance
	cd := &hivev1.ClusterDeployment{}

	err := r.Get(context.TODO(), request.NamespacedName, cd)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request
		r.logger.WithError(err).Error("error looking up cluster deployment")
		return reconcile.Result{}, err
	}

	// If the clusterdeployment is deleted, do not reconcile.
	if cd.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}

	cdLog := r.logger.WithFields(log.Fields{
		"clusterDeployment": cd.Name,
		"namespace":         cd.Namespace,
	})

	if !cd.Status.Installed {
		// Cluster isn't installed yet, return
		cdLog.Debug("cluster installation is not complete")
		return reconcile.Result{}, nil
	}

	origCD := cd
	cd = cd.DeepCopy()

	cdLog.Info("reconciling sync sets for cluster deployment")

	// get all sync sets that apply to cd
	syncSets, err := r.getRelatedSyncSets(cd)
	if err != nil {
		cdLog.WithError(err).Error("unable to list related sync sets for cluster deployment")
		return reconcile.Result{}, err
	}

	// get all selector sync sets that apply to cd
	selectorSyncSets, err := r.getRelatedSelectorSyncSets(cd)
	if err != nil {
		cdLog.WithError(err).Error("unable to list related sync sets for cluster deployment")
		return reconcile.Result{}, err
	}

	// TODO: Remove this code when no longer needed
	// BEGIN migration code
	err = r.migrateSyncSetPatchTypes(syncSets, cdLog)
	if err != nil {
		cdLog.WithError(err).Error("failed to migrate existing syncsets")
	}
	err = r.migrateSelectorSyncSetPatchTypes(selectorSyncSets, cdLog)
	if err != nil {
		cdLog.WithError(err).Error("failed to migrate existing selector syncsets")
	}
	// END migration code

	// get kubeconfig for the cluster
	secretName := cd.Status.AdminKubeconfigSecret.Name
	secretData, err := r.loadSecretData(secretName, cd.Namespace, adminKubeConfigKey)
	if err != nil {
		cdLog.WithError(err).Error("unable to load admin kubeconfig")
		return reconcile.Result{}, err
	}
	kubeConfig := []byte(secretData)
	dynamicClient, err := r.dynamicClientBuilder(string(kubeConfig))
	if err != nil {
		cdLog.WithError(err).Error("unable to build dynamic client")
		return reconcile.Result{}, err
	}

	// Track the first error we hit during reconcile. This allows us to keep processing
	// objects even if one encounters an error, but we always want to return an error to
	// the controllers so they will re-try.
	var firstSyncSetErr error

	for _, syncSet := range syncSets {
		ssLog := cdLog.WithFields(log.Fields{"syncSet": syncSet.Name})

		if syncSet.DeletionTimestamp != nil {
			if controllerutils.HasFinalizer(&syncSet, hivev1.FinalizerSyncSetCleanup) {
				// Delete syncset resources
				if syncSet.Spec.ResourceDeletionPolicy != hivev1.OrphanResourceDeletionPolicy {
					syncSetStatus := findSyncSetStatus(syncSet.Name, cd.Status.SyncSetStatus)
					err := r.deleteSyncSetResources(syncSet.Spec.Resources, syncSetStatus, dynamicClient, ssLog)
					if err != nil {
						ssLog.WithError(err).Error("unable to cleanup syncset resources")
					}
				}
				// Remove syncset status from clusterdeployment
				cd.Status.SyncSetStatus = removeSyncSetObjectStatus(cd.Status.SyncSetStatus, syncSet.Name)
				if err := r.removeSyncSetFinalizer(&syncSet); err != nil {
					ssLog.WithError(err).Error("unable to remove finalizer")
					return reconcile.Result{}, err
				}
				continue
			}
		}

		// Add syncset cleanup finalizer if not present
		if !controllerutils.HasFinalizer(&syncSet, hivev1.FinalizerSyncSetCleanup) {
			ssLog.Debugf("adding syncset finalizer")
			if err := r.addSyncSetFinalizer(&syncSet); err != nil {
				ssLog.WithError(err).Error("error adding finalizer")
				return reconcile.Result{}, err
			}
			continue
		}

		ssLog.Debug("applying sync set")

		syncSetStatus := findSyncSetStatus(syncSet.Name, cd.Status.SyncSetStatus)
		applier := r.applierBuilder(kubeConfig, cdLog)
		err = r.applySyncSetResources(syncSet.Spec.ResourceApplyMode, syncSet.Spec.Resources, dynamicClient, applier, &syncSetStatus, ssLog)
		if err != nil {
			ssLog.WithError(err).Error("unable to apply sync set resources")
			// skip applying sync set patches when resources could not be applied
			cd.Status.SyncSetStatus = appendOrUpdateSyncSetObjectStatus(cd.Status.SyncSetStatus, syncSetStatus)
			if firstSyncSetErr == nil {
				firstSyncSetErr = err
			}
			continue
		}
		err = r.applySyncSetPatches(syncSet.Spec.Patches, kubeConfig, &syncSetStatus, ssLog)
		if err != nil {
			ssLog.WithError(err).Error("unable to apply sync set patches")
			if firstSyncSetErr == nil {
				firstSyncSetErr = err
			}
		}
		cd.Status.SyncSetStatus = appendOrUpdateSyncSetObjectStatus(cd.Status.SyncSetStatus, syncSetStatus)
	}

	for _, selectorSyncSet := range selectorSyncSets {
		ssLog := cdLog.WithFields(log.Fields{"selectorSyncSet": selectorSyncSet.Name})

		if selectorSyncSet.DeletionTimestamp != nil {
			if controllerutils.HasFinalizer(&selectorSyncSet, hivev1.FinalizerSyncSetCleanup) {
				// Delete syncset resources
				if selectorSyncSet.Spec.ResourceDeletionPolicy != hivev1.OrphanResourceDeletionPolicy {
					syncSetStatus := findSyncSetStatus(selectorSyncSet.Name, cd.Status.SyncSetStatus)
					err := r.deleteSyncSetResources(selectorSyncSet.Spec.Resources, syncSetStatus, dynamicClient, ssLog)
					if err != nil {
						ssLog.WithError(err).Error("unable to cleanup syncset resources")
					}
				}
				// Remove syncset status from clusterdeployment
				cd.Status.SyncSetStatus = removeSyncSetObjectStatus(cd.Status.SyncSetStatus, selectorSyncSet.Name)
				if err := r.removeSelectorSyncSetFinalizer(&selectorSyncSet); err != nil {
					ssLog.WithError(err).Error("unable to remove finalizer")
					return reconcile.Result{}, err
				}
				continue
			}
		}

		// Add syncset cleanup finalizer if not present
		if !controllerutils.HasFinalizer(&selectorSyncSet, hivev1.FinalizerSyncSetCleanup) {
			ssLog.Debugf("adding syncset finalizer")
			if err := r.addSelectorSyncSetFinalizer(&selectorSyncSet); err != nil {
				ssLog.WithError(err).Error("error adding finalizer")
				return reconcile.Result{}, err
			}
			continue
		}

		ssLog.Debug("applying selector sync set")

		syncSetStatus := findSyncSetStatus(selectorSyncSet.Name, cd.Status.SelectorSyncSetStatus)
		applier := r.applierBuilder(kubeConfig, cdLog)
		err = r.applySyncSetResources(selectorSyncSet.Spec.ResourceApplyMode, selectorSyncSet.Spec.Resources, dynamicClient, applier, &syncSetStatus, ssLog)
		if err != nil {
			ssLog.WithError(err).Error("unable to apply selector sync set resources")
			// skip applying selector sync set patches when resources could not be applied
			cd.Status.SelectorSyncSetStatus = appendOrUpdateSyncSetObjectStatus(cd.Status.SelectorSyncSetStatus, syncSetStatus)
			if firstSyncSetErr == nil {
				firstSyncSetErr = err
			}
			continue
		}
		err = r.applySyncSetPatches(selectorSyncSet.Spec.Patches, kubeConfig, &syncSetStatus, ssLog)
		if err != nil {
			ssLog.WithError(err).Error("unable to apply selector sync set patches")
			if firstSyncSetErr == nil {
				firstSyncSetErr = err
			}
		}
		cd.Status.SelectorSyncSetStatus = appendOrUpdateSyncSetObjectStatus(cd.Status.SelectorSyncSetStatus, syncSetStatus)
	}

	err = r.updateClusterDeploymentStatus(cd, origCD, cdLog)
	if err != nil {
		cdLog.WithError(err).Errorf("error updating cluster deployment status")
		return reconcile.Result{}, err
	}

	cdLog.WithError(err).Info("done reconciling sync sets for cluster deployment")
	return reconcile.Result{}, firstSyncSetErr
}

// applySyncSetResources evaluates resource objects from RawExtension and applies them to the cluster identified by kubeConfig
func (r *ReconcileSyncSet) applySyncSetResources(applyMode hivev1.SyncSetResourceApplyMode, ssResources []runtime.RawExtension, dynamicClient dynamic.Interface, h Applier, syncSetStatus *hivev1.SyncSetObjectStatus, ssLog log.FieldLogger) error {
	// determine if we can gather info for all resources
	infos := []hiveresource.Info{}
	for i, resource := range ssResources {
		info, err := h.Info(resource.Raw)
		if err != nil {
			// error gathering resource info, set UnknownObjectSyncCondition within syncSetStatus conditions
			syncSetStatus.Conditions = r.setUnknownObjectSyncCondition(syncSetStatus.Conditions, err, i)
			return err
		}
		infos = append(infos, *info)
	}

	syncSetStatus.Conditions = r.setUnknownObjectSyncCondition(syncSetStatus.Conditions, nil, 0)
	syncStatusList := []hivev1.SyncStatus{}

	var applyErr error
	for i, resource := range ssResources {
		resourceSyncStatus := hivev1.SyncStatus{
			APIVersion: infos[i].APIVersion,
			Kind:       infos[i].Kind,
			Resource:   infos[i].Resource,
			Name:       infos[i].Name,
			Namespace:  infos[i].Namespace,
			Hash:       r.hash(resource.Raw),
		}

		var resourceSyncConditions []hivev1.SyncCondition

		// determine if resource is found, different or should be reapplied based on last probe time
		found := false
		different := false
		shouldReApply := false
		for _, rss := range syncSetStatus.Resources {
			if rss.Name == resourceSyncStatus.Name &&
				rss.Namespace == resourceSyncStatus.Namespace &&
				rss.APIVersion == resourceSyncStatus.APIVersion &&
				rss.Kind == resourceSyncStatus.Kind {
				resourceSyncConditions = rss.Conditions
				found = true
				if rss.Hash != resourceSyncStatus.Hash {
					ssLog.Debugf("Resource %s/%s (%s) has changed, will re-apply", infos[i].Namespace, infos[i].Name, infos[i].Kind)
					different = true
					break
				}

				// re-apply if failure occurred
				if failureCondition := controllerutils.FindSyncCondition(rss.Conditions, hivev1.ApplyFailureSyncCondition); failureCondition != nil {
					if failureCondition.Status == corev1.ConditionTrue {
						ssLog.Debugf("Resource %s/%s (%s) failed last time, will re-apply", infos[i].Namespace, infos[i].Name, infos[i].Kind)
						shouldReApply = true
						break
					}
				}

				// re-apply if two hours have passed since LastProbeTime
				if applySuccessCondition := controllerutils.FindSyncCondition(rss.Conditions, hivev1.ApplySuccessSyncCondition); applySuccessCondition != nil {
					since := time.Since(applySuccessCondition.LastProbeTime.Time)
					if since > reapplyInterval {
						ssLog.Debugf("It has been %v since resource %s/%s (%s) was last applied, will re-apply", since, infos[i].Namespace, infos[i].Name, infos[i].Kind)
						shouldReApply = true
					}
				}
				break
			}
		}

		if !found || different || shouldReApply {
			ssLog.Debugf("applying resource: %s/%s (%s)", infos[i].Namespace, infos[i].Name, infos[i].Kind)
			var result hiveresource.ApplyResult
			result, applyErr = h.Apply(resource.Raw)
			resourceSyncStatus.Conditions = r.setApplySyncConditions(resourceSyncConditions, applyErr)
			if applyErr != nil {
				ssLog.WithError(applyErr).Errorf("error applying resource %s/%s (%s)", infos[i].Namespace, infos[i].Name, infos[i].Kind)
			} else {
				ssLog.Debug("resource %s/%s (%s): %s", infos[i].Namespace, infos[i].Name, infos[i].Kind, result)
			}
		} else {
			ssLog.Debugf("resource %s/%s (%s) has not changed, will not apply", infos[i].Namespace, infos[i].Name, infos[i].Kind)
			resourceSyncStatus.Conditions = resourceSyncConditions
		}

		syncStatusList = append(syncStatusList, resourceSyncStatus)

		// If an error applying occurred, stop processing right here
		if applyErr != nil {
			break
		}
	}

	var delErr error
	syncSetStatus.Resources, delErr = r.reconcileDeletedSyncSetResources(applyMode, dynamicClient, syncSetStatus.Resources, syncStatusList, applyErr, ssLog)
	if delErr != nil {
		ssLog.WithError(delErr).Error("error reconciling syncset resources")
		return delErr
	}

	// We've saved apply and deletion errors separately, if either are present return an error for the controller to trigger retries
	// and go into exponential backoff if the problem does not resolve itself.
	if applyErr != nil {
		return applyErr
	}
	if delErr != nil {
		return delErr
	}

	return nil
}

// applySyncSetPatches applies patches to cluster identified by kubeConfig
func (r *ReconcileSyncSet) applySyncSetPatches(ssPatches []hivev1.SyncObjectPatch, kubeConfig []byte, syncSetStatus *hivev1.SyncSetObjectStatus, ssLog log.FieldLogger) error {
	h := r.applierBuilder(kubeConfig, r.logger)

	for _, ssPatch := range ssPatches {
		patchSyncStatus := hivev1.SyncStatus{
			APIVersion: ssPatch.APIVersion,
			Kind:       ssPatch.Kind,
			Name:       ssPatch.Name,
			Namespace:  ssPatch.Namespace,
			Hash:       r.hash([]byte(ssPatch.Patch)),
		}

		patchSyncConditions := []hivev1.SyncCondition{}

		// determine if patch is found, different or should be reapplied based on patch apply mode
		found := false
		different := false
		shouldReApply := false
		for _, pss := range syncSetStatus.Patches {
			if pss.Name == patchSyncStatus.Name && pss.Namespace == patchSyncStatus.Namespace && pss.Kind == patchSyncStatus.Kind {
				patchSyncConditions = pss.Conditions
				found = true
				if pss.Hash != patchSyncStatus.Hash {
					ssLog.Debugf("Patch %s/%s (%s) has changed, will re-apply", ssPatch.Namespace, ssPatch.Name, ssPatch.Kind)
					different = true
					break
				}

				// re-apply if failure occurred
				if failureCondition := controllerutils.FindSyncCondition(pss.Conditions, hivev1.ApplyFailureSyncCondition); failureCondition != nil {
					if failureCondition.Status == corev1.ConditionTrue {
						ssLog.Debugf("Patch %s/%s (%s) failed last time, will re-apply", ssPatch.Namespace, ssPatch.Name, ssPatch.Kind)
						shouldReApply = true
						break
					}
				}

				// re-apply if two hours have passed since LastProbeTime and patch apply mode is not apply once
				if ssPatch.ApplyMode != hivev1.ApplyOncePatchApplyMode {
					if applySuccessCondition := controllerutils.FindSyncCondition(pss.Conditions, hivev1.ApplySuccessSyncCondition); applySuccessCondition != nil {
						since := time.Since(applySuccessCondition.LastProbeTime.Time)
						if since > reapplyInterval {
							ssLog.Debugf("It has been %v since resource %s/%s (%s) was last applied, will re-apply", since, ssPatch.Namespace, ssPatch.Name, ssPatch.Kind)
							shouldReApply = true
						}
					}
				}
				break
			}
		}

		if !found || different || shouldReApply {
			ssLog.Debugf("applying patch: %s/%s (%s)", ssPatch.Namespace, ssPatch.Name, ssPatch.Kind)
			namespacedName := types.NamespacedName{
				Name:      ssPatch.Name,
				Namespace: ssPatch.Namespace,
			}
			err := h.Patch(namespacedName, ssPatch.Kind, ssPatch.APIVersion, []byte(ssPatch.Patch), ssPatch.PatchType)
			patchSyncStatus.Conditions = r.setApplySyncConditions(patchSyncConditions, err)
			syncSetStatus.Patches = appendOrUpdateSyncStatus(syncSetStatus.Patches, patchSyncStatus)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ReconcileSyncSet) reconcileDeletedSyncSetResources(applyMode hivev1.SyncSetResourceApplyMode, dynamicClient dynamic.Interface, existingStatusList, newStatusList []hivev1.SyncStatus, err error, ssLog log.FieldLogger) ([]hivev1.SyncStatus, error) {
	ssLog.Debugf("reconciling syncset resources, existing: %d, actual: %d", len(existingStatusList), len(newStatusList))
	if applyMode == "" || applyMode == hivev1.UpsertResourceApplyMode {
		ssLog.Debugf("apply mode is upsert, syncset status will be updated")
		return newStatusList, nil
	}
	deletedStatusList := []hivev1.SyncStatus{}
	deletedStatusIndices := []int{}
	for i, existingStatus := range existingStatusList {
		found := false
		for _, newStatus := range newStatusList {
			if existingStatus.Name == newStatus.Name &&
				existingStatus.Namespace == newStatus.Namespace &&
				existingStatus.APIVersion == newStatus.APIVersion &&
				existingStatus.Kind == newStatus.Kind {
				found = true
				break
			}
		}
		if !found {
			ssLog.WithField("resource", fmt.Sprintf("%s/%s", existingStatus.Namespace, existingStatus.Name)).
				WithField("apiversion", existingStatus.APIVersion).
				WithField("kind", existingStatus.Kind).Debug("resource not found in updated status, will queue up for deletion")
			deletedStatusList = append(deletedStatusList, existingStatus)
			deletedStatusIndices = append(deletedStatusIndices, i)
		}
	}

	// If an error occurred applying resources, do not delete yet
	if err != nil {
		ssLog.Debugf("an error occurred applying resources, will preserve all syncset status items")
		return append(newStatusList, deletedStatusList...), nil
	}

	for i, deletedStatus := range deletedStatusList {
		itemLog := ssLog.WithField("resource", fmt.Sprintf("%s/%s", deletedStatus.Namespace, deletedStatus.Name)).
			WithField("apiversion", deletedStatus.APIVersion).
			WithField("kind", deletedStatus.Kind)
		gv, err := schema.ParseGroupVersion(deletedStatus.APIVersion)
		if err != nil {
			return nil, err
		}
		gvr := gv.WithResource(deletedStatus.Resource)
		itemLog.Debug("deleting resource")
		err = dynamicClient.Resource(gvr).Namespace(deletedStatus.Namespace).Delete(deletedStatus.Name, &metav1.DeleteOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				itemLog.WithError(err).Error("error deleting resource")
				index := deletedStatusIndices[i]
				existingStatusList[index].Conditions = r.setDeletionFailedSyncCondition(existingStatusList[index].Conditions, err)
			} else {
				itemLog.Debug("resource not found, nothing to do")
			}
		}
	}

	return newStatusList, nil
}

func (r *ReconcileSyncSet) deleteSyncSetResources(ssResources []runtime.RawExtension, syncSetStatus hivev1.SyncSetObjectStatus, dynamicClient dynamic.Interface, ssLog log.FieldLogger) error {
	for _, resourceStatus := range syncSetStatus.Resources {
		itemLog := ssLog.WithField("resource", fmt.Sprintf("%s/%s", resourceStatus.Namespace, resourceStatus.Name)).
			WithField("apiversion", resourceStatus.APIVersion).
			WithField("kind", resourceStatus.Kind)
		gv, err := schema.ParseGroupVersion(resourceStatus.APIVersion)
		if err != nil {
			// continue instead if the goal is a brute force cleanup?
			return err
		}
		gvr := gv.WithResource(resourceStatus.Resource)
		itemLog.Debug("deleting resource")
		err = dynamicClient.Resource(gvr).Namespace(resourceStatus.Namespace).Delete(resourceStatus.Name, &metav1.DeleteOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				itemLog.WithError(err).Error("error deleting resource")
				// what should we do when we encounter an error deleting resources for cleanup? set deletionfailed status?
				// existingStatusList[index].Conditions = r.setDeletionFailedSyncCondition(existingStatusList[index].Conditions, err)
			} else {
				itemLog.Debug("resource not found, nothing to do")
			}
		}
	}
	return nil
}

func appendOrUpdateSyncStatus(statusList []hivev1.SyncStatus, syncStatus hivev1.SyncStatus) []hivev1.SyncStatus {
	for i, ss := range statusList {
		if ss.Name == syncStatus.Name && ss.Namespace == syncStatus.Namespace && ss.Kind == syncStatus.Kind {
			statusList[i] = syncStatus
			return statusList
		}
	}
	return append(statusList, syncStatus)
}

func appendOrUpdateSyncSetObjectStatus(statusList []hivev1.SyncSetObjectStatus, syncSetObjectStatus hivev1.SyncSetObjectStatus) []hivev1.SyncSetObjectStatus {
	for i, ssos := range statusList {
		if ssos.Name == syncSetObjectStatus.Name {
			statusList[i] = syncSetObjectStatus
			return statusList
		}
	}
	return append(statusList, syncSetObjectStatus)
}

func findSyncSetStatus(name string, statusList []hivev1.SyncSetObjectStatus) hivev1.SyncSetObjectStatus {
	for _, ssos := range statusList {
		if name == ssos.Name {
			return ssos
		}
	}
	return hivev1.SyncSetObjectStatus{Name: name}
}

func removeSyncSetObjectStatus(statusList []hivev1.SyncSetObjectStatus, syncSetName string) []hivev1.SyncSetObjectStatus {
	for i := 0; i < len(statusList); i++ {
		if statusList[i].Name == syncSetName {
			statusList = append(statusList[:i], statusList[i+1:]...)
			i--
		}
	}
	return statusList
}

func (r *ReconcileSyncSet) updateClusterDeploymentStatus(cd *hivev1.ClusterDeployment, origCD *hivev1.ClusterDeployment, cdLog log.FieldLogger) error {
	// Update cluster deployment status if changed:
	if !reflect.DeepEqual(cd.Status, origCD.Status) {
		cdLog.Infof("status has changed, updating cluster deployment status")
		err := r.Status().Update(context.TODO(), cd)
		if err != nil {
			cdLog.Errorf("error updating cluster deployment status: %v", err)
			return err
		}
	}
	return nil
}

func (r *ReconcileSyncSet) setUnknownObjectSyncCondition(syncSetConditions []hivev1.SyncCondition, err error, index int) []hivev1.SyncCondition {
	status := corev1.ConditionFalse
	reason := unknownObjectNotFoundReason
	message := fmt.Sprintf("Info available for all SyncSet resources")
	if err != nil {
		status = corev1.ConditionTrue
		reason = unknownObjectFoundReason
		message = fmt.Sprintf("Unable to gather Info for SyncSet resource at index %v in resources: %v", index, err)
	}
	syncSetConditions = controllerutils.SetSyncCondition(
		syncSetConditions,
		hivev1.UnknownObjectSyncCondition,
		status,
		reason,
		message,
		controllerutils.UpdateConditionNever)
	return syncSetConditions
}

func (r *ReconcileSyncSet) setApplySyncConditions(resourceSyncConditions []hivev1.SyncCondition, err error) []hivev1.SyncCondition {
	var reason, message string
	var successStatus, failureStatus corev1.ConditionStatus
	var updateCondition controllerutils.UpdateConditionCheck
	if err == nil {
		reason = applySucceededReason
		message = "Apply successful"
		successStatus = corev1.ConditionTrue
		failureStatus = corev1.ConditionFalse
		updateCondition = controllerutils.UpdateConditionAlways
	} else {
		reason = applyFailedReason
		// TODO: we cannot include the actual error here as it currently contains a temp filename which always changes,
		// which triggers a hotloop by always updating status and then reconciling again. If we were to filter out the portion
		// of the error message with filename, we could re-add this here.
		message = "Apply failed"
		successStatus = corev1.ConditionFalse
		failureStatus = corev1.ConditionTrue
		updateCondition = controllerutils.UpdateConditionIfReasonOrMessageChange
	}
	resourceSyncConditions = controllerutils.SetSyncCondition(
		resourceSyncConditions,
		hivev1.ApplySuccessSyncCondition,
		successStatus,
		reason,
		message,
		updateCondition)
	resourceSyncConditions = controllerutils.SetSyncCondition(
		resourceSyncConditions,
		hivev1.ApplyFailureSyncCondition,
		failureStatus,
		reason,
		message,
		updateCondition)

	// If we are reporting that apply succeeded or failed, it means we no longer
	// want to delete this resource. Set that failure condition to false in case
	// it was previously set to true.
	resourceSyncConditions = controllerutils.SetSyncCondition(
		resourceSyncConditions,
		hivev1.DeletionFailedSyncCondition,
		corev1.ConditionFalse,
		reason,
		message,
		updateCondition)
	return resourceSyncConditions
}

func (r *ReconcileSyncSet) setDeletionFailedSyncCondition(resourceSyncConditions []hivev1.SyncCondition, err error) []hivev1.SyncCondition {
	if err == nil {
		return resourceSyncConditions
	}
	return controllerutils.SetSyncCondition(
		resourceSyncConditions,
		hivev1.DeletionFailedSyncCondition,
		corev1.ConditionTrue,
		deletionFailedReason,
		fmt.Sprintf("Failed to delete resource: %v", err),
		controllerutils.UpdateConditionAlways)
}

func (r *ReconcileSyncSet) getRelatedSelectorSyncSets(cd *hivev1.ClusterDeployment) ([]hivev1.SelectorSyncSet, error) {
	list := &hivev1.SelectorSyncSetList{}
	err := r.Client.List(context.TODO(), &client.ListOptions{}, list)
	if err != nil {
		return nil, err
	}

	cdLabels := labels.Set(cd.Labels)
	selectorSyncSets := []hivev1.SelectorSyncSet{}
	for _, selectorSyncSet := range list.Items {
		labelSelector, err := metav1.LabelSelectorAsSelector(&selectorSyncSet.Spec.ClusterDeploymentSelector)
		if err != nil {
			r.logger.WithError(err).Error("unable to convert selector")
			continue
		}

		if labelSelector.Matches(cdLabels) {
			selectorSyncSets = append(selectorSyncSets, selectorSyncSet)
		}
	}

	return selectorSyncSets, err
}

func (r *ReconcileSyncSet) getRelatedSyncSets(cd *hivev1.ClusterDeployment) ([]hivev1.SyncSet, error) {
	list := &hivev1.SyncSetList{}
	err := r.Client.List(context.TODO(), &client.ListOptions{Namespace: cd.Namespace}, list)
	if err != nil {
		return nil, err
	}

	syncSets := []hivev1.SyncSet{}
	for _, syncSet := range list.Items {
		for _, cdr := range syncSet.Spec.ClusterDeploymentRefs {
			if cdr.Name == cd.Name {
				syncSets = append(syncSets, syncSet)
				break
			}
		}
	}

	return syncSets, err
}

func (r *ReconcileSyncSet) loadSecretData(secretName, namespace, dataKey string) (string, error) {
	s := &kapi.Secret{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: secretName, Namespace: namespace}, s)
	if err != nil {
		return "", err
	}
	retStr, ok := s.Data[dataKey]
	if !ok {
		return "", fmt.Errorf("secret %s did not contain key %s", secretName, dataKey)
	}
	return string(retStr), nil
}

func (r *ReconcileSyncSet) resourceHash(data []byte) string {
	return fmt.Sprintf("%x", md5.Sum(data))
}

var (
	oldPatchTypes = map[string]string{
		string(types.JSONPatchType):           "json",
		string(types.MergePatchType):          "merge",
		string(types.StrategicMergePatchType): "strategic",
	}
)

func (r *ReconcileSyncSet) migrateSyncSetPatchTypes(items []hivev1.SyncSet, logger log.FieldLogger) error {
	for i, syncSet := range items {
		if len(syncSet.Spec.Patches) == 0 {
			continue
		}
		migrated := false
		for j, patch := range syncSet.Spec.Patches {
			if newType, isOldType := oldPatchTypes[patch.PatchType]; isOldType {
				items[i].Spec.Patches[j].PatchType = newType
				migrated = true
			}
		}
		if migrated {
			logger.Infof("Migrating syncset %s/%s with outdated patch type", syncSet.Namespace, syncSet.Name)
			err := r.Update(context.TODO(), &items[i])
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ReconcileSyncSet) migrateSelectorSyncSetPatchTypes(items []hivev1.SelectorSyncSet, logger log.FieldLogger) error {
	for i, selectorSyncSet := range items {
		if len(selectorSyncSet.Spec.Patches) == 0 {
			continue
		}
		migrated := false
		for j, patch := range selectorSyncSet.Spec.Patches {
			if newType, isOldType := oldPatchTypes[patch.PatchType]; isOldType {
				items[i].Spec.Patches[j].PatchType = newType
				migrated = true
			}
		}
		if migrated {
			logger.Infof("Migrating selector syncset %s/%s with outdated patch type", selectorSyncSet.Namespace, selectorSyncSet.Name)
			err := r.Update(context.TODO(), &items[i])
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ReconcileSyncSet) addSyncSetFinalizer(ss *hivev1.SyncSet) error {
	ss = ss.DeepCopy()
	controllerutils.AddFinalizer(ss, hivev1.FinalizerSyncSetCleanup)
	return r.Update(context.TODO(), ss)
}

func (r *ReconcileSyncSet) removeSyncSetFinalizer(ss *hivev1.SyncSet) error {
	ss = ss.DeepCopy()
	controllerutils.DeleteFinalizer(ss, hivev1.FinalizerSyncSetCleanup)
	return r.Update(context.TODO(), ss)
}

func (r *ReconcileSyncSet) addSelectorSyncSetFinalizer(ss *hivev1.SelectorSyncSet) error {
	ss = ss.DeepCopy()
	controllerutils.AddFinalizer(ss, hivev1.FinalizerSyncSetCleanup)
	return r.Update(context.TODO(), ss)
}

func (r *ReconcileSyncSet) removeSelectorSyncSetFinalizer(ss *hivev1.SelectorSyncSet) error {
	ss = ss.DeepCopy()
	controllerutils.DeleteFinalizer(ss, hivev1.FinalizerSyncSetCleanup)
	return r.Update(context.TODO(), ss)
}

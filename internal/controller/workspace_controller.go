/*
Copyright 2026.

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/utkarsh2801/docker-workspaces/api/v1alpha1"
	"github.com/utkarsh2801/docker-workspaces/internal/compute"
)

const (
	containerFinalizerName = "workspaces.example.com/container"
	readyConditionType     = "Ready"
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Compute compute.Provider
}

// +kubebuilder:rbac:groups=workspaces.example.com,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.example.com,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.example.com,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves the Kubernetes resources for a Workspace toward the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	var ws workspacesv1alpha1.Workspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	uid := string(ws.UID)

	if !ws.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&ws, containerFinalizerName) {
			return ctrl.Result{}, nil
		}
		logf.Log.Info("Deleting workspace", "uid", uid)
		if err := r.Compute.Delete(ctx, compute.DeleteRequest{
			UID:           uid,
			RetainStorage: ws.Spec.StoragePolicy == workspacesv1alpha1.StoragePolicyRetain,
		}); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&ws, containerFinalizerName)
		return ctrl.Result{}, r.Update(ctx, &ws)
	}

	if !controllerutil.ContainsFinalizer(&ws, containerFinalizerName) {
		controllerutil.AddFinalizer(&ws, containerFinalizerName)
		return ctrl.Result{}, r.Update(ctx, &ws)
	}

	if _, err := r.Compute.Get(ctx, uid); err != nil && !errors.Is(err, compute.ErrNotFound) {
		return ctrl.Result{}, r.updateFailureStatus(ctx, &ws, "GetWorkspaceInstance", err)
	}

	controller := true
	logf.Log.Info("Ensuring workspace", "uid", uid)
	inst, err := r.Compute.Create(ctx, compute.CreateRequest{
		UID:         uid,
		Name:        ws.Name,
		Namespace:   ws.Namespace,
		Owner:       ws.Spec.Owner,
		Image:       ws.Spec.Image,
		Port:        int(ws.Spec.Port),
		Args:        ws.Spec.Args,
		StorageSize: ws.Spec.StorageSize,
		OwnerReference: metav1.OwnerReference{
			APIVersion: workspacesv1alpha1.GroupVersion.String(),
			Kind:       "Workspace",
			Name:       ws.Name,
			UID:        ws.UID,
			Controller: &controller,
		},
	})

	if err != nil {
		return ctrl.Result{}, r.updateFailureStatus(ctx, &ws, "CreateWorkspaceInstance", err)
	}

	if inst.State == compute.StateRunning && inst.IP != "" {
		if err := r.ensureRouting(ctx, &ws, inst.IP); err != nil {
			return ctrl.Result{}, r.updateFailureStatus(ctx, &ws, "ConfigureWorkspaceRoute", err)
		}
	}

	want := workspacesv1alpha1.WorkspaceStatus{
		Phase:       workspacesv1alpha1.PhaseCreating,
		ContainerID: inst.ID,
		IP:          inst.IP,
		URL:         inst.URL,
		Message:     "container" + inst.State,
		Conditions:  append([]metav1.Condition(nil), ws.Status.Conditions...),
	}

	if inst.State == compute.StateFailed {
		want.Phase = workspacesv1alpha1.PhaseFailed
		want.Message = "workspace setup failed"
		apimeta.SetStatusCondition(&want.Conditions, metav1.Condition{
			Type:               readyConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: ws.Generation,
			Reason:             "SetupFailed",
			Message:            "Node setup init container failed",
			LastTransitionTime: metav1.Now(),
		})
	} else if inst.State == compute.StateRunning {
		want.Phase = workspacesv1alpha1.PhaseRunning
		if inst.IP != "" {
			want.URL = routeURL(&ws)
		}
		apimeta.SetStatusCondition(&want.Conditions, metav1.Condition{
			Type:               readyConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: ws.Generation,
			Reason:             "WorkspaceRunning",
			Message:            "Workspace container is running",
			LastTransitionTime: metav1.Now(),
		})
	} else {
		apimeta.SetStatusCondition(&want.Conditions, metav1.Condition{
			Type:               readyConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: ws.Generation,
			Reason:             "WorkspaceProgressing",
			Message:            "Workspace container is not running",
			LastTransitionTime: metav1.Now(),
		})
	}

	if !apiequality.Semantic.DeepEqual(ws.Status, want) {
		ws.Status = want
		if err := r.Status().Update(ctx, &ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	if want.Phase != workspacesv1alpha1.PhaseRunning {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func (r *WorkspaceReconciler) updateFailureStatus(ctx context.Context, ws *workspacesv1alpha1.Workspace, reason string, reconcileErr error) error {
	ws.Status.Phase = workspacesv1alpha1.PhaseFailed
	ws.Status.Message = reconcileErr.Error()
	apimeta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ws.Generation,
		Reason:             reason,
		Message:            reconcileErr.Error(),
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, ws); err != nil {
		return errors.Join(reconcileErr, fmt.Errorf("update failure status: %w", err))
	}
	return reconcileErr
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Workspace{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Endpoints{}). //nolint:staticcheck // Pomerium still discovers backends from core Endpoints.
		Owns(&networkingv1.Ingress{}).
		Named("workspace").
		Complete(r)
}

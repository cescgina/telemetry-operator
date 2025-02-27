/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"

	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	logr "github.com/go-logr/logr"
	common "github.com/openstack-k8s-operators/lib-common/modules/common"
	condition "github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	helper "github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"k8s.io/client-go/kubernetes"

	telemetryv1 "github.com/openstack-k8s-operators/telemetry-operator/api/v1beta1"
	telemetry "github.com/openstack-k8s-operators/telemetry-operator/pkg/telemetry"
)

// TelemetryReconciler reconciles a Telemetry object
type TelemetryReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	Scheme  *runtime.Scheme
}

// GetLogger returns a logger object with a prefix of "conroller.name" and aditional controller context fields
func (r *TelemetryReconciler) GetLogger(ctx context.Context) logr.Logger {
	return log.FromContext(ctx).WithName("Controllers").WithName("Telemetry")
}

// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=telemetries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=telemetries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=telemetries/finalizers,verbs=update
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=autoscalings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=autoscalings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=autoscalings/finalizers,verbs=update;delete
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=ceilometers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=ceilometers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=telemetry.openstack.org,resources=ceilometers/finalizers,verbs=update;delete
// +kubebuilder:rbac:groups=rabbitmq.openstack.org,resources=transporturls,verbs=get;list;watch;create;update;patch;delete

// Reconcile reconciles a Telemetry
func (r *TelemetryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, _err error) {
	Log := r.GetLogger(ctx)

	// Fetch the Telemetry instance
	instance := &telemetryv1.Telemetry{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected.
			// For additional cleanup logic use finalizers. Return and don't requeue.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		// update the Ready condition based on the sub conditions
		if instance.Status.Conditions.AllSubConditionIsTrue() {
			instance.Status.Conditions.MarkTrue(
				condition.ReadyCondition, condition.ReadyMessage)
		} else {
			// something is not ready so reset the Ready condition
			instance.Status.Conditions.MarkUnknown(
				condition.ReadyCondition, condition.InitReason, condition.ReadyInitMessage)
			// and recalculate it based on the state of the rest of the conditions
			instance.Status.Conditions.Set(
				instance.Status.Conditions.Mirror(condition.ReadyCondition))
		}
		err := helper.PatchInstance(ctx, instance)
		if err != nil {
			_err = err
			return
		}
	}()

	// If we're not deleting this and the service object doesn't have our finalizer, add it.
	if instance.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(instance, helper.GetFinalizer()) {
		return ctrl.Result{}, nil
	}

	//
	// initialize status
	//
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = condition.Conditions{}
		// initialize conditions used later as Status=Unknown
		cl := condition.CreateList(
			condition.UnknownCondition(telemetryv1.CeilometerReadyCondition, condition.InitReason, telemetryv1.CeilometerReadyInitMessage),
			condition.UnknownCondition(telemetryv1.AutoscalingReadyCondition, condition.InitReason, telemetryv1.AutoscalingReadyInitMessage),
		)

		instance.Status.Conditions.Init(&cl)

		// Register overall status immediately to have an early feedback e.g. in the cli
		return ctrl.Result{}, nil
	}

	if instance.Status.Hash == nil {
		instance.Status.Hash = map[string]string{}
	}

	serviceLabels := map[string]string{
		common.AppSelector: telemetry.ServiceName,
	}

	// Handle service init
	ctrlResult, err := r.reconcileInit(ctx, instance, helper, serviceLabels)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Handle service delete
	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, instance, helper)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, instance, helper)
}

func (r *TelemetryReconciler) reconcileDelete(ctx context.Context, instance *telemetryv1.Telemetry, helper *helper.Helper) (ctrl.Result, error) {
	Log := r.GetLogger(ctx)
	Log.Info(fmt.Sprintf("Reconciled Service '%s' delete", instance.Name))

	// Service is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(instance, helper.GetFinalizer())

	Log.Info(fmt.Sprintf("Reconciled Service '%s' delete successfully", instance.Name))
	return ctrl.Result{}, nil
}

func (r *TelemetryReconciler) reconcileInit(
	ctx context.Context,
	instance *telemetryv1.Telemetry,
	helper *helper.Helper,
	serviceLabels map[string]string,
) (ctrl.Result, error) {
	Log := r.GetLogger(ctx)
	Log.Info("Reconciling Service init")

	Log.Info("Reconciled Service init successfully")
	return ctrl.Result{}, nil
}

func (r *TelemetryReconciler) reconcileNormal(ctx context.Context, instance *telemetryv1.Telemetry, helper *helper.Helper) (ctrl.Result, error) {
	Log := r.GetLogger(ctx)
	Log.Info(fmt.Sprintf("Reconciling Service '%s'", instance.Name))

	// deploy autoscaling
	autoscaling, op, err := r.autoscalingCreateOrUpdate(instance)
	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			telemetryv1.AutoscalingReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			telemetryv1.AutoscalingReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		Log.Info(fmt.Sprintf("Deployment %s successfully reconciled - operation: %s", instance.Name, string(op)))
	}
	// Mirror autoscaling's condition status
	as := autoscaling.Status.Conditions.Mirror(telemetryv1.AutoscalingReadyCondition)
	if as != nil {
		instance.Status.Conditions.Set(as)
	}
	// end deploy autoscaling

	// deploy ceilometer
	ceilometer, op, err := r.ceilometerCreateOrUpdate(instance)
	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			telemetryv1.CeilometerReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			telemetryv1.CeilometerReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		Log.Info(fmt.Sprintf("Deployment %s successfully reconciled - operation: %s", instance.Name, string(op)))
	}

	// Mirror ceilometer's status ReadyCount to this parent CR
	instance.Status.CeilometerReadyCount = ceilometer.Status.ReadyCount

	// Mirror ceilometer's condition status
	ccentral := ceilometer.Status.Conditions.Mirror(telemetryv1.CeilometerReadyCondition)
	if ccentral != nil {
		instance.Status.Conditions.Set(ccentral)
	}
	// end deploy ceilometer

	Log.Info("Reconciled Service successfully")
	return ctrl.Result{}, nil
}

func (r *TelemetryReconciler) autoscalingCreateOrUpdate(instance *telemetryv1.Telemetry) (*telemetryv1.Autoscaling, controllerutil.OperationResult, error) {
	autoscaling := &telemetryv1.Autoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-autoscaling", instance.Name),
			Namespace: instance.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(context.TODO(), r.Client, autoscaling, func() error {
		autoscaling.Spec = instance.Spec.Autoscaling

		err := controllerutil.SetControllerReference(instance, autoscaling, r.Scheme)
		if err != nil {
			return err
		}

		return nil
	})

	return autoscaling, op, err
}

func (r *TelemetryReconciler) ceilometerCreateOrUpdate(instance *telemetryv1.Telemetry) (*telemetryv1.Ceilometer, controllerutil.OperationResult, error) {
	ccentral := &telemetryv1.Ceilometer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ceilometer-central", instance.Name),
			Namespace: instance.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(context.TODO(), r.Client, ccentral, func() error {
		ccentral.Spec = instance.Spec.Ceilometer

		err := controllerutil.SetControllerReference(instance, ccentral, r.Scheme)
		if err != nil {
			return err
		}

		return nil
	})

	return ccentral, op, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *TelemetryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&telemetryv1.Telemetry{}).
		Owns(&telemetryv1.Ceilometer{}).
		Owns(&telemetryv1.Autoscaling{}).
		Complete(r)
}

// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package resources

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	meshv2beta1 "github.com/hashicorp/consul-k8s/control-plane/api/mesh/v2beta1"
	"github.com/hashicorp/consul-k8s/control-plane/gateways"
)

// errResourceNotOwned indicates that a resource the controller would have
// updated or deleted does not have an owner reference pointing to the MeshGateway.
var errResourceNotOwned = errors.New("existing resource not owned by controller")

// MeshGatewayController reconciles a MeshGateway object.
type MeshGatewayController struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	Controller    *ConsulResourceController
	GatewayConfig gateways.GatewayConfig
}

// +kubebuilder:rbac:groups=mesh.consul.hashicorp.com,resources=meshgateway,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mesh.consul.hashicorp.com,resources=meshgateway/status,verbs=get;update;patch

func (r *MeshGatewayController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Logger(req.NamespacedName)

	// Fetch the resource being reconciled
	resource := &meshv2beta1.MeshGateway{}
	if err := r.Get(ctx, req.NamespacedName, resource); k8serr.IsNotFound(err) {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	} else if err != nil {
		logger.Error(err, "retrieving resource")
		return ctrl.Result{}, err
	}

	// Call hooks
	if !resource.GetDeletionTimestamp().IsZero() {
		logger.Info("deletion event")

		if err := r.onDelete(ctx, req, resource); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if err := r.onCreateUpdate(ctx, req, resource); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.Controller.ReconcileResource(ctx, r, req, &meshv2beta1.MeshGateway{})
}

func (r *MeshGatewayController) Logger(name types.NamespacedName) logr.Logger {
	return r.Log.WithValues("request", name)
}

func (r *MeshGatewayController) UpdateStatus(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return r.Status().Update(ctx, obj, opts...)
}

func (r *MeshGatewayController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&meshv2beta1.MeshGateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Complete(r)
}

// onCreateUpdate is responsible for creating/updating all K8s resources that
// are required in order to run a meshv2beta1.MeshGateway. These are created/updated
// in dependency order.
//  1. ServiceAccount
//  2. Deployment
//  3. Service
//  4. Role
//  5. RoleBinding
func (r *MeshGatewayController) onCreateUpdate(ctx context.Context, req ctrl.Request, resource *meshv2beta1.MeshGateway) error {
	// fetch gatewayclassconfig
	gcc, err := r.getGatewayClassConfigForGateway(ctx, resource)
	if err != nil {
		r.Log.Error(err, "unable to get gatewayclassconfig for gateway: %s gatewayclass: %s", resource.Name, resource.Spec.GatewayClassName)
		return err
	}

	builder := gateways.NewMeshGatewayBuilder(resource, r.GatewayConfig, gcc)

	upsertOp := func(ctx context.Context, _, object client.Object) error {
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, object, func() error { return nil })
		return err
	}

	err = r.opIfNewOrOwned(ctx, resource, &corev1.ServiceAccount{}, builder.ServiceAccount(), upsertOp)
	if err != nil {
		return fmt.Errorf("unable to create service account: %w", err)
	}

	// Create Role

	err = r.opIfNewOrOwned(ctx, resource, &rbacv1.Role{}, builder.Role(), upsertOp)
	if err != nil {
		return fmt.Errorf("unable to create role: %w", err)
	}

	// Create RoleBinding

	err = r.opIfNewOrOwned(ctx, resource, &rbacv1.RoleBinding{}, builder.RoleBinding(), upsertOp)
	if err != nil {
		return fmt.Errorf("unable to create role binding: %w", err)
	}

	// Create Service

	mergeServiceOp := func(ctx context.Context, existingObject, object client.Object) error {
		existingService, ok := existingObject.(*corev1.Service)
		if !ok && existingService != nil {
			return fmt.Errorf("unable to infer existing service type")
		}
		builtService, ok := object.(*corev1.Service)
		if !ok {
			return fmt.Errorf("unable to infer built service type")
		}

		mergedService := mergeService(existingService, builtService)

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, mergedService, func() error { return nil })
		return err
	}

	err = r.opIfNewOrOwned(ctx, resource, &corev1.Service{}, builder.Service(), mergeServiceOp)
	if err != nil {
		return fmt.Errorf("unable to create service: %w", err)
	}

	// Create deployment

	mergeDeploymentOp := func(ctx context.Context, existingObject, object client.Object) error {
		existingDeployment, ok := existingObject.(*appsv1.Deployment)
		if !ok && existingDeployment != nil {
			return fmt.Errorf("unable to infer existing deployment type")
		}
		builtDeployment, ok := object.(*appsv1.Deployment)
		if !ok {
			return fmt.Errorf("unable to infer built deployment type")
		}

		mergedDeployment := builder.MergeDeployments(gcc, existingDeployment, builtDeployment)

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, mergedDeployment, func() error { return nil })
		return err
	}

	builtDeployment, err := builder.Deployment()
	if err != nil {
		return fmt.Errorf("unable to build deployment: %w", err)
	}

	err = r.opIfNewOrOwned(ctx, resource, &appsv1.Deployment{}, builtDeployment, mergeDeploymentOp)
	if err != nil {
		return fmt.Errorf("unable to create deployment: %w", err)
	}

	return nil
}

// onDelete is responsible for cleaning up any side effects of onCreateUpdate.
// We only clean up side effects because all resources that we create explicitly
// have an owner reference and will thus be cleaned up by the K8s garbage collector
// once the owning meshv2beta1.MeshGateway is deleted.
func (r *MeshGatewayController) onDelete(ctx context.Context, req ctrl.Request, resource *meshv2beta1.MeshGateway) error {
	// TODO NET-6392 NET-6393
	return nil
}

// ownedObjectOp represents an operation that needs to be applied
// only if the newObject does not yet exist or if the existingObject
// has an owner reference pointing to the MeshGateway being reconciled.
//
// The existing and new object are available in case any merging needs
// to occur, such as unknown annotations and values from the existing object
// that need to be carried forward onto the new object.
type ownedObjectOp func(ctx context.Context, existingObject client.Object, newObject client.Object) error

// opIfNewOrOwned runs a given ownedObjectOp to create, update, or delete a resource.
// The purpose of opIfNewOrOwned is to ensure that we aren't updating or deleting a
// resource that was not created by us. If this scenario is encountered, we error.
func (r *MeshGatewayController) opIfNewOrOwned(ctx context.Context, gateway *meshv2beta1.MeshGateway, scanTarget, writeSource client.Object, op ownedObjectOp) error {
	// Ensure owner reference is always set on objects that we write
	if err := ctrl.SetControllerReference(gateway, writeSource, r.Client.Scheme()); err != nil {
		return err
	}

	key := client.ObjectKey{
		Namespace: writeSource.GetNamespace(),
		Name:      writeSource.GetName(),
	}

	exists := false
	if err := r.Get(ctx, key, scanTarget); err != nil {
		// We failed to fetch the object in a way that doesn't tell us about its existence
		if !k8serr.IsNotFound(err) {
			return err
		}
	} else {
		// We successfully fetched the object, so it exists
		exists = true
	}

	// None exists, so we need only execute the operation
	if !exists {
		return op(ctx, nil, writeSource)
	}

	// Ensure the existing object was put there by us so that we don't overwrite random objects
	owned := false
	for _, reference := range scanTarget.GetOwnerReferences() {
		if reference.UID == gateway.GetUID() && reference.Name == gateway.GetName() {
			owned = true
			break
		}
	}
	if !owned {
		return errResourceNotOwned
	}
	return op(ctx, scanTarget, writeSource)
}

func (r *MeshGatewayController) getGatewayClassConfigForGateway(ctx context.Context, gateway *meshv2beta1.MeshGateway) (*meshv2beta1.GatewayClassConfig, error) {
	gatewayClass, err := r.getGatewayClassForGateway(ctx, gateway)
	if err != nil {
		return nil, err
	}

	gatewayClassConfig, err := r.getGatewayClassConfigForGatewayClass(ctx, gatewayClass)
	if err != nil {
		return nil, err
	}

	return gatewayClassConfig, nil
}

func (r *MeshGatewayController) getGatewayClassConfigForGatewayClass(ctx context.Context, gatewayClass *meshv2beta1.GatewayClass) (*meshv2beta1.GatewayClassConfig, error) {
	if gatewayClass == nil {
		// if we don't have a gateway class we can't fetch the corresponding config
		return nil, nil
	}

	config := &meshv2beta1.GatewayClassConfig{}
	if ref := gatewayClass.Spec.ParametersRef; ref != nil {
		if ref.Group != meshv2beta1.MeshGroup || ref.Kind != "GatewayClassConfig" {
			// TODO @Gateway-Management additionally check for controller name when available
			return nil, nil
		}

		if err := r.Client.Get(ctx, types.NamespacedName{Name: ref.Name}, config); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
	}
	return config, nil
}

func (r *MeshGatewayController) getGatewayClassForGateway(ctx context.Context, gateway *meshv2beta1.MeshGateway) (*meshv2beta1.GatewayClass, error) {
	var gatewayClass meshv2beta1.GatewayClass

	if err := r.Client.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return &gatewayClass, nil
}

func areServicesEqual(a, b *corev1.Service) bool {
	// If either service "a" or "b" is nil, don't want to try and merge the nil service
	if a == nil || b == nil {
		return true
	}

	if !equality.Semantic.DeepEqual(a.Annotations, b.Annotations) {
		return false
	}

	if len(b.Spec.Ports) != len(a.Spec.Ports) {
		return false
	}

	for i, port := range a.Spec.Ports {
		otherPort := b.Spec.Ports[i]
		if port.Port != otherPort.Port || port.Protocol != otherPort.Protocol {
			return false
		}
	}
	return true
}

// mergeService is used to keep annotations and ports from the `from` Service
// to the `to` service. This prevents an infinite reconciliation loop when
// Kubernetes adds this configuration back in.
func mergeService(from, to *corev1.Service) *corev1.Service {
	if areServicesEqual(from, to) {
		return to
	}

	to.Annotations = from.Annotations
	to.Spec.Ports = from.Spec.Ports

	return to
}
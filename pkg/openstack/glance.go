package openstack

import (
	"context"
	"fmt"

	glancev1 "github.com/openstack-k8s-operators/glance-operator/api/v1beta1"
	"github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/service"
	corev1beta1 "github.com/openstack-k8s-operators/openstack-operator/apis/core/v1beta1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// svcSelector is used as selector to get the list of "Services" associated
	// to a specific glanceAPI instance
	svcSelector = "glanceAPI"
)

// ReconcileGlance -
func ReconcileGlance(ctx context.Context, instance *corev1beta1.OpenStackControlPlane, helper *helper.Helper) (ctrl.Result, error) {
	glance := &glancev1.Glance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "glance",
			Namespace: instance.Namespace,
		},
	}

	Log := GetLogger(ctx)

	if !instance.Spec.Glance.Enabled {
		if res, err := EnsureDeleted(ctx, helper, glance); err != nil {
			return res, err
		}
		instance.Status.Conditions.Remove(corev1beta1.OpenStackControlPlaneGlanceReadyCondition)
		instance.Status.Conditions.Remove(corev1beta1.OpenStackControlPlaneExposeGlanceReadyCondition)
		return ctrl.Result{}, nil
	}

	// When component services got created check if there is the need to create a route
	if err := helper.GetClient().Get(ctx, types.NamespacedName{Name: "glance", Namespace: instance.Namespace}, glance); err != nil {
		if !k8s_errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// add selector to service overrides
	for name, glanceAPI := range instance.Spec.Glance.Template.GlanceAPIs {
		for _, endpointType := range []service.Endpoint{service.EndpointPublic, service.EndpointInternal} {
			if glanceAPI.Override.Service == nil {
				glanceAPI.Override.Service = map[service.Endpoint]service.RoutedOverrideSpec{}
			}
			glanceAPI.Override.Service[endpointType] = AddServiceComponentLabel(
				glanceAPI.Override.Service[endpointType], glance.Name)

			svcOverride := glanceAPI.Override.Service[endpointType]
			svcOverride.AddLabel(getGlanceAPILabelMap(glance.Name, name, glanceAPI.Type))
			glanceAPI.Override.Service[endpointType] = svcOverride
		}

		// preserve any previously set TLS certs,set CA cert
		if instance.Spec.TLS.Enabled(service.EndpointInternal) {
			glanceAPI.TLS.API = glance.Spec.GlanceAPIs[name].TLS.API
		}
		glanceAPI.TLS.CaBundleSecretName = instance.Status.TLS.CaBundleSecretName
		instance.Spec.Glance.Template.GlanceAPIs[name] = glanceAPI
	}

	if glance.Status.Conditions.IsTrue(glancev1.GlanceAPIReadyCondition) {
		// initialize the main APIOverride struct
		if instance.Spec.Glance.APIOverride == nil {
			instance.Spec.Glance.APIOverride = map[string]corev1beta1.Override{}
		}

		var changed bool = false
		for name, glanceAPI := range instance.Spec.Glance.Template.GlanceAPIs {
			if _, ok := instance.Spec.Glance.APIOverride[name]; !ok {
				instance.Spec.Glance.APIOverride[name] = corev1beta1.Override{}
			}
			// Retrieve the services by Label and filter on glanceAPI: for
			// each instance we should get **only** the associated `SVCs`
			// and not the whole list. As per the Glance design doc we know
			// that a given instance name is made in the form: "<service>
			// <apiName> <apiType>", so we build the filter accordingly
			// to resolve the label as <service>-<apiName>
			svcs, err := service.GetServicesListWithLabel(
				ctx,
				helper,
				instance.Namespace,
				getGlanceAPILabelMap(glance.Name, name, glanceAPI.Type),
			)
			if err != nil {
				return ctrl.Result{}, err
			}
			endpointDetails, ctrlResult, err := EnsureEndpointConfig(
				ctx,
				instance,
				helper,
				glance,
				svcs,
				glanceAPI.Override.Service,
				instance.Spec.Glance.APIOverride[name],
				corev1beta1.OpenStackControlPlaneExposeGlanceReadyCondition,
				false, // TODO (mschuppert) could be removed when all integrated service support TLS
			)
			if err != nil {
				return ctrlResult, err
			}
			glanceAPI.Override.Service = endpointDetails.GetEndpointServiceOverrides()

			// update TLS cert secret
			glanceAPI.TLS.API.Public.SecretName = endpointDetails.GetEndptCertSecret(service.EndpointPublic)
			glanceAPI.TLS.API.Internal.SecretName = endpointDetails.GetEndptCertSecret(service.EndpointInternal)
			instance.Spec.Glance.Template.GlanceAPIs[name] = glanceAPI

			// let's keep track of changes for any instance, but return
			// only when the iteration on the whole APIList is over
			if (ctrlResult != ctrl.Result{}) {
				changed = true
			}
		}
		// if one of the API changed, return
		if changed {
			return ctrl.Result{}, nil
		}
	}

	Log.Info("Reconciling Glance", "Glance.Namespace", instance.Namespace, "Glance.Name", "glance")
	op, err := controllerutil.CreateOrPatch(ctx, helper.GetClient(), glance, func() error {
		instance.Spec.Glance.Template.DeepCopyInto(&glance.Spec)
		if glance.Spec.Secret == "" {
			glance.Spec.Secret = instance.Spec.Secret
		}
		if glance.Spec.DatabaseInstance == "" {
			glance.Spec.DatabaseInstance = "openstack"
		}
		if glance.Spec.StorageClass == "" {
			glance.Spec.StorageClass = instance.Spec.StorageClass
		}
		// if already defined at service level (template section), we don't merge
		// with the global defined extra volumes
		if len(glance.Spec.ExtraMounts) == 0 {

			var glanceVolumes []glancev1.GlanceExtraVolMounts

			for _, ev := range instance.Spec.ExtraMounts {
				glanceVolumes = append(glanceVolumes, glancev1.GlanceExtraVolMounts{
					Name:      ev.Name,
					Region:    ev.Region,
					VolMounts: ev.VolMounts,
				})
			}
			glance.Spec.ExtraMounts = glanceVolumes
		}

		err := controllerutil.SetControllerReference(helper.GetBeforeObject(), glance, helper.GetScheme())
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			corev1beta1.OpenStackControlPlaneGlanceReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			corev1beta1.OpenStackControlPlaneGlanceReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		Log.Info(fmt.Sprintf("glance %s - %s", glance.Name, op))
	}

	if glance.IsReady() {
		instance.Status.Conditions.MarkTrue(corev1beta1.OpenStackControlPlaneGlanceReadyCondition, corev1beta1.OpenStackControlPlaneGlanceReadyMessage)
	} else {
		instance.Status.Conditions.Set(condition.FalseCondition(
			corev1beta1.OpenStackControlPlaneGlanceReadyCondition,
			condition.RequestedReason,
			condition.SeverityInfo,
			corev1beta1.OpenStackControlPlaneGlanceReadyRunningMessage))
	}

	return ctrl.Result{}, nil
}

func getGlanceAPILabelMap(name string, apiName string, apiType string) map[string]string {
	apiFilter := fmt.Sprintf("%s-%s-%s", name, apiName, apiType)

	return map[string]string{
		svcSelector: apiFilter,
	}
}

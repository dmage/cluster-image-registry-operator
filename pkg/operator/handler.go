package operator

import (
	"context"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/operator-framework/operator-sdk/pkg/util/k8sutil"

	kappsapi "k8s.io/api/apps/v1"
	coreapi "k8s.io/api/core/v1"
	rbacapi "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	appsapi "github.com/openshift/api/apps/v1"
	operatorapi "github.com/openshift/api/operator/v1alpha1"
	routeapi "github.com/openshift/api/route/v1"

	regopapi "github.com/openshift/cluster-image-registry-operator/pkg/apis/imageregistry/v1alpha1"
	osapi "github.com/openshift/cluster-version-operator/pkg/apis/operatorstatus.openshift.io/v1"

	"github.com/openshift/cluster-image-registry-operator/pkg/clusteroperator"
	"github.com/openshift/cluster-image-registry-operator/pkg/metautil"
	"github.com/openshift/cluster-image-registry-operator/pkg/parameters"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage"
)

func NewHandler(namespace string) (sdk.Handler, error) {
	operatorNamespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		logrus.Fatalf("Failed to get watch namespace: %v", err)
	}

	operatorName, err := k8sutil.GetOperatorName()
	if err != nil {
		logrus.Fatalf("Failed to get operator name: %v", err)
	}

	p := parameters.Globals{}

	p.Deployment.Namespace = namespace
	p.Deployment.Labels = map[string]string{"docker-registry": "default"}

	p.Pod.ServiceAccount = "registry"
	p.Container.Port = 5000

	p.Healthz.Route = "/healthz"
	p.Healthz.TimeoutSeconds = 5

	p.Service.Name = "image-registry"
	p.ImageConfig.Name = "cluster"

	h := &Handler{
		params:             p,
		generateDeployment: resource.Deployment,
		clusterStatus:      clusteroperator.NewStatusHandler(operatorName, operatorNamespace),
	}

	return h, nil
	_, err = h.Bootstrap()
	if err != nil {
		return nil, err
	}

	err = h.clusterStatus.Create()
	if err != nil {
		logrus.Errorf("unable to create cluster operator resource: %s", err)
	}

	return h, nil
}

type Handler struct {
	params             parameters.Globals
	generateDeployment resource.Generator
	clusterStatus      *clusteroperator.StatusHandler
}

func isDeploymentStatusAvailable(o runtime.Object) bool {
	switch deploy := o.(type) {
	case *appsapi.DeploymentConfig:
		return deploy.Status.AvailableReplicas > 0
	case *kappsapi.Deployment:
		return deploy.Status.AvailableReplicas > 0
	}
	return false
}

func isDeploymentStatusComplete(o runtime.Object) bool {
	switch deploy := o.(type) {
	case *appsapi.DeploymentConfig:
		return deploy.Status.UpdatedReplicas == deploy.Spec.Replicas &&
			deploy.Status.Replicas == deploy.Spec.Replicas &&
			deploy.Status.AvailableReplicas == deploy.Spec.Replicas &&
			deploy.Status.ObservedGeneration >= deploy.Generation
	case *kappsapi.Deployment:
		replicas := int32(1)
		if deploy.Spec.Replicas != nil {
			replicas = *(deploy.Spec.Replicas)
		}
		return deploy.Status.UpdatedReplicas == replicas &&
			deploy.Status.Replicas == replicas &&
			deploy.Status.AvailableReplicas == replicas &&
			deploy.Status.ObservedGeneration >= deploy.Generation
	}
	return false
}

func (h *Handler) syncDeploymentStatus(cr *regopapi.ImageRegistry, o runtime.Object, statusChanged *bool) {
	operatorAvailable := osapi.ConditionFalse
	operatorAvailableMsg := ""

	if isDeploymentStatusAvailable(o) {
		operatorAvailable = osapi.ConditionTrue
		operatorAvailableMsg = "deployment has minimum availability"
	}

	errOp := h.clusterStatus.Update(osapi.OperatorAvailable, operatorAvailable, operatorAvailableMsg)
	if errOp != nil {
		logrus.Errorf("unable to update cluster status to %s=%s: %s", osapi.OperatorAvailable, osapi.ConditionTrue, errOp)
	}

	operatorProgressing := osapi.ConditionTrue
	operatorProgressingMsg := "deployment is progressing"

	if isDeploymentStatusComplete(o) {
		operatorProgressing = osapi.ConditionFalse
		operatorProgressingMsg = "deployment successfully progressed"
	}

	errOp = h.clusterStatus.Update(osapi.OperatorProgressing, operatorProgressing, operatorProgressingMsg)
	if errOp != nil {
		logrus.Errorf("unable to update cluster status to %s=%s: %s", osapi.OperatorProgressing, operatorProgressing, errOp)
	}

	syncSuccessful := operatorapi.ConditionFalse

	if operatorProgressing == osapi.ConditionFalse {
		syncSuccessful = operatorapi.ConditionTrue
	}

	conditionSyncDeployment(cr, syncSuccessful, operatorProgressingMsg, statusChanged)
}

func updateCondition(cr *regopapi.ImageRegistry, condition *operatorapi.OperatorCondition) bool {
	modified := false
	found := false
	conditions := []operatorapi.OperatorCondition{}

	for _, c := range cr.Status.Conditions {
		if condition.Type != c.Type {
			conditions = append(conditions, c)
			continue
		}
		if condition.Status != c.Status {
			modified = true
		}
		conditions = append(conditions, *condition)
		found = true
	}

	if !found {
		conditions = append(conditions, *condition)
		modified = true
	}

	cr.Status.Conditions = conditions
	return modified
}

func conditionResourceApply(cr *regopapi.ImageRegistry, status operatorapi.ConditionStatus, m string, modified *bool) {
	if status == operatorapi.ConditionFalse {
		logrus.Errorf("condition failed on %s: %s", metautil.TypeAndName(cr), m)
	}

	changed := updateCondition(cr, &operatorapi.OperatorCondition{
		Type:               operatorapi.OperatorStatusTypeAvailable,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             "ResourceApplied",
		Message:            m,
	})

	if changed {
		*modified = true
	}
}

func conditionSyncDeployment(cr *regopapi.ImageRegistry, syncSuccessful operatorapi.ConditionStatus, m string, modified *bool) {
	reason := "DeploymentProgressed"

	if syncSuccessful == operatorapi.ConditionFalse {
		reason = "DeploymentInProgress"
	}

	changed := updateCondition(cr, &operatorapi.OperatorCondition{
		Type:               operatorapi.OperatorStatusTypeSyncSuccessful,
		Status:             syncSuccessful,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            m,
	})

	if changed {
		*modified = true
	}
}

func (h *Handler) reCreateByEvent(event sdk.Event, gen resource.Generator) (*regopapi.ImageRegistry, bool, error) {
	o := event.Object.(metav1.Object)

	cr, err := h.getImageRegistryForResource(o)
	if err != nil {
		return nil, false, err
	}

	if cr == nil || !metav1.IsControlledBy(o, cr) {
		return cr, false, nil
	}

	statusChanged := false

	tmpl, err := gen(cr, &h.params)
	if err != nil {
		conditionResourceApply(cr, operatorapi.ConditionFalse,
			fmt.Sprintf("unable to make template for %T %s/%s: %s", o, o.GetNamespace(), o.GetName(), err),
			&statusChanged,
		)
		return cr, statusChanged, nil
	}

	err = resource.ApplyTemplate(tmpl, false, &statusChanged)
	if err != nil {
		conditionResourceApply(cr, operatorapi.ConditionFalse,
			fmt.Sprintf("unable to apply template %s: %s", tmpl.Name(), err),
			&statusChanged,
		)
		return cr, statusChanged, nil
	}

	if statusChanged {
		logrus.Debugf("resource %s is recreated", tmpl.Name())
		conditionResourceApply(cr, operatorapi.ConditionTrue, "all resources applied", &statusChanged)
	}

	return cr, statusChanged, nil
}

func (h *Handler) reDeployByEvent(event sdk.Event, gen resource.Generator) (*regopapi.ImageRegistry, bool, error) {
	cr, statusChanged, err := h.reCreateByEvent(event, gen)
	if err != nil {
		return cr, statusChanged, err
	}

	if !statusChanged {
		return cr, false, nil
	}

	tmpl, err := h.generateDeployment(cr, &h.params)
	if err != nil {
		conditionResourceApply(cr, operatorapi.ConditionFalse,
			fmt.Sprintf("unable to make template for %T: %s", event.Object, err),
			&statusChanged,
		)
		return cr, statusChanged, nil
	}

	err = resource.ApplyTemplate(tmpl, true, &statusChanged)
	if err != nil {
		conditionResourceApply(cr, operatorapi.ConditionFalse,
			fmt.Sprintf("unable to apply template %s: %s", tmpl.Name(), err),
			&statusChanged,
		)
		return cr, statusChanged, nil
	}

	conditionResourceApply(cr, operatorapi.ConditionTrue, "all resources applied", &statusChanged)

	return cr, statusChanged, nil
}

func (h *Handler) Handle(ctx context.Context, event sdk.Event) error {
	return nil
	logrus.Debugf("received event for %T (deleted=%t)", event.Object, event.Deleted)

	var (
		statusChanged bool
		err           error
		cr            *regopapi.ImageRegistry
	)

	switch o := event.Object.(type) {
	case *rbacapi.ClusterRole:
		cr, statusChanged, err = h.reCreateByEvent(event, resource.ClusterRole)
		if err != nil {
			return err
		}

	case *rbacapi.ClusterRoleBinding:
		cr, statusChanged, err = h.reCreateByEvent(event, resource.ClusterRoleBinding)
		if err != nil {
			return err
		}

	case *coreapi.Service:
		cr, statusChanged, err = h.reCreateByEvent(event, resource.Service)
		if err != nil {
			return err
		}
		if cr != nil {
			svc := event.Object.(*coreapi.Service)
			svcHostname := fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc.Name, svc.Namespace, svc.Spec.Ports[0].Port)
			if cr.Status.InternalRegistryHostname != svcHostname {
				cr.Status.InternalRegistryHostname = svcHostname
				statusChanged = true
			}
		}

	case *coreapi.ServiceAccount:
		cr, statusChanged, err = h.reDeployByEvent(event, resource.ServiceAccount)
		if err != nil {
			return err
		}

	case *coreapi.ConfigMap:
		cr, statusChanged, err = h.reDeployByEvent(event, resource.ConfigMap)
		if err != nil {
			return err
		}

	case *coreapi.Secret:
		cr, statusChanged, err = h.reDeployByEvent(event, resource.Secret)
		if err != nil {
			return err
		}

	case *routeapi.Route:
		cr, err = h.getImageRegistryForResource(&o.ObjectMeta)
		if err != nil {
			return err
		}

		if cr == nil || !metav1.IsControlledBy(o, cr) {
			return nil
		}

		routes := resource.GetRouteGenerators(cr, &h.params)

		if gen, found := routes[o.ObjectMeta.Name]; found {
			tmpl, err := gen(cr, &h.params)
			if err != nil {
				conditionResourceApply(cr, operatorapi.ConditionFalse,
					fmt.Sprintf("unable to make template for %T %s/%s: %s", o, o.GetNamespace(), o.GetName(), err),
					&statusChanged,
				)
				break
			}

			err = resource.ApplyTemplate(tmpl, false, &statusChanged)
			if err != nil {
				conditionResourceApply(cr, operatorapi.ConditionFalse,
					fmt.Sprintf("unable to apply template %s: %s", tmpl.Name(), err),
					&statusChanged,
				)
				break
			}
		}

	case *kappsapi.Deployment:
		cr, err = h.getImageRegistryForResource(o)
		if err != nil {
			return err
		}

		if cr == nil || !metav1.IsControlledBy(o, cr) {
			return nil
		}

		if event.Deleted {
			tmpl, err := resource.Deployment(cr, &h.params)
			if err != nil {
				return err
			}

			err = resource.ApplyTemplate(tmpl, false, &statusChanged)
			if err != nil {
				return err
			}

			logrus.Debugf("resource %s is recreated", tmpl.Name())
			break
		}

		h.syncDeploymentStatus(cr, o, &statusChanged)

	case *appsapi.DeploymentConfig:
		cr, err = h.getImageRegistryForResource(&o.ObjectMeta)
		if err != nil {
			return err
		}

		if cr == nil || !metav1.IsControlledBy(o, cr) {
			return nil
		}

		if event.Deleted {
			tmpl, err := resource.DeploymentConfig(cr, &h.params)
			if err != nil {
				return err
			}

			err = resource.ApplyTemplate(tmpl, false, &statusChanged)
			if err != nil {
				return err
			}

			logrus.Debugf("resource %s is recreated", tmpl.Name())
			break
		}

		h.syncDeploymentStatus(cr, o, &statusChanged)

	case *regopapi.ImageRegistry:
		cr = event.Object.(*regopapi.ImageRegistry)

		if cr.ObjectMeta.DeletionTimestamp != nil {
			cr, err = h.Bootstrap()
			if err != nil {
				return err
			}
		}

		switch cr.Spec.ManagementState {
		case operatorapi.Removed:
			err = h.RemoveResources(cr)
			if err != nil {
				conditionResourceApply(o, operatorapi.ConditionFalse, fmt.Sprintf("unable to remove objects: %s", err), &statusChanged)
			}
		case operatorapi.Managed:
			err = h.CreateOrUpdateResources(cr, &statusChanged)

			if err != nil {
				errOp := h.clusterStatus.Update(osapi.OperatorFailing, osapi.ConditionTrue, "unable to deploy registry")
				if errOp != nil {
					logrus.Errorf("unable to update cluster status to %s=%s: %s", osapi.OperatorFailing, osapi.ConditionTrue, errOp)
				}
				conditionResourceApply(o, operatorapi.ConditionFalse, err.Error(), &statusChanged)
			} else {
				errOp := h.clusterStatus.Update(osapi.OperatorFailing, osapi.ConditionFalse, "")
				if errOp != nil {
					logrus.Errorf("unable to update cluster status to %s=%s: %s", osapi.OperatorFailing, osapi.ConditionFalse, errOp)
				}
				conditionResourceApply(o, operatorapi.ConditionTrue, "all resources applied", &statusChanged)
			}
		case operatorapi.Unmanaged:
			// ignore
		}
	}

	if cr != nil && statusChanged {
		logrus.Infof("%s changed", metautil.TypeAndName(cr))

		cr.Status.ObservedGeneration = cr.Generation

		err = sdk.Update(cr)
		if err != nil && !errors.IsConflict(err) {
			logrus.Errorf("unable to update %s: %s", metautil.TypeAndName(cr), err)
		}
	}

	return nil
}

func (h *Handler) getImageRegistryForResource(o metav1.Object) (*regopapi.ImageRegistry, error) {
	ownerRef := metav1.GetControllerOf(o)

	if ownerRef == nil || ownerRef.Kind != "ImageRegistry" || ownerRef.APIVersion != regopapi.SchemeGroupVersion.String() {
		return nil, nil
	}

	namespace := o.GetNamespace()
	if len(namespace) == 0 {
		// FIXME
		namespace = os.Getenv("WATCH_NAMESPACE")
	}

	cr := &regopapi.ImageRegistry{
		TypeMeta: metav1.TypeMeta{
			APIVersion: ownerRef.APIVersion,
			Kind:       ownerRef.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ownerRef.Name,
			Namespace: namespace,
		},
	}

	err := sdk.Get(cr)
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get %q custom resource: %s", ownerRef.Name, err)
		}
		return nil, nil
	}

	if cr.Spec.ManagementState != operatorapi.Managed {
		return nil, nil
	}

	return cr, nil
}

func (h *Handler) GenerateTemplates(o *regopapi.ImageRegistry, p *parameters.Globals) (ret []resource.Template, err error) {
	generators := []resource.Generator{
		resource.ClusterRole,
		resource.ClusterRoleBinding,
		resource.ServiceAccount,
		resource.ConfigMap,
		resource.Secret,
		resource.Service,
		resource.ImageConfig,
	}

	routes := resource.GetRouteGenerators(o, p)
	for i := range routes {
		generators = append(generators, routes[i])
	}

	generators = append(generators, h.generateDeployment)

	ret = make([]resource.Template, len(generators))

	for i, gen := range generators {
		ret[i], err = gen(o, p)
		if err != nil {
			return
		}
	}

	return
}

func (h *Handler) CreateOrUpdateResources(o *regopapi.ImageRegistry, modified *bool) error {
	appendFinalizer(o, modified)

	err := verifyResource(o, &h.params)
	if err != nil {
		return fmt.Errorf("unable to complete resource: %s", err)
	}

	configState, err := resource.GetConfigState(h.params.Deployment.Namespace)
	if err != nil {
		return fmt.Errorf("unable to get previous config state: %s", err)
	}

	driver, err := storage.NewDriver(o.Name, h.params.Deployment.Namespace, &o.Spec.Storage)
	if err != nil {
		return fmt.Errorf("unable to create storage driver: %s", err)
	}

	err = driver.ValidateConfiguration(configState)
	if err != nil {
		return fmt.Errorf("bad custom resource: %s", err)
	}

	templates, err := h.GenerateTemplates(o, &h.params)
	if err != nil {
		return fmt.Errorf("unable to generate templates: %s", err)
	}

	for _, tpl := range templates {
		err = resource.ApplyTemplate(tpl, false, modified)
		if err != nil {
			return fmt.Errorf("unable to apply objects: %s", err)
		}
	}

	err = syncRoutes(o, &h.params, modified)
	if err != nil {
		return fmt.Errorf("unable to sync routes: %s", err)
	}

	err = resource.SetConfigState(o, configState)
	if err != nil {
		return fmt.Errorf("unable to write current config state: %s", err)
	}

	return nil
}

package resource

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metaapi "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	appsset "k8s.io/client-go/kubernetes/typed/apps/v1"
	coreset "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacset "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"

	configset "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	routeset "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	imageregistryv1 "github.com/openshift/cluster-image-registry-operator/pkg/apis/imageregistry/v1"
	"github.com/openshift/cluster-image-registry-operator/pkg/client"
	"github.com/openshift/cluster-image-registry-operator/pkg/parameters"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource/object"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage/util"
)

func NewGenerator(kubeconfig *rest.Config, listers *client.Listers, params *parameters.Globals) *Generator {
	return &Generator{
		kubeconfig: kubeconfig,
		listers:    listers,
		params:     params,
	}
}

type Generator struct {
	kubeconfig *rest.Config
	listers    *client.Listers
	params     *parameters.Globals
}

func (g *Generator) listRoutes(routeClient routeset.RouteV1Interface, cr *imageregistryv1.Config) []Mutator {
	var mutators []Mutator
	if cr.Spec.DefaultRoute {
		mutators = append(mutators, newGeneratorRoute(g.listers.Routes, g.listers.Secrets, routeClient, g.params, cr, imageregistryv1.ImageRegistryConfigRoute{
			Name: imageregistryv1.DefaultRouteName,
		}))
	}
	for _, route := range cr.Spec.Routes {
		mutators = append(mutators, newGeneratorRoute(g.listers.Routes, g.listers.Secrets, routeClient, g.params, cr, route))
	}
	return mutators
}

func (g *Generator) list(cr *imageregistryv1.Config) ([]Mutator, error) {
	coreClient, err := coreset.NewForConfig(g.kubeconfig)
	if err != nil {
		return nil, err
	}

	appsClient, err := appsset.NewForConfig(g.kubeconfig)
	if err != nil {
		return nil, err
	}

	rbacClient, err := rbacset.NewForConfig(g.kubeconfig)
	if err != nil {
		return nil, err
	}

	routeClient, err := routeset.NewForConfig(g.kubeconfig)
	if err != nil {
		return nil, err
	}

	configClient, err := configset.NewForConfig(g.kubeconfig)
	if err != nil {
		return nil, err
	}

	driver, err := storage.NewDriver(&cr.Spec.Storage, g.kubeconfig, g.listers)
	if err != nil {
		return nil, err
	}

	var mutators []Mutator
	mutators = append(mutators, newGeneratorClusterRole(g.listers.ClusterRoles, rbacClient, cr))
	mutators = append(mutators, newGeneratorClusterRoleBinding(g.listers.ClusterRoleBindings, rbacClient, g.params, cr))
	mutators = append(mutators, newGeneratorServiceAccount(g.listers.ServiceAccounts, coreClient, g.params, cr))
	mutators = append(mutators, newGeneratorServiceCA(g.listers.ConfigMaps, coreClient, g.params))
	mutators = append(mutators, newGeneratorCAConfig(g.listers.ConfigMaps, g.listers.ImageConfigs, g.listers.OpenShiftConfig, g.listers.Services, coreClient, g.params, cr))
	mutators = append(mutators, newGeneratorSecret(g.listers.Secrets, coreClient, driver, g.params, cr))
	mutators = append(mutators, newGeneratorImageConfig(g.listers.ImageConfigs, g.listers.Routes, g.listers.Services, configClient, g.params))
	mutators = append(mutators, newGeneratorNodeCADaemonSet(g.listers.DaemonSets, g.listers.Services, appsClient, g.params))
	mutators = append(mutators, newGeneratorService(g.listers.Services, coreClient, g.params, cr))
	mutators = append(mutators, newGeneratorDeployment(g.listers.Deployments, g.listers.ConfigMaps, g.listers.Secrets, g.listers.ProxyConfigs, coreClient, appsClient, driver, g.params, cr))
	mutators = append(mutators, g.listRoutes(routeClient, cr)...)

	// This generator must be the last because he uses other generators.
	mutators = append(mutators, newGeneratorClusterOperator(g.listers.Deployments, g.listers.ClusterOperators, configClient, cr, mutators))

	return mutators, nil
}

// syncStorage checks:
// 1.)  to make sure that an existing storage medium still exists and we can access it
// 2.)  to see if the storage medium name changed and we need to:
//      a.) check to make sure that we can access the storage or
//      b.) see if we need to try to create the new storage
func (g *Generator) syncStorage(cr *imageregistryv1.Config) error {
	var runCreate bool
	// Create a driver with the current configuration
	driver, err := storage.NewDriver(&cr.Spec.Storage, g.kubeconfig, g.listers)
	if err != nil {
		return err
	}

	if driver.StorageChanged(cr) {
		runCreate = true
	} else {
		exists, err := driver.StorageExists(cr)
		if err != nil {
			return err
		}
		if !exists {
			runCreate = true
		}
	}

	if runCreate {
		if err := driver.CreateStorage(cr); err != nil {
			return err
		}
	}
	return nil
}

func (g *Generator) removeObsoleteRoutes(cr *imageregistryv1.Config) error {
	routeClient, err := routeset.NewForConfig(g.kubeconfig)

	if err != nil {
		return err
	}

	routes, err := g.listers.Routes.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list routes: %s", err)
	}

	routesGenerators := g.listRoutes(routeClient, cr)
	knownNames := map[string]struct{}{}
	for _, gen := range routesGenerators {
		knownNames[gen.GetName()] = struct{}{}
	}

	gracePeriod := int64(0)
	propagationPolicy := metaapi.DeletePropagationForeground
	opts := &metaapi.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &propagationPolicy,
	}
	for _, route := range routes {
		if !RouteIsCreatedByOperator(route) {
			continue
		}
		if _, found := knownNames[route.Name]; found {
			continue
		}
		err = routeClient.Routes(g.params.Deployment.Namespace).Delete(route.Name, opts)
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *Generator) Apply(cr *imageregistryv1.Config) error {
	err := g.syncStorage(cr)
	if err == storage.ErrStorageNotConfigured {
		return err
	} else if _, ok := err.(*util.ConfigurationError); ok {
		return err
	} else if err != nil {
		return fmt.Errorf("unable to sync storage configuration: %s", err)
	}

	generators, err := g.list(cr)
	if err != nil {
		return fmt.Errorf("unable to get generators: %s", err)
	}

	for _, gen := range generators {
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			o, err := gen.Get()
			if err != nil {
				if !errors.IsNotFound(err) {
					return fmt.Errorf("failed to get object %s: %s", Name(gen), err)
				}

				n, err := gen.Create()
				if err != nil {
					return fmt.Errorf("failed to create object %s: %s", Name(gen), err)
				}

				str, err := object.DumpString(n)
				if err != nil {
					klog.Errorf("unable to dump object: %s", err)
				}

				klog.Infof("object %s created: %s", Name(gen), str)
				return nil
			}

			n, updated, err := gen.Update(o.DeepCopyObject())
			if err != nil {
				if errors.IsConflict(err) {
					return err
				}
				return fmt.Errorf("failed to update object %s: %s", Name(gen), err)
			}
			if updated {
				difference, err := object.DiffString(o, n)
				if err != nil {
					klog.Errorf("unable to calculate difference: %s", err)
				}
				klog.Infof("object %s updated: %s", Name(gen), difference)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("unable to apply objects: %s", err)
		}
	}

	err = g.removeObsoleteRoutes(cr)
	if err != nil {
		return fmt.Errorf("unable to remove obsolete routes: %s", err)
	}

	return nil
}

func (g *Generator) Remove(cr *imageregistryv1.Config) error {
	generators, err := g.list(cr)
	if err != nil {
		return fmt.Errorf("unable to get generators: %s", err)
	}

	gracePeriod := int64(0)
	propagationPolicy := metaapi.DeletePropagationForeground
	opts := &metaapi.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &propagationPolicy,
	}
	for _, gen := range generators {
		if !gen.Owned() {
			continue
		}
		if err := gen.Delete(opts); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to delete object %s: %s", Name(gen), err)
		}
		klog.Infof("object %s deleted", Name(gen))
	}

	driver, err := storage.NewDriverFromStatus(&cr.Status.Storage, g.kubeconfig, g.listers)
	if err != nil {
		return err
	}

	var derr error
	var retriable bool
	err = wait.PollImmediate(1*time.Second, 5*time.Minute, func() (stop bool, err error) {
		if retriable, derr = driver.RemoveStorage(cr); derr != nil {
			if retriable {
				return false, nil
			} else {
				return true, derr
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("unable to remove storage: %s, %s", err, derr)
	}

	return nil
}

func (g *Generator) ApplyClusterOperator(cr *imageregistryv1.Config) error {
	configClient, err := configset.NewForConfig(g.kubeconfig)
	if err != nil {
		return err
	}

	gen := newGeneratorClusterOperator(g.listers.Deployments, g.listers.ClusterOperators, configClient, cr, nil)

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		o, err := gen.Get()
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get object %s: %s", Name(gen), err)
			}

			n, err := gen.Create()
			if err != nil {
				return fmt.Errorf("failed to create object %s: %s", Name(gen), err)
			}

			str, err := object.DumpString(n)
			if err != nil {
				klog.Errorf("unable to dump object: %s", err)
			}

			klog.Infof("object %s created: %s", Name(gen), str)
			return nil
		}

		n, updated, err := gen.Update(o.DeepCopyObject())
		if err != nil {
			if errors.IsConflict(err) {
				return err
			}
			return fmt.Errorf("failed to update object %s: %s", Name(gen), err)
		}
		if updated {
			difference, err := object.DiffString(o, n)
			if err != nil {
				klog.Errorf("unable to calculate difference: %s", err)
			}
			klog.Infof("object %s updated: %s", Name(gen), difference)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to apply objects: %s", err)
	}

	return nil
}

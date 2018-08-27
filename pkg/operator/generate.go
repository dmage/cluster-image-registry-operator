package operator

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/operator-framework/operator-sdk/pkg/sdk"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	appsapi "github.com/openshift/api/apps/v1"
	authapi "github.com/openshift/api/authorization/v1"
	projectapi "github.com/openshift/api/project/v1"

	"github.com/openshift/cluster-image-registry-operator/pkg/apis/dockerregistry/v1alpha1"
	"github.com/openshift/cluster-image-registry-operator/pkg/operator/strategy"
)

const (
	checksumOperatorAnnotation    = "dockerregistry.operator.openshift.io/checksum"
	storageTypeOperatorAnnotation = "dockerregistry.operator.openshift.io/storagetype"

	supplementalGroupsAnnotation = "openshift.io/sa.scc.supplemental-groups"
)

type Template struct {
	Object   runtime.Object
	Strategy strategy.Strategy
}

// addOwnerRefToObject appends the desired OwnerReference to the object
func addOwnerRefToObject(obj metav1.Object, ownerRef metav1.OwnerReference) {
	obj.SetOwnerReferences(append(obj.GetOwnerReferences(), ownerRef))
}

// asOwner returns an OwnerReference set as the memcached CR
func asOwner(cr *v1alpha1.OpenShiftDockerRegistry) metav1.OwnerReference {
	trueVar := true
	return metav1.OwnerReference{
		APIVersion: cr.APIVersion,
		Kind:       cr.Kind,
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: &trueVar,
	}
}

func generateLivenessProbeConfig(p *Parameters) *corev1.Probe {
	probeConfig := generateProbeConfig(p)
	probeConfig.InitialDelaySeconds = 10

	return probeConfig
}

func generateReadinessProbeConfig(p *Parameters) *corev1.Probe {
	return generateProbeConfig(p)
}

func generateProbeConfig(p *Parameters) *corev1.Probe {
	var scheme corev1.URIScheme
	if p.Container.UseTLS {
		scheme = corev1.URISchemeHTTPS
	}
	return &corev1.Probe{
		TimeoutSeconds: int32(p.Healthz.TimeoutSeconds),
		Handler: corev1.Handler{
			HTTPGet: &corev1.HTTPGetAction{
				Scheme: scheme,
				Path:   p.Healthz.Route,
				Port:   intstr.FromInt(p.Container.Port),
			},
		},
	}
}

func generateSecurityContext(cr *v1alpha1.OpenShiftDockerRegistry, namespace string) (*corev1.PodSecurityContext, error) {
	ns := &projectapi.Project{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "project.openshift.io/v1",
			Kind:       "Project",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	err := sdk.Get(ns)
	if err != nil {
		return nil, err
	}

	sgrange, ok := ns.Annotations[supplementalGroupsAnnotation]
	if !ok {
		return nil, fmt.Errorf("namespace %q doesn't have annotation %s", namespace, supplementalGroupsAnnotation)
	}

	idx := strings.Index(sgrange, "/")
	if idx == -1 {
		return nil, fmt.Errorf("annotation %s in namespace %q doesn't contain '/'", supplementalGroupsAnnotation, namespace)
	}

	gid, err := strconv.ParseInt(sgrange[:idx], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unable to parse annotation %s in namespace %q: %s", supplementalGroupsAnnotation, namespace, err)
	}

	return &corev1.PodSecurityContext{
		FSGroup: &gid,
	}, nil
}

func GenerateServiceAccount(cr *v1alpha1.OpenShiftDockerRegistry, p *Parameters) Template {
	sa := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Pod.ServiceAccount,
			Namespace: p.Deployment.Namespace,
		},
	}
	addOwnerRefToObject(sa, asOwner(cr))
	return Template{
		Object:   sa,
		Strategy: strategy.Override{},
	}
}

func GenerateClusterRole(cr *v1alpha1.OpenShiftDockerRegistry) Template {
	role := &authapi.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRole",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "system:registry",
		},
		Rules: []authapi.PolicyRule{
			{
				Verbs:     []string{"list"},
				APIGroups: []string{""},
				Resources: []string{
					"limitranges",
					"resourcequotas",
				},
			},
			{
				Verbs:     []string{"get"},
				APIGroups: []string{ /* "", */ "image.openshift.io"},
				Resources: []string{
					"imagestreamimages",
					/* "imagestreams/layers", */
					"imagestreams/secrets",
				},
			},
			{
				Verbs:     []string{ /* "list", */ "get", "update"},
				APIGroups: []string{ /* "", */ "image.openshift.io"},
				Resources: []string{
					"imagestreams",
				},
			},
			{
				Verbs:     []string{ /* "get", */ "delete"},
				APIGroups: []string{ /* "", */ "image.openshift.io"},
				Resources: []string{
					"imagestreamtags",
				},
			},
			{
				Verbs:     []string{"get", "update" /*, "delete" */},
				APIGroups: []string{ /* "", */ "image.openshift.io"},
				Resources: []string{
					"images",
				},
			},
			{
				Verbs:     []string{"create"},
				APIGroups: []string{ /* "", */ "image.openshift.io"},
				Resources: []string{
					"imagestreammappings",
				},
			},
		},
	}
	addOwnerRefToObject(role, asOwner(cr))
	return Template{
		Object:   role,
		Strategy: strategy.Override{},
	}
}

func GenerateClusterRoleBinding(cr *v1alpha1.OpenShiftDockerRegistry, p *Parameters) Template {
	crb := &authapi.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("registry-%s-role", p.Container.Name),
		},
		Subjects: []corev1.ObjectReference{
			{
				Kind:      "ServiceAccount",
				Name:      p.Pod.ServiceAccount,
				Namespace: p.Deployment.Namespace,
			},
		},
		RoleRef: corev1.ObjectReference{
			Kind: "ClusterRole",
			Name: "system:registry",
		},
	}
	addOwnerRefToObject(crb, asOwner(cr))
	return Template{
		Object:   crb,
		Strategy: strategy.Override{},
	}
}

func GenerateService(cr *v1alpha1.OpenShiftDockerRegistry, p *Parameters) *corev1.Service {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Deployment.Name,
			Namespace: p.Deployment.Namespace,
			Labels:    p.Deployment.Labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: p.Deployment.Labels,
			Ports: []corev1.ServicePort{
				{
					Name:       fmt.Sprintf("%d-tcp", p.Container.Port),
					Port:       int32(p.Container.Port),
					Protocol:   "TCP",
					TargetPort: intstr.FromInt(p.Container.Port),
				},
			},
		},
	}
	addOwnerRefToObject(svc, asOwner(cr))
	return svc
}

func GenerateDeploymentConfig(cr *v1alpha1.OpenShiftDockerRegistry, p *Parameters) (*appsapi.DeploymentConfig, error) {
	storageType := ""

	var (
		storageConfigured int
		env               []corev1.EnvVar
		mounts            []corev1.VolumeMount
		volumes           []corev1.Volume
	)

	env = append(env,
		corev1.EnvVar{Name: "REGISTRY_HTTP_ADDR", Value: fmt.Sprintf(":%d", p.Container.Port)},
		corev1.EnvVar{Name: "REGISTRY_HTTP_NET", Value: "tcp"},
		corev1.EnvVar{Name: "REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR", Value: "inmemory"},
		corev1.EnvVar{Name: "REGISTRY_STORAGE_DELETE_ENABLED", Value: "true"},
		corev1.EnvVar{Name: "REGISTRY_OPENSHIFT_QUOTA_ENABLED", Value: "true"},
	)

	if cr.Spec.Storage.Filesystem != nil {
		if cr.Spec.Storage.Filesystem.VolumeSource.HostPath != nil {
			return nil, fmt.Errorf("HostPath is not supported")
		}

		env = append(env,
			corev1.EnvVar{Name: "REGISTRY_STORAGE", Value: "filesystem"},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY", Value: "/registry"},
		)

		vol := corev1.Volume{
			Name:         "registry-storage",
			VolumeSource: cr.Spec.Storage.Filesystem.VolumeSource,
		}

		volumes = append(volumes, vol)
		mounts = append(mounts, corev1.VolumeMount{Name: vol.Name, MountPath: "/registry"})

		storageConfigured += 1
	}

	if cr.Spec.Storage.Azure != nil {
		storageType = "azure"
		env = append(env,
			corev1.EnvVar{Name: "REGISTRY_STORAGE", Value: storageType},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_AZURE_ACCOUNTNAME", Value: cr.Spec.Storage.Azure.AccountName},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_AZURE_ACCOUNTKEY", Value: cr.Spec.Storage.Azure.AccountKey},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_AZURE_CONTAINER", Value: cr.Spec.Storage.Azure.Container},
		)
		storageConfigured += 1
	}

	if cr.Spec.Storage.GCS != nil {
		storageType = "gcs"
		env = append(env,
			corev1.EnvVar{Name: "REGISTRY_STORAGE", Value: storageType},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_GCS_BUCKET", Value: cr.Spec.Storage.GCS.Bucket},
		)
		storageConfigured += 1
	}

	if cr.Spec.Storage.S3 != nil {
		storageType = "s3"
		env = append(env,
			corev1.EnvVar{Name: "REGISTRY_STORAGE", Value: storageType},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_S3_ACCESSKEY", Value: cr.Spec.Storage.S3.AccessKey},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_S3_SECRETKEY", Value: cr.Spec.Storage.S3.SecretKey},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_S3_BUCKET", Value: cr.Spec.Storage.S3.Bucket},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_S3_REGION", Value: cr.Spec.Storage.S3.Region},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_S3_REGIONENDPOINT", Value: cr.Spec.Storage.S3.RegionEndpoint},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_S3_ENCRYPT", Value: fmt.Sprintf("%v", cr.Spec.Storage.S3.Encrypt)},
		)
		storageConfigured += 1
	}

	if cr.Spec.Storage.Swift != nil {
		storageType = "swift"
		env = append(env,
			corev1.EnvVar{Name: "REGISTRY_STORAGE", Value: storageType},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_SWIFT_AUTHURL", Value: cr.Spec.Storage.Swift.AuthURL},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_SWIFT_USERNAME", Value: cr.Spec.Storage.Swift.Username},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_SWIFT_PASSWORD", Value: cr.Spec.Storage.Swift.Password},
			corev1.EnvVar{Name: "REGISTRY_STORAGE_SWIFT_CONTAINER", Value: cr.Spec.Storage.Swift.Container},
		)
		storageConfigured += 1
	}

	if storageConfigured != 1 {
		return nil, fmt.Errorf("it is not possible to initialize more than one storage backend at the same time")
	}

	namespace := cr.Namespace

	securityContext, err := generateSecurityContext(cr, namespace)
	if err != nil {
		return nil, fmt.Errorf("generate security context for deployment config: %s", err)
	}

	dc := &appsapi.DeploymentConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps.openshift.io/v1",
			Kind:       "DeploymentConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Deployment.Name,
			Namespace: p.Deployment.Namespace,
			Labels:    p.Deployment.Labels,
			Annotations: map[string]string{
				storageTypeOperatorAnnotation: storageType,
			},
		},
		Spec: appsapi.DeploymentConfigSpec{
			Replicas: cr.Spec.Replicas,
			Selector: p.Deployment.Labels,
			Triggers: []appsapi.DeploymentTriggerPolicy{
				{
					Type: appsapi.DeploymentTriggerOnConfigChange,
				},
			},
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: p.Deployment.Labels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: cr.Spec.NodeSelector,
					Containers: []corev1.Container{
						{
							Name:  p.Container.Name,
							Image: cr.Spec.ImagePullSpec,
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: int32(p.Container.Port),
									Protocol:      "TCP",
								},
							},
							Env:            env,
							VolumeMounts:   mounts,
							LivenessProbe:  generateLivenessProbeConfig(p),
							ReadinessProbe: generateReadinessProbeConfig(p),
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
					Volumes:            volumes,
					ServiceAccountName: p.Pod.ServiceAccount,
					SecurityContext:    securityContext,
				},
			},
		},
	}

	addOwnerRefToObject(dc, asOwner(cr))

	return dc, nil
}

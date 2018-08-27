package operator

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/operator-framework/operator-sdk/pkg/sdk"

	appsapi "github.com/openshift/api/apps/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	kmeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

func checksum(o interface{}) (string, error) {
	data, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), nil
}

func mergeObjectMeta(existing, required *metav1.ObjectMeta) {
	existing.Name = required.Name
	existing.Namespace = required.Namespace
	existing.Labels = required.Labels
	existing.Annotations = required.Annotations
	existing.OwnerReferences = required.OwnerReferences
}

func templateName(tmpl Template) string {
	gvk := tmpl.Object.GetObjectKind().GroupVersionKind()

	var name string
	accessor, err := kmeta.Accessor(tmpl.Object)
	if err != nil {
		name = fmt.Sprintf("%#+v", tmpl.Object)
	} else {
		if namespace := accessor.GetNamespace(); namespace != "" {
			name = fmt.Sprintf("Namespace=%s, ", namespace)
		}
		name += fmt.Sprintf("Name=%s", accessor.GetName())
	}

	return fmt.Sprintf("%s, %s", gvk, name)
}

func ApplyTemplate(tmpl Template, modified *bool) error {
	dgst, err := checksum(tmpl.Object)
	if err != nil {
		return fmt.Errorf("unable to generate checksum for %s: %s", templateName(tmpl), err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// TODO(dmage): we do not need to copy the entire template to fetch the current object
		current := tmpl.Object.DeepCopyObject()

		err := sdk.Get(current)
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get %s: %s", templateName(tmpl), err)
			}
			err = sdk.Create(tmpl.Object)
			*modified = err == nil
			return err
		}

		currentMeta, err := kmeta.Accessor(current)
		if err != nil {
			return fmt.Errorf("unable to get meta accessor for current object: %s", err)
		}

		curdgst, ok := currentMeta.GetAnnotations()[checksumOperatorAnnotation]
		if ok && dgst == curdgst {
			return nil
		}

		updated, err := tmpl.Strategy.Apply(current, tmpl.Object)
		if err != nil {
			return fmt.Errorf("unable to apply template %s: %s", templateName(tmpl), err)
		}

		updatedMeta, err := kmeta.Accessor(updated)
		if err != nil {
			return fmt.Errorf("unable to get meta accessor for updated object: %s", err)
		}

		if updatedMeta.GetAnnotations() == nil {
			updatedMeta.SetAnnotations(map[string]string{})
		}
		updatedMeta.GetAnnotations()[checksumOperatorAnnotation] = dgst

		err = sdk.Update(updated)
		*modified = err == nil
		return err
	})
}

func ApplyService(expect *corev1.Service, modified *bool) error {
	dgst, err := checksum(expect)
	if err != nil {
		return fmt.Errorf("unable to generate CR checksum: %s", err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1.Service{
			TypeMeta:   expect.TypeMeta,
			ObjectMeta: expect.ObjectMeta,
		}

		err := sdk.Get(current)
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get service %s: %s", expect.GetName(), err)
			}
			err = sdk.Create(expect)
			*modified = err == nil
			return err
		}

		curdgst, ok := current.ObjectMeta.Annotations[checksumOperatorAnnotation]
		if ok && dgst == curdgst {
			return nil
		}

		if expect.ObjectMeta.Annotations == nil {
			expect.ObjectMeta.Annotations = map[string]string{}
		}
		expect.ObjectMeta.Annotations[checksumOperatorAnnotation] = dgst

		mergeObjectMeta(&current.ObjectMeta, &expect.ObjectMeta)
		current.Spec.Selector = expect.Spec.Selector
		current.Spec.Type = expect.Spec.Type
		current.Spec.Ports = expect.Spec.Ports

		err = sdk.Update(current)
		*modified = err == nil
		return err
	})
}

func ApplyDeploymentConfig(expect *appsapi.DeploymentConfig, modified *bool) error {
	dgst, err := checksum(expect)
	if err != nil {
		return fmt.Errorf("unable to generate CR checksum: %s", err)
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &appsapi.DeploymentConfig{
			TypeMeta:   expect.TypeMeta,
			ObjectMeta: expect.ObjectMeta,
		}

		err := sdk.Get(current)
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get deployment config %s: %s", expect.GetName(), err)
			}
			err = sdk.Create(expect)
			*modified = err == nil
			return err
		}

		curdgst, ok := current.ObjectMeta.Annotations[checksumOperatorAnnotation]
		if ok && dgst == curdgst {
			return nil
		}

		if expect.ObjectMeta.Annotations == nil {
			expect.ObjectMeta.Annotations = map[string]string{}
		}
		expect.ObjectMeta.Annotations[checksumOperatorAnnotation] = dgst

		mergeObjectMeta(&current.ObjectMeta, &expect.ObjectMeta)
		current.Spec = expect.Spec

		err = sdk.Update(current)
		*modified = err == nil
		return err
	})
}

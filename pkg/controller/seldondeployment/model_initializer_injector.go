/*
Copyright 2019 kubeflow.org.
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

package seldondeployment

import (
	"context"
	"fmt"
	"strings"

	"github.com/seldonio/seldon-operator/pkg/controller/resources/credentials"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//TODO: change to seldon
const (
	DefaultModelLocalMountPath       = "/mnt/models"
	ModelInitializerContainerName    = "model-initializer"
	ModelInitializerVolumeName       = "kfserving-provision-location"
	ModelInitializerContainerImage   = "gcr.io/kfserving/model-initializer"
	ModelInitializerContainerVersion = "latest"
	PvcURIPrefix                     = "pvc://"
	PvcSourceMountName               = "kfserving-pvc-source"
	PvcSourceMountPath               = "/mnt/pvc"
	UserContainerName                = "user-container"
)

var (
	ControllerNamespace     = getEnv("POD_NAMESPACE", "seldon-system")
	ControllerConfigMapName = "seldon-config"
)

func credentialsBuilder(Client client.Client) (credentialsBuilder *credentials.CredentialBuilder, err error) {

	configMap := &corev1.ConfigMap{}
	err = Client.Get(context.TODO(), types.NamespacedName{Name: ControllerConfigMapName, Namespace: ControllerNamespace}, configMap)
	if err != nil {
		log.Error(err, "Failed to find config map", "name", ControllerConfigMapName)
		return nil, err
	}

	credentialBuilder := credentials.NewCredentialBulder(Client, configMap)
	return credentialBuilder, nil
}

// InjectModelInitializer injects an init container to provision model data
func InjectModelInitializer(deployment *appsv1.Deployment, userContainer *corev1.Container, srcURI string, serviceAccountName string, Client client.Client) error {

	// TODO: KFServing does a check for an annotation before injecting - not doing that for now
	podSpec := &deployment.Spec.Template.Spec

	// Dont inject if InitContianer already injected
	for _, container := range podSpec.InitContainers {
		if strings.Compare(container.Name, ModelInitializerContainerName) == 0 {
			return nil
		}
	}

	podVolumes := []corev1.Volume{}
	modelInitializerMounts := []corev1.VolumeMount{}

	// For PVC source URIs we need to mount the source to be able to access it
	// See design and discussion here: https://github.com/kubeflow/kfserving/issues/148
	if strings.HasPrefix(srcURI, PvcURIPrefix) {
		pvcName, pvcPath, err := parsePvcURI(srcURI)
		if err != nil {
			return err
		}

		// add the PVC volume on the pod
		pvcSourceVolume := corev1.Volume{
			Name: PvcSourceMountName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		}
		podVolumes = append(podVolumes, pvcSourceVolume)

		// add a corresponding PVC volume mount to the INIT container
		pvcSourceVolumeMount := corev1.VolumeMount{
			Name:      PvcSourceMountName,
			MountPath: PvcSourceMountPath,
			ReadOnly:  true,
		}
		modelInitializerMounts = append(modelInitializerMounts, pvcSourceVolumeMount)

		// modify the sourceURI to point to the PVC path
		srcURI = PvcSourceMountPath + "/" + pvcPath
	}

	// Create a volume that is shared between the model-initializer and user-container
	sharedVolume := corev1.Volume{
		Name: ModelInitializerVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	podVolumes = append(podVolumes, sharedVolume)

	// Create a write mount into the shared volume
	sharedVolumeWriteMount := corev1.VolumeMount{
		Name:      ModelInitializerVolumeName,
		MountPath: DefaultModelLocalMountPath,
		ReadOnly:  false,
	}
	modelInitializerMounts = append(modelInitializerMounts, sharedVolumeWriteMount)

	// Add an init container to run provisioning logic to the PodSpec
	initContainer := &corev1.Container{
		Name:  ModelInitializerContainerName,
		Image: ModelInitializerContainerImage + ":" + ModelInitializerContainerVersion,
		Args: []string{
			srcURI,
			DefaultModelLocalMountPath,
		},
		VolumeMounts: modelInitializerMounts,
	}

	// Add a mount the shared volume on the user-container, update the PodSpec
	sharedVolumeReadMount := corev1.VolumeMount{
		Name:      ModelInitializerVolumeName,
		MountPath: DefaultModelLocalMountPath,
		ReadOnly:  true,
	}
	userContainer.VolumeMounts = append(userContainer.VolumeMounts, sharedVolumeReadMount)

	// Add volumes to the PodSpec
	podSpec.Volumes = append(podSpec.Volumes, podVolumes...)

	// Inject credentials
	credentialsBuilder, err := credentialsBuilder(Client)
	if err != nil {
		return err
	}
	if serviceAccountName == "" {
		serviceAccountName = podSpec.ServiceAccountName
	}

	if err := credentialsBuilder.CreateSecretVolumeAndEnv(
		deployment.Namespace,
		serviceAccountName,
		initContainer,
		&podSpec.Volumes,
	); err != nil {
		return err
	}

	// Add init container to the spec
	podSpec.InitContainers = append(podSpec.InitContainers, *initContainer)

	return nil
}

func parsePvcURI(srcURI string) (pvcName string, pvcPath string, err error) {
	parts := strings.Split(strings.TrimPrefix(srcURI, PvcURIPrefix), "/")
	if len(parts) > 1 {
		pvcName = parts[0]
		pvcPath = strings.Join(parts[1:], "/")
	} else if len(parts) == 1 {
		pvcName = parts[0]
		pvcPath = ""
	} else {
		return "", "", fmt.Errorf("Invalid URI must be pvc://<pvcname>/[path]: %s", srcURI)
	}

	return pvcName, pvcPath, nil
}

// Copyright 2018 Portieris Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package notary

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"admission-controller2/helpers/image"
	securityenforcementv1beta1 "admission-controller2/pkg/apis/securityenforcement/v1beta1"
	"admission-controller2/pkg/kubernetes"
	"admission-controller2/pkg/notary"
	"admission-controller2/pkg/policy"
	registryclient "admission-controller2/pkg/registry"
	"admission-controller2/pkg/webhook"
	"admission-controller2/types"
	"github.com/golang/glog"
	store "github.com/theupdateframework/notary/storage"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var codec = serializer.NewCodecFactory(runtime.NewScheme())

// Controller is the notary controller
type Controller struct {
	// kubeClientsetWrapper is a standard kubernetes clientset with a wrapper for retrieving podSpec from a given object
	kubeClientsetWrapper kubernetes.WrapperInterface
	// policyClient is a securityenforcementclientset with a wrapper for retrieving the relevent policy spec
	policyClient policy.Interface
	// Trust Client
	trust notary.Interface
	// Container Registry client
	cr registryclient.Interface
}

// NewController creates a new controller object from the various clients passed in
func NewController(kubeWrapper kubernetes.WrapperInterface, policyClient policy.Interface, trust notary.Interface, cr registryclient.Interface) *Controller {
	return &Controller{
		kubeClientsetWrapper: kubeWrapper,
		policyClient:         policyClient,
		trust:                trust,
		cr:                   cr,
	}
}

// Admit is the admissionRequest handler
func (c *Controller) Admit(admissionRequest *admissionv1beta1.AdmissionRequest) *admissionv1beta1.AdmissionResponse {
	glog.Infof("Processing Trust Admission Request for %s on %s", admissionRequest.Operation, admissionRequest.Name)

	podSpecLocation, ps, err := c.kubeClientsetWrapper.GetPodSpec(admissionRequest)
	switch err {
	case nil:
		break
	case kubernetes.ErrObjectHasParents, kubernetes.ErrObjectHasZeroReplicas:
		return &admissionv1beta1.AdmissionResponse{
			Allowed: true,
		}
	default:
		a := &webhook.AdmissionResponder{}
		a.ToAdmissionResponse(err)
		return a.Flush()
	}
	return c.mutatePodSpec(admissionRequest.Namespace, podSpecLocation, *ps)
}
func (c *Controller) mutatePodSpec(namespace, specPath string, pod corev1.PodSpec) *admissionv1beta1.AdmissionResponse {
	a := &webhook.AdmissionResponder{}
	patches := []types.JSONPatch{}

	// Iterate over each container image specified
	for _, containerType := range []string{"initContainers", "containers"} {
		var containers []corev1.Container
		switch containerType {
		case "initContainers":
			containers = pod.InitContainers
		case "containers":
			containers = pod.Containers
		default:
			a.StringToAdmissionResponse("Unhandled container type")
			return a.Flush()
		}

	containerLoop:
		for containerIndex, container := range containers {
			var policy *securityenforcementv1beta1.Policy
			img, err := image.NewReference(container.Image)
			if err != nil {
				glog.Error(err)
				a.StringToAdmissionResponse(fmt.Sprintf("Deny %q, invalid image name", container.Image))
				continue containerLoop
			}

			glog.Infof("Container Image: %s   Namespace: %s", img.String(), namespace)
			if policy, err = c.policyClient.GetPolicyToEnforce(namespace, img.String()); err != nil {
				a.StringToAdmissionResponse(err.Error())
				continue containerLoop
			} else if policy == nil || !(policy.Trust.Enabled != nil && *policy.Trust.Enabled == true) {
				a.SetAllowed()
				continue containerLoop
			}

			// Trust is enforced
			glog.Info("Trust is enforced")

			// Make sure image sure there is a ImagePullSecret defined
			// TODO: This prevents use of signed publically available images with publically available signing data
			if len(pod.ImagePullSecrets) == 0 {
				a.StringToAdmissionResponse(fmt.Sprintf("Deny %q, no ImagePullSecret defined for %s", img.String(), img.GetHostname()))
				continue containerLoop
			}

		secretLoop:
			for _, secret := range pod.ImagePullSecrets {
				notaryURL := policy.Trust.TrustServer
				if notaryURL == "" {
					notaryURL, err = img.GetContentTrustURL()
					if err != nil {
						a.StringToAdmissionResponse(fmt.Sprintf("Trust Server/Image Configuration Error: %v", err.Error()))
						continue containerLoop
					}
				}

				username, password, err := c.kubeClientsetWrapper.GetSecretToken(namespace, secret.Name, img.GetHostname())
				if err != nil {
					glog.Error(err)
					continue secretLoop
				}

				notaryToken, err := c.cr.GetContentTrustToken(username, password, img.NameWithoutTag(), img.GetRegistryURL())
				if err != nil {
					glog.Error(err)
					continue secretLoop
				}

				var signers []Signer
				if policy.Trust.SignerSecrets != nil {
					// Generate a []Singer with the values for each signerSecret
					signers = make([]Signer, len(policy.Trust.SignerSecrets))
					for i, secretName := range policy.Trust.SignerSecrets {
						signers[i], err = c.getSignerSecret(namespace, secretName.Name)
						if err != nil {
							a.StringToAdmissionResponse(fmt.Sprintf("Deny %q, could not get signerSecret from your cluster, %s", img.String(), err.Error()))
							continue containerLoop
						}
					}
				}

				// Get image digest
				glog.Info("getting signed image...")

				digest, err := c.getDigest(notaryURL, img.NameWithoutTag(), notaryToken, img.GetTag(), signers)
				if err != nil {
					if strings.Contains(err.Error(), "401") {
						continue secretLoop
					}
					a.StringToAdmissionResponse(fmt.Sprintf("Deny %q, failed to get content trust information: %s", img.String(), err.Error()))
					if _, ok := err.(store.ErrServerUnavailable); ok {
						glog.Errorf("Trust server unavailable: %v", err)
						return a.Flush()
					}
					glog.Warningf("Failed to get trust information for %q: %v", img.String(), err)
					continue containerLoop
				}
				glog.Infof("Mutation #: %s %d  Image name: %s", containerType, containerIndex+1, img.String())
				if strings.Contains(container.Image, img.String()) {
					glog.Infof("Mutated to: %s@sha256:%s", img.String(), digest.String())
					patches = append(patches, types.JSONPatch{
						Op:    "replace",
						Path:  fmt.Sprintf("%s/%s/%s/image", specPath, containerType, strconv.Itoa(containerIndex)),
						Value: fmt.Sprintf("%s@sha256:%s", img.NameWithTag(), digest.String()),
					})
				}
				a.SetAllowed()
				break secretLoop
			}
			if !a.IsAllowed() {
				a.StringToAdmissionResponse(fmt.Sprintf("Deny %q, no valid ImagePullSecret defined for %s", img.String(), img.GetHostname()))
			}
		}
	}

	if a.HasErrors() {
		return a.Flush()
	}

	// Apply patches if any
	if len(patches) > 0 {
		jsonPatch, err := json.Marshal(patches)
		if err != nil {
			a.StringToAdmissionResponse(fmt.Sprintf("Invalid Patch: %s", err.Error()))
			return a.Flush()
		}
		glog.Infof("Mutation patch: %s", string(jsonPatch))
		a.SetAllowed()
		a.SetPatch(jsonPatch)
	}

	return a.Flush()
}

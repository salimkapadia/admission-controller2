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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"

	"k8s.io/apimachinery/pkg/runtime"

	securityenforcementfake "admission-controller2/pkg/apis/securityenforcement/client/clientset/versioned/fake"
	"admission-controller2/pkg/kubernetes"
	"admission-controller2/pkg/notary/fakenotary"
	"admission-controller2/pkg/policy"
	"admission-controller2/pkg/webhook"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	notaryclient "github.com/theupdateframework/notary/client"
	store "github.com/theupdateframework/notary/storage"
	"github.com/theupdateframework/notary/tuf/data"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

var _ = Describe("Main", func() {

	BeforeEach(func() {
		resetAllFakes()
	})

	Describe("serveMutatePods", func() {

		var (
			imagePolicyPayload                                       *bytes.Buffer
			clusterImagePolicyPayload                                *bytes.Buffer
			namespace                                                string
			secretName, secretName1, secretName2, badSecretName      string
			registry1, registry2                                     string
			fakeSecret, fakeSecret1, fakeSecret2, fakeSecretWrongReg *corev1.Secret
			w                                                        *httptest.ResponseRecorder
			resp                                                     *v1beta1.AdmissionReview
		)

		fakeGetRepo := func() {
			fakeRepo := &fakenotary.FakeRepository{}

			publicKey := data.NewPublicKey("sha256", []byte("abc"))
			fakeRepo.GetAllTargetMetadataByNameReturns([]notaryclient.TargetSignedStruct{
				{
					Target: notaryclient.Target{
						Hashes: data.Hashes{"sha256": []byte("1234567890")},
					},
					Role: data.DelegationRole{
						BaseRole: data.BaseRole{
							Name: "targets/wibble",
							Keys: map[string]data.PublicKey{"bfb7dd166e144cb33641f23b71c00f1e3a20d427e7b55d6c2e6b4d5fdeb790f1": publicKey},
						},
					},
				},
				{
					Target: notaryclient.Target{
						Hashes: data.Hashes{"sha256": []byte("1234567890")},
					},
					Role: data.DelegationRole{
						BaseRole: data.BaseRole{
							Name: "targets/releases",
							Keys: map[string]data.PublicKey{"whatever": publicKey},
						},
					},
				},
			}, nil)

			trust.GetNotaryRepoReturns(fakeRepo, nil)
		}

		// fakeEnforcer .
		fakeEnforcer := func(imageRepos, clusterRepos string) {
			namespace = metav1.NamespaceDefault
			secretName = "regsecret"
			secretName1 = "regsecret1"
			secretName2 = "regsecret3"
			registry1 = "registry.ng.bluemix.net"
			registry2 = "registry.bluemix.net"

			badSecretName = "badSecretName"

			// Fake kube objects
			fakeSecret = newFakeSecret(secretName, namespace, registry1)
			fakeSecret1 = newFakeSecret(secretName1, namespace, registry1)
			fakeSecret2 = newFakeSecret(secretName2, namespace, registry2)
			fakeSecretWrongReg = newFakeSecret(badSecretName, namespace, "blah")
			kubeClientset = k8sfake.NewSimpleClientset(fakeSecret, fakeSecretWrongReg, fakeSecret1, fakeSecret2)
			kubeWrapper = kubernetes.NewKubeClientsetWrapper(kubeClientset)

			policies := []runtime.Object{}

			// Fake imagepolicy objects
			// if imageRepos is an empty string the fake CRD for the namespace won't be created
			if imageRepos != "" {
				imagePolicyPayload = bytes.NewBufferString(fmt.Sprintf(`
				{
					"apiVersion": "admissionpolicy.ibm.com/v1beta1",
					"kind": "ImagePolicy",
					"metadata": {
						"name": "namespace-policy"
					},
					"spec": {
						%s
					}
				}`, imageRepos))
				imagePolicy := newImagePolicyFromYAMLOrJSON(imagePolicyPayload, namespace)
				policies = append(policies, imagePolicy)
			}

			// Fake clusterimagepolicy objects
			// if clusterRepos is an empty string the fake CRD for the cluster won't be created
			if clusterRepos != "" {
				clusterImagePolicyPayload = bytes.NewBufferString(fmt.Sprintf(`
				{
					"apiVersion": "admissionpolicy.ibm.com/v1beta1",
					"kind": "ClusterImagePolicy",
					"metadata": {
						"name": "cluster-policy"
					},
					"spec": {
						%s
					}
				}`, clusterRepos))
				clusterImagePolicy := newClusterImagePolicyFromYAMLOrJSON(clusterImagePolicyPayload, namespace)
				policies = append(policies, clusterImagePolicy)
			}
			secClientset = securityenforcementfake.NewSimpleClientset(policies...)
			policyClient = policy.NewClient(secClientset)

			// Fake content trust token
			cr.GetContentTrustTokenReturns("token", nil)

			fakeGetRepo()
		}

		updateController := func() {
			ctrl = NewController(kubeWrapper, policyClient, trust, cr)
			wh = webhook.NewServer("notary", ctrl, []byte{}, []byte{})
		}

		Describe("when there are not policies", func() {
			parseResponse := func() {
				json.Unmarshal(w.Body.Bytes(), resp)
			}

			BeforeEach(func() {
				w = httptest.NewRecorder()
				resp = &v1beta1.AdmissionReview{}
			})

			Context("if there is not a relevant policy to apply`", func() {
				It("should deny the image ", func() {
					fakeEnforcer("", "")
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/hello", no image policies or cluster polices`))
				})
			})

		})

		Describe("when there is a CRD for the namespace", func() {

			parseResponse := func() {
				json.Unmarshal(w.Body.Bytes(), resp)
			}

			BeforeEach(func() {
				w = httptest.NewRecorder()
				resp = &v1beta1.AdmissionReview{}
			})

			Context("if the `trust` policy was not specified`", func() {
				It("should not enforce `trust` and allow the image without mutation", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Patch).To(BeEmpty())
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is disabled`", func() {
				It("should not enforce `trust` and allow the image without mutation", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": false
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Patch).To(BeEmpty())
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is disabled` and the image does not have an IBM Repo", func() {
				It("should allow the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "test.com/*",
							"policy": {
								"trust": {
									"enabled": false
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequest("test.com/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled` but there is not secret for the repo", func() {
				It("should deny the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.no-secret.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequest("registry.no-secret.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.no-secret.bluemix.net/hello", no valid ImagePullSecret defined for registry.no-secret.bluemix.net`))
				})
			})

			Context("if `trust is enabled` but image name is invalid", func() {
				It("should deny the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/?")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/?", invalid image name`))
				})
			})

			Context("if `trust is enabled` but it failed to get the content trust token", func() {
				It("should deny the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					cr.GetContentTrustTokenReturns("", fmt.Errorf("FAKE_INVALID_TOKEN_ERROR"))
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/hello", no valid ImagePullSecret defined for registry.ng.bluemix.net`))
				})
			})

			Context("if `trust is enabled` but there is not a signed image", func() {
				It("should deny the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`

					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("FAKE_NO_SIGNED_IMAGE_ERROR"))
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Patch).To(BeEmpty())
					Expect(resp.Response.Result.Message).To(ContainSubstring("FAKE_NO_SIGNED_IMAGE_ERROR"))
				})
			})

			Context("if `trust is enabled` but the first secret is not valid", func() {
				It("should mutate and allow the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`

					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{}
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("401"))
					fakeGetRepo()
					updateController()
					req := newFakeRequestMultipleValidSecrets("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeTrue())
					Expect(string(resp.Response.Patch)).To(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
				})
			})

			Context("if `trust is enabled`, and there is a signed image", func() {
				It("should mutate and allow the image", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequest("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).To(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled`, and there is a signed image", func() {
				It("should break on first success", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequestMulitpleSecretsBadSecond("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).To(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled`, and there is a signed image", func() {
				It("should try all imagePullSecrets until successful ", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequestMulitpleSecrets("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).To(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled`, and the request is for a deployment", func() {
				It("should correctly mutate the podspec inside the deployment", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequestDeployment("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).To(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled`, and the request has parent objects", func() {
				It("should allow but not mutate the podspec", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequestWithParents("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).NotTo(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled`, and the request has zero replicas", func() {
				It("should allow but not mutate the podspec", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequestDeploymentWithZeroReplicas("registry.ng.bluemix.net/hello")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).NotTo(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

			Context("if `trust is enabled` and there is a server failure", func() {
				It("should fail immediately", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, store.ErrServerUnavailable{})
					updateController()
					req := newFakeRequestMultiContainer("registry.ng.bluemix.net/hello", "registry.ng.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(len(trust.GetNotaryRepoArgsForCall)).To(Equal(1))
					Expect(trust.GetNotaryRepoArgsForCall[0].Server).To(Equal("https://registry.ng.bluemix.net:4443"))
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(BeIdenticalTo("\n" + `Deny "registry.ng.bluemix.net/hello", failed to get content trust information: unable to reach trust server at this time: 0.`))
				})
			})

			Context("if `trust is enabled`, with custom trust server and there is a server failure", func() {
				It("should fail immediately", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true,
										"trustServer": "https://some-trust-server.com:4443"
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, store.ErrServerUnavailable{})
					updateController()
					req := newFakeRequestMultiContainer("registry.ng.bluemix.net/hello", "registry.ng.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(len(trust.GetNotaryRepoArgsForCall)).To(Equal(1))
					Expect(trust.GetNotaryRepoArgsForCall[0].Server).To(Equal("https://some-trust-server.com:4443"))
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(BeIdenticalTo("\n" + `Deny "registry.ng.bluemix.net/hello", failed to get content trust information: unable to reach trust server at this time: 0.`))
				})
			})

			Context("if `trust is enabled` and there mulitple containers in the pod", func() {
				It("should return all failures", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("FAKE_NO_SIGNED_IMAGE_ERROR"))
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("FAKE_NO_SIGNED_IMAGE_ERROR"))
					updateController()
					req := newFakeRequestMultiContainer("registry.ng.bluemix.net/hello", "registry.ng.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(len(trust.GetNotaryRepoArgsForCall)).To(Equal(2))
					Expect(trust.GetNotaryRepoArgsForCall[0].Server).To(Equal("https://registry.ng.bluemix.net:4443"))
					Expect(trust.GetNotaryRepoArgsForCall[1].Server).To(Equal("https://registry.ng.bluemix.net:4443"))
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/hello", failed to get content trust information: FAKE_NO_SIGNED_IMAGE_ERROR`))
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/goodbye", failed to get content trust information: FAKE_NO_SIGNED_IMAGE_ERROR`))
				})
			})

			Context("if `trust is enabled`, with custom trust server and there mulitple containers in the pod", func() {
				It("should return all failures", func() {
					imageRepos := `"repositories": [
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true,
										"trustServer": "https://some-trust-server.com:4443"
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("FAKE_NO_SIGNED_IMAGE_ERROR"))
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("FAKE_NO_SIGNED_IMAGE_ERROR"))
					updateController()
					req := newFakeRequestMultiContainer("registry.ng.bluemix.net/hello", "registry.ng.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(len(trust.GetNotaryRepoArgsForCall)).To(Equal(2))
					Expect(trust.GetNotaryRepoArgsForCall[0].Server).To(Equal("https://some-trust-server.com:4443"))
					Expect(trust.GetNotaryRepoArgsForCall[1].Server).To(Equal("https://some-trust-server.com:4443"))
					Expect(resp.Response.Allowed).To(BeFalse())
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/hello", failed to get content trust information: FAKE_NO_SIGNED_IMAGE_ERROR`))
					Expect(resp.Response.Result.Message).To(ContainSubstring(`Deny "registry.ng.bluemix.net/goodbye", failed to get content trust information: FAKE_NO_SIGNED_IMAGE_ERROR`))
				})
			})

			Context("if the pod has 2 containers that have different policies we should honor those policies correctly", func() {
				It("should allow the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": false
								},
								"va": {
									"enabled": false
								}
							}
						},
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("FAKE_NO_SIGNED_IMAGE_ERROR"))
					updateController()
					req := newFakeRequestMultiContainer("registry.ng.bluemix.net/hello", "registry.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(len(trust.GetNotaryRepoArgsForCall)).To(Equal(1))
					Expect(trust.GetNotaryRepoArgsForCall[0].Server).To(Equal("https://registry.ng.bluemix.net:4443"))
					Expect(resp.Response.Allowed).To(BeFalse())
				})
			})

			Context("if the pod has 2 containers, with custom trust server that have different policies we should honor those policies correctly", func() {
				It("should allow the image", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/hello",
							"policy": {
								"trust": {
									"enabled": true
								},
								"va": {
									"enabled": false
								}
							}
						},
						{
							"name": "registry.bluemix.net/goodbye",
							"policy": {
								"trust": {
									"enabled": true,
									"trustServer": "https://some-trust-server.com:4443"
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					trust = &fakenotary.FakeNotary{} // Wipe out the stubbed good notary response that fakeEnforcer sets up
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("some error"))
					trust.GetNotaryRepoReturns(nil, fmt.Errorf("some error"))
					updateController()
					req := newFakeRequestMultiContainerMultiSecret("registry.ng.bluemix.net/hello", "registry.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(len(trust.GetNotaryRepoArgsForCall)).To(Equal(2))
					Expect(trust.GetNotaryRepoArgsForCall[0].Server).To(Equal("https://registry.ng.bluemix.net:4443"))
					Expect(trust.GetNotaryRepoArgsForCall[0].Image).To(Equal("registry.ng.bluemix.net/hello"))
					Expect(trust.GetNotaryRepoArgsForCall[1].Server).To(Equal("https://some-trust-server.com:4443"))
					Expect(trust.GetNotaryRepoArgsForCall[1].Image).To(Equal("registry.bluemix.net/goodbye"))
					Expect(resp.Response.Allowed).To(BeFalse())
				})
			})

			Context("if request container initContainers with non-compliant images", func() {
				It("should deny the admission of the request", func() {
					imageRepos := `"repositories": [
						{
							"name": "registry.ng.bluemix.net/*",
							"policy": {
								"trust": {
									"enabled": false
								},
								"va": {
									"enabled": false
								}
							}
						}
					]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					updateController()
					req := newFakeRequestInitContainer("registry.bluemix.net/hello", "registry.ng.bluemix.net/goodbye")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(resp.Response.Allowed).To(BeFalse())
				})
			})

			Context("if `trust` is enabled, initContainer pods require signing", func() {
				It("should correctly mutate the initContainer field in the podspec", func() {
					imageRepos := `"repositories": [
                            {
								"name": "registry.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": false
									},
									"va": {
										"enabled": false
									}
								}
							},
							{
								"name": "registry.ng.bluemix.net/*",
								"policy": {
									"trust": {
										"enabled": true
									},
									"va": {
										"enabled": false
									}
								}
							}
						]`
					clusterRepos := `"repositories": []`
					fakeEnforcer(imageRepos, clusterRepos)
					fakeGetRepo()
					updateController()
					req := newFakeRequestInitContainer("registry.ng.bluemix.net/hello", "registry.bluemix.net/nosign")
					wh.HandleAdmissionRequest(w, req)
					parseResponse()
					Expect(string(resp.Response.Patch)).To(ContainSubstring("registry.ng.bluemix.net/hello:latest@sha256:31323334353637383930"))
					// Check if added patch contains patch to initContainers
					Expect(string(resp.Response.Patch)).To(ContainSubstring("initContainers"))
					Expect(resp.Response.Allowed).To(BeTrue())
				})
			})

		})
	})
})

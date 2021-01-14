package admitters

import (
	"encoding/json"

	"github.com/golang/mock/gomock"
	"k8s.io/client-go/testing"

	virtv1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"

	/*
	 * This file is part of the KubeVirt project
	 *
	 * Licensed under the Apache License, Version 2.0 (the "License");
	 * you may not use this file except in compliance with the License.
	 * You may obtain a copy of the License at
	 *
	 *     http://www.apache.org/licenses/LICENSE-2.0
	 *
	 * Unless required by applicable law or agreed to in writing, software
	 * distributed under the License is distributed on an "AS IS" BASIS,
	 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	 * See the License for the specific language governing permissions and
	 * limitations under the License.
	 *
	 * Copyright 2018 Red Hat, Inc.
	 *
	 */

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"k8s.io/api/admission/v1beta1"

	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
)

var _ = Describe("Validating NodeUpdate Admitter", func() {
	newNode := func() corev1.Node {
		return corev1.Node{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Node",
				APIVersion: corev1.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"foo":      "bar",
					"workload": "schedule",
				},
			},
		}
	}

	newKV := func() virtv1.KubeVirt {
		return virtv1.KubeVirt{
			Spec: virtv1.KubeVirtSpec{},
		}
	}

	newKVList := func(kv virtv1.KubeVirt) *virtv1.KubeVirtList {
		return &virtv1.KubeVirtList{
			Items: []virtv1.KubeVirt{
				kv,
			},
		}
	}

	newVirtLauncherPod := func() corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "testpod",
				Namespace: "ns",
				Labels: map[string]string{
					virtv1.AppLabel: "virt-launcher",
				},
			},
			Spec:   corev1.PodSpec{},
			Status: corev1.PodStatus{},
		}
	}

	nodeAdmitter := &NodeAdmitter{}

	var ctrl *gomock.Controller

	var virtClient *kubecli.MockKubevirtClient
	var kvInterface *kubecli.MockKubeVirtInterface
	var kubeClient *fake.Clientset

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		kvInterface = kubecli.NewMockKubeVirtInterface(ctrl)
		nodeAdmitter = &NodeAdmitter{
			Client: virtClient,
		}

		kubeClient = fake.NewSimpleClientset()
		virtClient.EXPECT().CoreV1().Return(kubeClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().KubeVirt("").Return(kvInterface).AnyTimes()
	})

	table.DescribeTable("Node update ", func(workloadPlacement *virtv1.ComponentConfig, virtHandlerPod corev1.Pod, updatedLabels map[string]string, expected bool) {
		node := newNode()

		oldNodeBytes, _ := json.Marshal(&node)
		newNode := node.DeepCopy()

		for k, v := range updatedLabels {
			newNode.ObjectMeta.Labels[k] = v
		}
		newNodeBytes, _ := json.Marshal(&newNode)

		ar := &v1beta1.AdmissionReview{
			Request: &v1beta1.AdmissionRequest{
				Resource: webhooks.NodeGroupVersionResource,
				Object: runtime.RawExtension{
					Raw: newNodeBytes,
				},
				OldObject: runtime.RawExtension{
					Raw: oldNodeBytes,
				},
				Operation: v1beta1.Update,
			},
		}

		kubeClient.Fake.PrependReactor("list", "pods", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			_, ok := action.(testing.ListAction)
			Expect(ok).To(BeTrue())

			return true, &corev1.PodList{Items: []corev1.Pod{}}, nil
		})

		kv := newKV()
		kv.Spec.Workloads = workloadPlacement
		kvInterface.EXPECT().List(gomock.Any()).Return(newKVList(kv), nil)

		resp := nodeAdmitter.Admit(ar)
		Expect(resp.Allowed).To(BeTrue())
	},

		table.Entry("should be allowed w/o virt-launcher pod",
			nil,
			nil,
			map[string]string{
				"foo": "updatedbar",
			},
			true,
		),

		table.Entry("should be allowed w/o workload placement specified",
			nil,
			newVirtLauncherPod(),
			map[string]string{
				"foo": "updatedbar",
			},
			true,
		),

		table.Entry("should be allowed when label change doesn't effect placement",
			&virtv1.ComponentConfig{
				NodePlacement: &virtv1.NodePlacement{
					NodeSelector: map[string]string{
						"workload": "schedule",
					},
				},
			},
			newVirtLauncherPod(),
			map[string]string{
				"foo": "updatedbar",
			},
			true,
		),

		table.Entry("should not be allowed because it will remove virt-handler from Node running VM",
			&virtv1.ComponentConfig{
				NodePlacement: &virtv1.NodePlacement{
					NodeSelector: map[string]string{
						"foo": "bar",
					},
				},
			},
			newVirtLauncherPod(),
			map[string]string{
				"foo": "updatedbar",
			},
			false,
		),

		table.Entry("should now allow match expression ",
			&virtv1.ComponentConfig{
				NodePlacement: &virtv1.NodePlacement{
					NodeSelector: map[string]string{
						"workload": "schedule",
					},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "foo",
												Operator: corev1.NodeSelectorOpNotIn,
												Values:   []string{"updatedbar"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			newVirtLauncherPod(),
			map[string]string{
				"foo": "updatedbar",
			},
			false,
		),
	)
})

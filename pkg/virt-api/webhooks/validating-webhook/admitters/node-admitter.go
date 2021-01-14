package admitters

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

import (
	"encoding/json"
	"fmt"
	"reflect"

	v1 "kubevirt.io/client-go/api/v1"

	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"kubevirt.io/client-go/kubecli"
	webhookutils "kubevirt.io/kubevirt/pkg/util/webhooks"
	validating_webhook "kubevirt.io/kubevirt/pkg/util/webhooks/validating-webhooks"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
)

type NodeAdmitter struct {
	Client kubecli.KubevirtClient
}

func NewNodeAdmitter(client kubecli.KubevirtClient) *NodeAdmitter {
	return &NodeAdmitter{
		Client: client,
	}
}

func (admitter *NodeAdmitter) Admit(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	if !webhookutils.ValidateRequestResource(ar.Request.Resource, webhooks.NodeGroupVersionResource.Group, webhooks.NodeGroupVersionResource.Resource) {
		err := fmt.Errorf("expect resource to be '%s'", webhooks.NodeGroupVersionResource.Resource)
		return webhookutils.ToAdmissionResponseError(err)
	}

	node := corev1.Node{}
	if resp := webhookutils.ValidateSchema(node.GroupVersionKind(), ar.Request.Object.Raw); resp != nil {
		return resp
	}

	newNode, oldNode, err := getAdmissionReviewNode(ar)
	if err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	// Event is not of type update so we don't need to worry about it
	if oldNode == nil {
		return validating_webhook.NewPassingAdmissionResponse()
	}

	// labels on the node didn't change so it will not effect the workload pods
	if reflect.DeepEqual(newNode.Labels, oldNode.Labels) {
		return validating_webhook.NewPassingAdmissionResponse()
	}

	hasVM, err := admitter.isNodeRunningVM(newNode.Name)
	if err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	// no vms are running, doesn't matter if virt-handler pod is removed
	if !hasVM {
		return validating_webhook.NewPassingAdmissionResponse()
	}

	matches, err := admitter.nodeWillStillRunVirtHandler(newNode)
	if err != nil {
		return webhookutils.ToAdmissionResponseError(err)
	}

	// if the new requirements continue to match the node we don't need to worry about the update because the virt-handler
	// pod will not be removed
	if !matches {
		return webhookutils.ToAdmissionResponseError(fmt.Errorf("you must remove all vms from this node before changing the label"))
	}

	return validating_webhook.NewPassingAdmissionResponse()
}

func (admitter *NodeAdmitter) nodeWillStillRunVirtHandler(node *corev1.Node) (bool, error) {
	kvs, err := admitter.Client.KubeVirt(corev1.NamespaceAll).List(&metav1.ListOptions{})
	if err != nil {
		return false, err
	}

	if len(kvs.Items) > 1 {
		return false, fmt.Errorf("you can not have more than one KubeVirt install")
	}

	kv := kvs.Items[0]
	if kv.Spec.Workloads == nil || kv.Spec.Workloads.NodePlacement == nil {
		return true, nil
	}

	selector, err := placementToSelector(kv.Spec.Workloads.NodePlacement)
	if err != nil {
		return false, err
	}

	return selector.Matches(labels.Set(node.GetLabels())), nil
}

func operator(op corev1.NodeSelectorOperator) selection.Operator {
	switch op {
	case corev1.NodeSelectorOpIn:
		return selection.In
	case corev1.NodeSelectorOpNotIn:
		return selection.NotIn
	case corev1.NodeSelectorOpExists:
		return selection.Exists
	case corev1.NodeSelectorOpDoesNotExist:
		return selection.DoesNotExist
	case corev1.NodeSelectorOpGt:
		return selection.GreaterThan
	case corev1.NodeSelectorOpLt:
		return selection.LessThan
	}

	return ""
}

func placementToSelector(placement *v1.NodePlacement) (labels.Selector, error) {
	selector := labels.NewSelector()

	for k, v := range placement.NodeSelector {
		r, err := labels.NewRequirement(k, selection.Equals, []string{v})
		if err != nil {
			fmt.Printf("New req error %v", err)
			return labels.NewSelector(), err
		}
		selector = selector.Add(*r)
	}

	selectorterms := []corev1.NodeSelectorTerm{}

	if placement.Affinity != nil &&
		placement.Affinity.NodeAffinity != nil &&
		placement.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		selectorterms = placement.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	}

	for _, st := range selectorterms {

		for _, ex := range st.MatchExpressions {
			r, err := labels.NewRequirement(ex.Key, operator(ex.Operator), ex.Values)
			if err != nil {
				return labels.NewSelector(), err
			}

			selector = selector.Add(*r)
		}
	}

	return selector, nil
}

func getAdmissionReviewNode(ar *v1beta1.AdmissionReview) (new *corev1.Node, old *corev1.Node, err error) {
	if !webhookutils.ValidateRequestResource(ar.Request.Resource, webhooks.NodeGroupVersionResource.Group, webhooks.NodeGroupVersionResource.Resource) {
		return nil, nil, fmt.Errorf("expect resource to be '%s'", webhooks.NodeGroupVersionResource)
	}

	raw := ar.Request.Object.Raw
	newNode := corev1.Node{}

	err = json.Unmarshal(raw, &newNode)
	if err != nil {
		return nil, nil, err
	}

	if ar.Request.Operation == v1beta1.Update {
		raw := ar.Request.OldObject.Raw
		oldNode := corev1.Node{}
		err = json.Unmarshal(raw, &oldNode)
		if err != nil {
			return nil, nil, err
		}
		return &newNode, &oldNode, nil
	}

	return &newNode, nil, nil
}

func (admitter *NodeAdmitter) isNodeRunningVM(name string) (bool, error) {
	pods, err := admitter.Client.CoreV1().Pods(corev1.NamespaceAll).List(metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + name,
		LabelSelector: "kubevirt.io=virt-launcher",
	})
	if err != nil || len(pods.Items) == 0 {
		return false, err
	}

	return true, nil
}

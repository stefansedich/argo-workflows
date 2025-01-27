package controller

import (
	"context"
	"encoding/json"
	"fmt"

	log "github.com/sirupsen/logrus"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/argoproj/argo-workflows/v3/errors"
	"github.com/argoproj/argo-workflows/v3/pkg/apis/workflow"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
)

func (woc *wfOperationCtx) patchTaskSet(ctx context.Context, patch interface{}, pathTypeType types.PatchType) error {
	patchByte, err := json.Marshal(patch)
	if err != nil {
		return errors.InternalWrapError(err)
	}
	_, err = woc.controller.wfclientset.ArgoprojV1alpha1().WorkflowTaskSets(woc.wf.Namespace).Patch(ctx, woc.wf.Name, pathTypeType, patchByte, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed patching taskset: %v", err)
	}
	return nil
}

func (woc *wfOperationCtx) getDeleteTaskAndNodePatch() map[string]interface{} {
	deletedNode := make(map[string]interface{})
	for _, node := range woc.wf.Status.Nodes {
		if node.Type == wfv1.NodeTypeHTTP && node.Fulfilled() {
			deletedNode[node.ID] = nil
		}
	}

	// Delete the completed Tasks and nodes status
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"tasks": deletedNode,
		},
		"status": map[string]interface{}{
			"nodes": deletedNode,
		},
	}
	return patch
}

func (woc *wfOperationCtx) removeCompletedTaskSetStatus(ctx context.Context) error {

	return woc.patchTaskSet(ctx, woc.getDeleteTaskAndNodePatch(), types.MergePatchType)
}

func (woc *wfOperationCtx) completeTaskSet(ctx context.Context) error {
	patch := woc.getDeleteTaskAndNodePatch()
	patch["metadata"] = metav1.ObjectMeta{
		Labels: map[string]string{
			common.LabelKeyCompleted: "true",
		},
	}
	return woc.patchTaskSet(ctx, patch, types.MergePatchType)
}

func (woc *wfOperationCtx) getWorkflowTaskSet() (*wfv1.WorkflowTaskSet, error) {
	taskSet, exist, err := woc.controller.wfTaskSetInformer.Informer().GetIndexer().GetByKey(woc.wf.Namespace + "/" + woc.wf.Name)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, nil
	}

	return taskSet.(*wfv1.WorkflowTaskSet), nil
}

func (woc *wfOperationCtx) taskSetReconciliation(ctx context.Context) error {
	workflowTaskset, err := woc.getWorkflowTaskSet()
	if err != nil {
		return err
	}

	woc.log.WithField("workflow", woc.wf.Name).WithField("namespace", woc.wf.Namespace).Infof("TaskSet Reconciliation")
	if workflowTaskset != nil && len(workflowTaskset.Status.Nodes) > 0 {
		for nodeID, taskResult := range workflowTaskset.Status.Nodes {
			node := woc.wf.Status.Nodes[nodeID]
			node.Outputs = taskResult.Outputs.DeepCopy()
			node.Phase = taskResult.Phase
			node.Message = taskResult.Message
			woc.wf.Status.Nodes[nodeID] = node
			node.FinishedAt = metav1.Now()
			woc.updated = true
		}
	}
	return woc.CreateTaskSet(ctx)
}

func (woc *wfOperationCtx) CreateTaskSet(ctx context.Context) error {
	if len(woc.taskSet) == 0 {
		return nil
	}
	key := fmt.Sprintf("%s/%s", woc.wf.Namespace, woc.wf.Name)
	log.WithField("workflow", woc.wf.Name).WithField("namespace", woc.wf.Namespace).WithField("TaskSet", key).Infof("Creating TaskSet")
	taskSet := wfv1.WorkflowTaskSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       workflow.WorkflowTaskSetKind,
			APIVersion: workflow.APIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: woc.wf.Namespace,
			Name:      woc.wf.Name,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: woc.wf.APIVersion,
					Kind:       woc.wf.Kind,
					UID:        woc.wf.UID,
					Name:       woc.wf.Name,
				},
			},
		},
		Spec: wfv1.WorkflowTaskSetSpec{
			Tasks: woc.taskSet,
		},
	}
	log.WithField("workflow", woc.wf.Name).WithField("namespace", woc.wf.Namespace).WithField("TaskSet", key).Debug("creating new taskset")

	_, err := woc.controller.wfclientset.ArgoprojV1alpha1().WorkflowTaskSets(woc.wf.Namespace).Create(ctx, &taskSet, metav1.CreateOptions{})

	if apierr.IsConflict(err) || apierr.IsAlreadyExists(err) {
		log.WithField("workflow", woc.wf.Name).WithField("namespace", woc.wf.Namespace).WithField("TaskSet", woc.taskSet).Debug("patching the exiting taskset")
		spec := map[string]interface{}{
			"spec": wfv1.WorkflowTaskSetSpec{Tasks: woc.taskSet},
		}
		// patch the new templates into taskset
		err = woc.patchTaskSet(ctx, spec, types.MergePatchType)
		if err != nil {
			log.WithError(err).WithField("workflow", woc.wf.Name).WithField("namespace", woc.wf.Namespace).Error("Failed to patch WorkflowTaskSet")
			return fmt.Errorf("failed to patch TaskSet. %v", err)
		}

	} else if err != nil {
		log.WithError(err).WithField("workflow", woc.wf.Name).WithField("namespace", woc.wf.Namespace).Error("Failed to create WorkflowTaskSet")
		return err
	}
	return nil
}

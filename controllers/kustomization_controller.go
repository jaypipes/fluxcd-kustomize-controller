/*
Copyright 2020 The Flux CD contributors.

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

package controllers

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1alpha1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1alpha1"
)

// KustomizationReconciler reconciles a Kustomization object
type KustomizationReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kustomize.fluxcd.io,resources=kustomizations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kustomize.fluxcd.io,resources=kustomizations/status,verbs=get;update;patch

func (r *KustomizationReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var kustomization kustomizev1.Kustomization
	if err := r.Get(ctx, req.NamespacedName, &kustomization); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := r.Log.WithValues(kustomization.Kind, req.NamespacedName)

	// get artifact source
	var repository sourcev1.GitRepository
	repositoryName := types.NamespacedName{
		Namespace: kustomization.GetNamespace(),
		Name:      kustomization.Spec.GitRepositoryRef.Name,
	}
	err := r.Client.Get(ctx, repositoryName, &repository)
	if err != nil {
		log.Error(err, "GitRepository not found", "gitrepository", repositoryName)
		return ctrl.Result{Requeue: true}, err
	}

	// try git sync
	syncedKustomization, err := r.sync(ctx, *kustomization.DeepCopy(), repository)
	if err != nil {
		log.Error(err, "Kustomization sync failed")
	}

	// update status
	if err := r.Status().Update(ctx, &syncedKustomization); err != nil {
		log.Error(err, "unable to update Kustomization status")
		return ctrl.Result{Requeue: true}, err
	}

	log.Info("Kustomization sync finished", "msg", kustomizev1.KustomizationReadyMessage(syncedKustomization))

	// requeue kustomization
	return ctrl.Result{RequeueAfter: kustomization.Spec.Interval.Duration}, nil
}

func (r *KustomizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kustomizev1.Kustomization{}).
		WithEventFilter(KustomizationSyncAtPredicate{}).
		Complete(r)
}

func (r *KustomizationReconciler) sync(
	ctx context.Context,
	kustomization kustomizev1.Kustomization,
	repository sourcev1.GitRepository) (kustomizev1.Kustomization, error) {
	if repository.Status.Artifact == nil || repository.Status.Artifact.URL == "" {
		err := fmt.Errorf("artifact not found in %s", repository.GetName())
		return kustomizev1.KustomizationNotReady(kustomization, kustomizev1.ArtifactFailedReason, err.Error()), err
	}

	// create tmp dir
	tmpDir, err := ioutil.TempDir("", repository.Name)
	if err != nil {
		err = fmt.Errorf("tmp dir error: %w", err)
		return kustomizev1.KustomizationNotReady(kustomization, sourcev1.StorageOperationFailedReason, err.Error()), err
	}
	defer os.RemoveAll(tmpDir)

	// download artifact and extract files
	url := repository.Status.Artifact.URL
	cmd := fmt.Sprintf("cd %s && curl -sL %s | tar -xz --strip-components=1 -C .", tmpDir, url)
	command := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	output, err := command.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("artifact acquisition failed: %w", err)
		return kustomizev1.KustomizationNotReady(
			kustomization,
			kustomizev1.ArtifactFailedReason,
			err.Error(),
		), fmt.Errorf("artifact download `%s` error: %s", url, string(output))
	}

	// kustomize build
	buildDir := kustomization.Spec.Path
	cmd = fmt.Sprintf("cd %s && kustomize build %s > %s.yaml", tmpDir, buildDir, kustomization.GetName())
	command = exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	output, err = command.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("kustomize build error: %w", err)
		fmt.Println(string(output))
		return kustomizev1.KustomizationNotReady(
			kustomization,
			kustomizev1.BuildFailedReason,
			err.Error(),
		), fmt.Errorf("kustomize build error: %s", string(output))
	}

	// apply kustomization
	cmd = fmt.Sprintf("cd %s && kubectl apply -f %s.yaml", tmpDir, kustomization.GetName())
	if kustomization.Spec.Prune != "" {
		cmd = fmt.Sprintf("cd %s && kubectl apply -f %s.yaml --prune -l %s",
			tmpDir, kustomization.GetName(), kustomization.Spec.Prune)
	}
	command = exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	output, err = command.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("kubectl apply error: %w", err)
		return kustomizev1.KustomizationNotReady(
			kustomization,
			kustomizev1.ApplyFailedReason,
			err.Error(),
		), fmt.Errorf("kubectl apply: %s", string(output))
	}

	// log apply output
	fmt.Println(string(output))

	return kustomizev1.KustomizationReady(
		kustomization,
		kustomizev1.ApplySucceedReason,
		"kustomization was successfully applied",
	), nil
}

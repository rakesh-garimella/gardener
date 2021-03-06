// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllerinstallation

import (
	"context"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/gardenlet/apis/config"
	"github.com/gardener/gardener/pkg/logger"
	seedpkg "github.com/gardener/gardener/pkg/operation/seed"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"

	resourcesv1alpha1 "github.com/gardener/gardener-resource-manager/pkg/apis/resources/v1alpha1"
	resourceshealth "github.com/gardener/gardener-resource-manager/pkg/health"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
)

func (c *Controller) controllerInstallationCareAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	c.controllerInstallationCareQueue.Add(key)
}

func (c *Controller) reconcileControllerInstallationCareKey(key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	controllerInstallation, err := c.controllerInstallationLister.Get(name)
	if apierrors.IsNotFound(err) {
		logger.Logger.Infof("[CONTROLLERINSTALLATION CARE] Stopping care operations for ControllerInstallation %s since it has been deleted", key)
		c.controllerInstallationCareQueue.Done(key)
		return nil
	}
	if err != nil {
		logger.Logger.Infof("[CONTROLLERINSTALLATION CARE] %s - unable to retrieve object from store: %v", key, err)
		return err
	}

	if err := c.careControl.Care(controllerInstallation, key); err != nil {
		return err
	}

	c.controllerInstallationCareQueue.AddAfter(key, c.config.Controllers.ControllerInstallationCare.SyncPeriod.Duration)
	return nil
}

// CareControlInterface implements the control logic for caring for ControllerInstallations. It is implemented as an interface to allow
// for extensions that provide different semantics. Currently, there is only one implementation.
type CareControlInterface interface {
	Care(controllerInstallation *gardencorev1beta1.ControllerInstallation, key string) error
}

// NewDefaultCareControl returns a new instance of the default implementation CareControlInterface that
// implements the documented semantics for caring for ControllerInstallations. You should use an instance returned from NewDefaultCareControl()
// for any scenario other than testing.
func NewDefaultCareControl(k8sGardenClient kubernetes.Interface, config *config.GardenletConfiguration) CareControlInterface {
	return &defaultCareControl{k8sGardenClient, config}
}

type defaultCareControl struct {
	k8sGardenClient kubernetes.Interface
	config          *config.GardenletConfiguration
}

func (c *defaultCareControl) Care(controllerInstallationObj *gardencorev1beta1.ControllerInstallation, key string) error {
	var (
		ctx = context.TODO()

		controllerInstallation       = controllerInstallationObj.DeepCopy()
		controllerInstallationLogger = logger.NewFieldLogger(logger.Logger, "controllerinstallation-care", controllerInstallation.Name)

		conditionControllerInstallationInstalled = gardencorev1beta1helper.GetOrInitCondition(controllerInstallation.Status.Conditions, gardencorev1beta1.ControllerInstallationInstalled)
		conditionControllerInstallationHealthy   = gardencorev1beta1helper.GetOrInitCondition(controllerInstallation.Status.Conditions, gardencorev1beta1.ControllerInstallationHealthy)
	)

	controllerInstallationLogger.Debugf("[CONTROLLERINSTALLATION CARE] %s", key)

	k8sSeedClient, err := seedpkg.GetSeedClient(ctx, c.k8sGardenClient.Client(), c.config.SeedClientConnection.ClientConnectionConfiguration, c.config.SeedSelector == nil, controllerInstallation.Spec.SeedRef.Name)
	if err != nil {
		controllerInstallationLogger.Errorf(err.Error())
		return nil // We do not want to run in the exponential backoff for the condition checks.
	}

	managedResource := &resourcesv1alpha1.ManagedResource{}
	if err := k8sSeedClient.Client().Get(ctx, kutil.Key(v1beta1constants.GardenNamespace, controllerInstallation.Name), managedResource); err != nil {
		controllerInstallationLogger.Errorf(err.Error())
		return nil // We do not want to run in the exponential backoff for the condition checks.
	}

	if err := resourceshealth.CheckManagedResourceApplied(managedResource); err != nil {
		conditionControllerInstallationInstalled = gardencorev1beta1helper.UpdatedCondition(conditionControllerInstallationInstalled, gardencorev1beta1.ConditionFalse, "InstallationPending", err.Error())
	} else {
		conditionControllerInstallationInstalled = gardencorev1beta1helper.UpdatedCondition(conditionControllerInstallationInstalled, gardencorev1beta1.ConditionTrue, "InstallationSuccessful", "The controller was successfully installed in the seed cluster.")
	}

	if err := resourceshealth.CheckManagedResourceHealthy(managedResource); err != nil {
		conditionControllerInstallationHealthy = gardencorev1beta1helper.UpdatedCondition(conditionControllerInstallationHealthy, gardencorev1beta1.ConditionFalse, "ControllerNotHealthy", err.Error())
	} else {
		conditionControllerInstallationHealthy = gardencorev1beta1helper.UpdatedCondition(conditionControllerInstallationHealthy, gardencorev1beta1.ConditionTrue, "ControllerHealthy", "The controller running in the seed cluster is healthy.")
	}

	if _, err := kutil.TryUpdateControllerInstallationStatusWithEqualFunc(c.k8sGardenClient.GardenCore(), retry.DefaultBackoff, controllerInstallation.ObjectMeta,
		func(controllerInstallation *gardencorev1beta1.ControllerInstallation) (*gardencorev1beta1.ControllerInstallation, error) {
			controllerInstallation.Status.Conditions = gardencorev1beta1helper.MergeConditions(controllerInstallation.Status.Conditions, conditionControllerInstallationHealthy, conditionControllerInstallationInstalled)
			return controllerInstallation, nil
		}, func(cur, updated *gardencorev1beta1.ControllerInstallation) bool {
			return equality.Semantic.DeepEqual(cur.Status.Conditions, updated.Status.Conditions)
		},
	); err != nil {
		controllerInstallationLogger.Errorf(err.Error())
		return nil // We do not want to run in the exponential backoff for the condition checks.
	}

	return nil // We do not want to run in the exponential backoff for the condition checks.
}

func (c *defaultCareControl) updateControllerInstallationStatus(controllerInstallation *gardencorev1beta1.ControllerInstallation, conditions ...gardencorev1beta1.Condition) (*gardencorev1beta1.ControllerInstallation, error) {
	newControllerInstallation, err := kutil.TryUpdateControllerInstallationStatus(c.k8sGardenClient.GardenCore(), retry.DefaultBackoff, controllerInstallation.ObjectMeta,
		func(controllerInstallation *gardencorev1beta1.ControllerInstallation) (*gardencorev1beta1.ControllerInstallation, error) {
			controllerInstallation.Status.Conditions = gardencorev1beta1helper.MergeConditions(controllerInstallation.Status.Conditions, conditions...)
			return controllerInstallation, nil
		})

	return newControllerInstallation, err
}

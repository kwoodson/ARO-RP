package checker

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

import (
	"context"
	"fmt"
	"strings"

	azureproviderv1beta1 "github.com/openshift/cluster-api-provider-azure/pkg/apis/azureprovider/v1beta1"
	machinev1beta1 "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	clusterapi "github.com/openshift/cluster-api/pkg/client/clientset_generated/clientset"
	"github.com/operator-framework/operator-sdk/pkg/status"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/Azure/ARO-RP/pkg/api"
	"github.com/Azure/ARO-RP/pkg/api/validate"
	aro "github.com/Azure/ARO-RP/pkg/operator/apis/aro.openshift.io/v1alpha1"
	aroclient "github.com/Azure/ARO-RP/pkg/operator/clientset/versioned/typed/aro.openshift.io/v1alpha1"
	"github.com/Azure/ARO-RP/pkg/operator/controllers"
	_ "github.com/Azure/ARO-RP/pkg/util/scheme"
)

const (
	machineSetsNamespace = "openshift-machine-api"
)

// MachineChecker reconciles the alertmanager webhook
type MachineChecker struct {
	clustercli      clusterapi.Interface
	arocli          aroclient.AroV1alpha1Interface
	log             *logrus.Entry
	developmentMode bool
	role            string
}

func NewMachineChecker(log *logrus.Entry, clustercli clusterapi.Interface, arocli aroclient.AroV1alpha1Interface, role string, developmentMode bool) *MachineChecker {
	return &MachineChecker{
		clustercli:      clustercli,
		arocli:          arocli,
		log:             log,
		role:            role,
		developmentMode: developmentMode,
	}
}

func (r *MachineChecker) workerReplicas() (int, error) {
	count := 0
	machinesets, err := r.clustercli.MachineV1beta1().MachineSets(machineSetsNamespace).List(metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	for _, machineset := range machinesets.Items {
		if machineset.Spec.Replicas != nil {
			count += int(*machineset.Spec.Replicas)
		}
	}
	return count, nil
}

func (r *MachineChecker) machineValid(ctx context.Context, machine *machinev1beta1.Machine, isMaster bool) (errs []error) {
	if machine.Spec.ProviderSpec.Value == nil {
		return []error{fmt.Errorf("machine %s: provider spec missing", machine.Name)}
	}

	o, _, err := scheme.Codecs.UniversalDeserializer().Decode(machine.Spec.ProviderSpec.Value.Raw, nil, nil)
	if err != nil {
		return []error{err}
	}

	machineProviderSpec, ok := o.(*azureproviderv1beta1.AzureMachineProviderSpec)
	if !ok {
		// This should never happen: codecs uses scheme that has only one registered type
		// and if something is wrong with the provider spec - decoding should fail
		return []error{fmt.Errorf("machine %s: failed to read provider spec: %T", machine.Name, o)}
	}

	if !validate.VMSizeIsValid(api.VMSize(machineProviderSpec.VMSize), r.developmentMode, isMaster) {
		errs = append(errs, fmt.Errorf("machine %s: invalid VM size '%s'", machine.Name, machineProviderSpec.VMSize))
	}

	if !isMaster && !validate.DiskSizeIsValid(int(machineProviderSpec.OSDisk.DiskSizeGB)) {
		errs = append(errs, fmt.Errorf("machine %s: invalid disk size '%d'", machine.Name, machineProviderSpec.OSDisk.DiskSizeGB))
	}

	// to begin with, just check that the image publisher and offer are correct
	if machineProviderSpec.Image.Publisher != "azureopenshift" || machineProviderSpec.Image.Offer != "aro4" {
		errs = append(errs, fmt.Errorf("machine %s: invalid image '%v'", machine.Name, machineProviderSpec.Image))
	}

	if machineProviderSpec.ManagedIdentity != "" {
		errs = append(errs, fmt.Errorf("machine %s: invalid managedIdentity '%s'", machine.Name, machineProviderSpec.ManagedIdentity))
	}

	return errs
}

func (r *MachineChecker) checkMachines(ctx context.Context) (errs []error) {
	actualWorkers := 0
	actualMasters := 0

	expectedMasters := 3
	expectedWorkers, err := r.workerReplicas()
	if err != nil {
		return []error{err}
	}

	machines, err := r.clustercli.MachineV1beta1().Machines(machineSetsNamespace).List(metav1.ListOptions{})
	if err != nil {
		return []error{err}
	}

	for _, machine := range machines.Items {
		isMaster, err := isMasterRole(&machine)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		errs = append(errs, r.machineValid(ctx, &machine, isMaster)...)

		if isMaster {
			actualMasters++
		} else {
			actualWorkers++
		}
	}

	if actualMasters != expectedMasters {
		errs = append(errs, fmt.Errorf("invalid number of master machines %d, expected %d", actualMasters, expectedMasters))
	}

	if actualWorkers != expectedWorkers {
		errs = append(errs, fmt.Errorf("invalid number of worker machines %d, expected %d", actualWorkers, expectedWorkers))
	}

	return errs
}

func (r *MachineChecker) Name() string {
	return "MachineChecker"
}

// Reconcile makes sure that the Machines are in a supportable state
func (r *MachineChecker) Check() error {
	ctx := context.Background()
	cond := &status.Condition{
		Type:    aro.MachineValid,
		Status:  corev1.ConditionTrue,
		Message: "all machines valid",
		Reason:  "CheckDone",
	}

	errs := r.checkMachines(ctx)
	if len(errs) > 0 {
		cond.Status = corev1.ConditionFalse
		cond.Reason = "CheckFailed"

		var sb strings.Builder
		for _, err := range errs {
			sb.WriteString(err.Error())
			sb.WriteByte('\n')
		}
		cond.Message = sb.String()
	}

	return controllers.SetCondition(r.arocli, cond, r.role)
}

func isMasterRole(m *machinev1beta1.Machine) (bool, error) {
	role, ok := m.Labels["machine.openshift.io/cluster-api-machine-role"]
	if !ok {
		return false, fmt.Errorf("machine %s: cluster-api-machine-role label not found", m.Name)
	}
	return role == "master", nil
}
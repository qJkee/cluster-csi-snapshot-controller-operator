package operator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformer "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/assets"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	dc "github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/managementstatecontroller"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	staticcontrollercommon "github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/klog/v2"
)

const (
	targetName        = "csi-snapshot-controller"
	targetNamespace   = "openshift-cluster-storage-operator"
	operatorNamespace = "openshift-cluster-storage-operator"

	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "OPERAND_IMAGE"
	webhookImageEnvName    = "WEBHOOK_IMAGE"

	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	kubeClient, err := kubeclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, targetName))
	if err != nil {
		return err
	}
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, "", operatorNamespace, targetNamespace)

	configClient, err := configclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, targetName))
	if err != nil {
		return err
	}
	configInformers := configinformer.NewSharedInformerFactoryWithOptions(configClient, resync)

	apiExtClient, err := apiextclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, targetName))
	if err != nil {
		return err
	}

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := operatorv1.SchemeGroupVersion.WithResource("csisnapshotcontrollers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, "cluster")
	if err != nil {
		return err
	}

	versionGetter := status.NewVersionGetter()

	staticResourcesController := staticresourcecontroller.NewStaticResourceController(
		"CSISnapshotStaticResourceController",
		assets.ReadFile,
		[]string{
			"volumesnapshots.yaml",
			"volumesnapshotcontents.yaml",
			"volumesnapshotclasses.yaml",
			"webhook_config.yaml",
		},
		resourceapply.NewKubeClientHolder(kubeClient).WithAPIExtensionsClient(apiExtClient),
		operatorClient,
		controllerConfig.EventRecorder,
	).WithConditionalResources(
		assets.ReadFile,
		[]string{
			"csi_controller_deployment_pdb.yaml",
			"webhook_deployment_pdb.yaml",
		},
		func() bool {
			isSNO, precheckSucceeded, err := staticcontrollercommon.NewIsSingleNodePlatformFn(configInformers.Config().V1().Infrastructures())()
			if err != nil {
				klog.Errorf("NewIsSingleNodePlatformFn failed: %v", err)
				return false
			}
			if !precheckSucceeded {
				klog.V(4).Infof("NewIsSingleNodePlatformFn precheck did not succeed, skipping")
				return false
			}
			return !isSNO
		},
		func() bool {
			isSNO, precheckSucceeded, err := staticcontrollercommon.NewIsSingleNodePlatformFn(configInformers.Config().V1().Infrastructures())()
			if err != nil {
				klog.Errorf("NewIsSingleNodePlatformFn failed: %v", err)
				return false
			}
			if !precheckSucceeded {
				klog.V(4).Infof("NewIsSingleNodePlatformFn precheck did not succeed, skipping")
				return false
			}
			return isSNO
		},
	).AddKubeInformers(kubeInformersForNamespaces)

	controllerDeploymentManifest, err := assets.ReadFile("csi_controller_deployment.yaml")
	if err != nil {
		return err
	}
	controllerDeploymentController := dc.NewDeploymentController(
		"CSISnapshotController",
		controllerDeploymentManifest,
		controllerConfig.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			configInformers.Config().V1().Infrastructures().Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(operandImageEnvName)),
		},
		csidrivercontrollerservicecontroller.WithControlPlaneTopologyHook(configInformers),
		csidrivercontrollerservicecontroller.WithReplicasHook(
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		),
	)

	webhookDeploymentManifest, err := assets.ReadFile("webhook_deployment.yaml")
	if err != nil {
		return err
	}
	webhookDeploymentController := dc.NewDeploymentController(
		// Name of this controller must match SISnapshotWebhookController from 4.11
		// so it "adopts" its conditions during upgrade
		"CSISnapshotWebhookController",
		webhookDeploymentManifest,
		controllerConfig.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			configInformers.Config().V1().Infrastructures().Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(webhookImageEnvName)),
		},
		csidrivercontrollerservicecontroller.WithControlPlaneTopologyHook(configInformers),
		csidrivercontrollerservicecontroller.WithReplicasHook(
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		),
	)

	versionController := NewVersionController(
		"VersionController",
		operatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
		"CSISnapshotControllerAvailable",
		"CSISnapshotControllerProgressing",
		os.Getenv(operatorVersionEnvName),
		os.Getenv(operandVersionEnvName),
	)

	clusterOperatorStatus := status.NewClusterOperatorStatusController(
		targetName,
		[]configv1.ObjectReference{
			{Resource: "namespaces", Name: targetNamespace},
			{Resource: "namespaces", Name: operatorNamespace},
			{Group: operatorv1.GroupName, Resource: "csisnapshotcontrollers", Name: "cluster"},
		},
		configClient.ConfigV1(),
		configInformers.Config().V1().ClusterOperators(),
		operatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
	)

	// This is the only controller that sets Upgradeable condition
	cndController := NewConditionController(
		"ConditionController",
		operatorClient,
		controllerConfig.EventRecorder,
		[]operatorv1.OperatorCondition{
			{
				// The condition name should match the same condition in previous OCP release (4.11).
				Type:   "CSISnapshotControllerUpgradeable",
				Status: operatorv1.ConditionTrue,
			},
		},
	)

	logLevelController := loglevel.NewClusterOperatorLoggingController(operatorClient, controllerConfig.EventRecorder)
	managementStateController := managementstatecontroller.NewOperatorManagementStateController(targetName, operatorClient, controllerConfig.EventRecorder)
	management.SetOperatorNotRemovable()

	klog.Info("Starting the Informers.")
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		dynamicInformers,
		configInformers,
		kubeInformersForNamespaces,
	} {
		informer.Start(ctx.Done())
	}

	klog.Info("Starting the controllers")
	for _, controller := range []interface {
		Run(ctx context.Context, workers int)
	}{
		clusterOperatorStatus,
		logLevelController,
		managementStateController,
		staticResourcesController,
		controllerDeploymentController,
		webhookDeploymentController,
		versionController,
		cndController,
	} {
		go controller.Run(ctx, 1)
	}

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func replacePlaceholdersHook(imageName string) dc.ManifestHookFunc {
	return func(spec *operatorv1.OperatorSpec, manifest []byte) ([]byte, error) {
		pairs := []string{
			"${OPERAND_IMAGE}", imageName,
		}
		logLevel := loglevel.LogLevelToVerbosity(spec.LogLevel)
		pairs = append(pairs, "${LOG_LEVEL}", fmt.Sprint(logLevel))

		replaced := strings.NewReplacer(pairs...).Replace(string(manifest))
		return []byte(replaced), nil
	}
}

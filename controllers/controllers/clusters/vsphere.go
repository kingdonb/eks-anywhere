package clusters

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/eks-anywhere/controllers/controllers/reconciler"
	anywherev1 "github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	c "github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/providers/vsphere"
	releasev1alpha1 "github.com/aws/eks-anywhere/release/api/v1alpha1"
)

const defaultRequeueTime = time.Minute

// Struct that holds common methods and properties
type VSphereReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Validator *vsphere.Validator
	Defaulter *vsphere.Defaulter
}

type VSphereClusterReconciler struct {
	VSphereReconciler
	*providerClusterReconciler
}

func NewVSphereReconciler(client client.Client, log logr.Logger, validator *vsphere.Validator, defaulter *vsphere.Defaulter) *VSphereClusterReconciler {
	return &VSphereClusterReconciler{
		VSphereReconciler: VSphereReconciler{
			Client:    client,
			Log:       log,
			Validator: validator,
			Defaulter: defaulter,
		},
		providerClusterReconciler: &providerClusterReconciler{},
	}
}

func VsphereCredentials(ctx context.Context, cli client.Client) (*apiv1.Secret, error) {
	secret := &apiv1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: "eksa-system",
		Name:      vsphere.CredentialsObjectName,
	}
	if err := cli.Get(ctx, secretKey, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func SetupEnvVars(ctx context.Context, vsphereDatacenter *anywherev1.VSphereDatacenterConfig, cli client.Client) error {
	secret, err := VsphereCredentials(ctx, cli)
	if err != nil {
		return fmt.Errorf("failed getting vsphere credentials secret: %v", err)
	}

	vsphereUsername := secret.Data["username"]
	vspherePassword := secret.Data["password"]

	if err := os.Setenv(vsphere.EksavSphereUsernameKey, string(vsphereUsername)); err != nil {
		return fmt.Errorf("failed setting env %s: %v", vsphere.EksavSphereUsernameKey, err)
	}

	if err := os.Setenv(vsphere.EksavSpherePasswordKey, string(vspherePassword)); err != nil {
		return fmt.Errorf("failed setting env %s: %v", vsphere.EksavSpherePasswordKey, err)
	}

	if err := vsphere.SetupEnvVars(vsphereDatacenter); err != nil {
		return fmt.Errorf("failed setting env vars: %v", err)
	}

	return nil
}

func (v *VSphereClusterReconciler) bundles(ctx context.Context, name, namespace string) (*releasev1alpha1.Bundles, error) {
	clusterBundle := &releasev1alpha1.Bundles{}
	bundleName := types.NamespacedName{Namespace: namespace, Name: name}

	if err := v.Client.Get(ctx, bundleName, clusterBundle); err != nil {
		return nil, err
	}

	return clusterBundle, nil
}

func (v *VSphereClusterReconciler) FetchAppliedSpec(ctx context.Context, cs *anywherev1.Cluster) (*c.Spec, error) {
	return c.BuildSpecForCluster(ctx, cs, v.bundles, nil)
}

func (v *VSphereClusterReconciler) Reconcile(ctx context.Context, cluster *anywherev1.Cluster) (reconciler.Result, error) {
	dataCenterConfig := &anywherev1.VSphereDatacenterConfig{}
	dataCenterName := types.NamespacedName{Namespace: cluster.Namespace, Name: cluster.Spec.DatacenterRef.Name}
	if err := v.Client.Get(ctx, dataCenterName, dataCenterConfig); err != nil {
		return reconciler.Result{}, err
	}
	// Set up envs for executing Govc cmd and default values for datacenter config
	if err := SetupEnvVars(ctx, dataCenterConfig, v.Client); err != nil {
		v.Log.Error(err, "Failed to set up env vars and default values for VsphereDatacenterConfig")
		return reconciler.Result{}, err
	}
	if !dataCenterConfig.Status.SpecValid {
		v.Log.Info("Skipping cluster reconciliation because data center config is invalid", "data center", dataCenterConfig.Name)
		return reconciler.Result{}, nil
	}

	machineConfigMap := map[string]*anywherev1.VSphereMachineConfig{}

	for _, ref := range cluster.MachineConfigRefs() {
		machineConfig := &anywherev1.VSphereMachineConfig{}
		machineConfigName := types.NamespacedName{Namespace: cluster.Namespace, Name: ref.Name}
		if err := v.Client.Get(ctx, machineConfigName, machineConfig); err != nil {
			return reconciler.Result{}, err
		}
		machineConfigMap[ref.Name] = machineConfig
	}

	bundles, err := v.bundles(ctx, cluster.Spec.ManagementCluster.Name, "default")
	if err != nil {
		return reconciler.Result{}, err
	}

	specWithBundles, err := c.BuildSpecFromBundles(cluster, bundles)
	if err != nil {
		return reconciler.Result{}, err
	}
	vshepreClusterSpec := vsphere.NewSpec(specWithBundles, machineConfigMap, dataCenterConfig)

	if err := v.Validator.ValidateClusterMachineConfigs(ctx, vshepreClusterSpec); err != nil {
		return reconciler.Result{}, err
	}

	workerNodeGroupMachineSpecs := make(map[string]anywherev1.VSphereMachineConfigSpec, len(cluster.Spec.WorkerNodeGroupConfigurations))
	for _, wnConfig := range cluster.Spec.WorkerNodeGroupConfigurations {
		workerNodeGroupMachineSpecs[wnConfig.MachineGroupRef.Name] = machineConfigMap[wnConfig.MachineGroupRef.Name].Spec
	}

	cp := machineConfigMap[specWithBundles.Spec.ControlPlaneConfiguration.MachineGroupRef.Name]
	var etcdSpec *anywherev1.VSphereMachineConfigSpec
	if specWithBundles.Spec.ExternalEtcdConfiguration != nil {
		etcd := machineConfigMap[specWithBundles.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name]
		etcdSpec = &etcd.Spec
	}

	templateBuilder := vsphere.NewVsphereTemplateBuilder(&dataCenterConfig.Spec, &cp.Spec, etcdSpec, workerNodeGroupMachineSpecs, time.Now, true)
	clusterName := cluster.ObjectMeta.Name

	cpOpt := func(values map[string]interface{}) {
		values["controlPlaneTemplateName"] = templateBuilder.CPMachineTemplateName(clusterName)
		controlPlaneUser := machineConfigMap[cluster.Spec.ControlPlaneConfiguration.MachineGroupRef.Name].Spec.Users[0]
		values["vsphereControlPlaneSshAuthorizedKey"] = controlPlaneUser.SshAuthorizedKeys[0]

		if cluster.Spec.ExternalEtcdConfiguration != nil {
			etcdUser := machineConfigMap[cluster.Spec.ExternalEtcdConfiguration.MachineGroupRef.Name].Spec.Users[0]
			values["vsphereEtcdSshAuthorizedKey"] = etcdUser.SshAuthorizedKeys[0]
		}

		values["etcdTemplateName"] = templateBuilder.EtcdMachineTemplateName(clusterName)
	}
	v.Log.Info("cluster", "name", cluster.Name)
	controlPlaneSpec, err := templateBuilder.GenerateCAPISpecControlPlane(specWithBundles, cpOpt)
	if err != nil {
		return reconciler.Result{}, err
	}

	workersSpec, err := templateBuilder.GenerateCAPISpecWorkers(specWithBundles, nil)
	if err != nil {
		return reconciler.Result{}, err
	}

	if err := reconciler.ReconcileYaml(ctx, v.Client, controlPlaneSpec); err != nil {
		return reconciler.Result{Result: &ctrl.Result{
			Requeue:      true,
			RequeueAfter: defaultRequeueTime,
		}}, err
	}
	if err := reconciler.ReconcileYaml(ctx, v.Client, workersSpec); err != nil {
		return reconciler.Result{}, err
	}

	return reconciler.Result{}, nil
}

/*
Copyright 2019 The KubeSphere Authors.

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

package app

import (
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/kubefed/pkg/controller/util"

	"kubesphere.io/kubesphere/pkg/controller/storage/snapshotclass"

	iamv1alpha2 "kubesphere.io/api/iam/v1alpha2"

	authoptions "kubesphere.io/kubesphere/pkg/apiserver/authentication/options"
	"kubesphere.io/kubesphere/pkg/controller/certificatesigningrequest"
	"kubesphere.io/kubesphere/pkg/controller/cluster"
	"kubesphere.io/kubesphere/pkg/controller/clusterrolebinding"
	"kubesphere.io/kubesphere/pkg/controller/destinationrule"
	"kubesphere.io/kubesphere/pkg/controller/globalrole"
	"kubesphere.io/kubesphere/pkg/controller/globalrolebinding"
	"kubesphere.io/kubesphere/pkg/controller/group"
	"kubesphere.io/kubesphere/pkg/controller/groupbinding"
	"kubesphere.io/kubesphere/pkg/controller/job"
	"kubesphere.io/kubesphere/pkg/controller/loginrecord"
	"kubesphere.io/kubesphere/pkg/controller/network/ippool"
	"kubesphere.io/kubesphere/pkg/controller/network/nsnetworkpolicy"
	"kubesphere.io/kubesphere/pkg/controller/network/nsnetworkpolicy/provider"
	"kubesphere.io/kubesphere/pkg/controller/notification"
	"kubesphere.io/kubesphere/pkg/controller/storage/capability"
	"kubesphere.io/kubesphere/pkg/controller/user"
	"kubesphere.io/kubesphere/pkg/controller/virtualservice"
	"kubesphere.io/kubesphere/pkg/informers"
	"kubesphere.io/kubesphere/pkg/simple/client/devops"
	"kubesphere.io/kubesphere/pkg/simple/client/k8s"
	ldapclient "kubesphere.io/kubesphere/pkg/simple/client/ldap"
	"kubesphere.io/kubesphere/pkg/simple/client/multicluster"
	"kubesphere.io/kubesphere/pkg/simple/client/network"
	ippoolclient "kubesphere.io/kubesphere/pkg/simple/client/network/ippool"
	"kubesphere.io/kubesphere/pkg/simple/client/s3"
)

func addControllers(
	mgr manager.Manager,
	client k8s.Client,
	informerFactory informers.InformerFactory,
	devopsClient devops.Interface,
	s3Client s3.Interface,
	ldapClient ldapclient.Interface,
	options *k8s.KubernetesOptions,
	authenticationOptions *authoptions.AuthenticationOptions,
	multiClusterOptions *multicluster.Options,
	networkOptions *network.Options,
	serviceMeshEnabled bool,
	kubectlImage string,
	stopCh <-chan struct{}) error {

	kubernetesInformer := informerFactory.KubernetesSharedInformerFactory()
	istioInformer := informerFactory.IstioSharedInformerFactory()
	kubesphereInformer := informerFactory.KubeSphereSharedInformerFactory()

	multiClusterEnabled := multiClusterOptions.Enable

	var vsController, drController manager.Runnable
	if serviceMeshEnabled {
		vsController = virtualservice.NewVirtualServiceController(kubernetesInformer.Core().V1().Services(),
			istioInformer.Networking().V1alpha3().VirtualServices(),
			istioInformer.Networking().V1alpha3().DestinationRules(),
			kubesphereInformer.Servicemesh().V1alpha2().Strategies(),
			client.Kubernetes(),
			client.Istio(),
			client.KubeSphere())

		drController = destinationrule.NewDestinationRuleController(kubernetesInformer.Apps().V1().Deployments(),
			istioInformer.Networking().V1alpha3().DestinationRules(),
			kubernetesInformer.Core().V1().Services(),
			kubesphereInformer.Servicemesh().V1alpha2().ServicePolicies(),
			client.Kubernetes(),
			client.Istio(),
			client.KubeSphere())
	}

	jobController := job.NewJobController(kubernetesInformer.Batch().V1().Jobs(), client.Kubernetes())

	storageCapabilityController := capability.NewController(
		client.Kubernetes().StorageV1().StorageClasses(),
		kubernetesInformer.Storage().V1().StorageClasses(),
		kubernetesInformer.Storage().V1().CSIDrivers(),
	)

	volumeSnapshotController := snapshotclass.NewController(
		kubernetesInformer.Storage().V1().StorageClasses(),
		client.Snapshot().SnapshotV1().VolumeSnapshotClasses(),
		informerFactory.SnapshotSharedInformerFactory().Snapshot().V1().VolumeSnapshotClasses(),
	)

	var fedUserCache, fedGlobalRoleBindingCache, fedGlobalRoleCache cache.Store
	var fedUserCacheController, fedGlobalRoleBindingCacheController, fedGlobalRoleCacheController cache.Controller

	if multiClusterEnabled {
		fedUserClient, err := util.NewResourceClient(client.Config(), &iamv1alpha2.FedUserResource)
		if err != nil {
			klog.Error(err)
			return err
		}
		fedGlobalRoleClient, err := util.NewResourceClient(client.Config(), &iamv1alpha2.FedGlobalRoleResource)
		if err != nil {
			klog.Error(err)
			return err
		}
		fedGlobalRoleBindingClient, err := util.NewResourceClient(client.Config(), &iamv1alpha2.FedGlobalRoleBindingResource)
		if err != nil {
			klog.Error(err)
			return err
		}

		fedUserCache, fedUserCacheController = util.NewResourceInformer(fedUserClient, "", &iamv1alpha2.FedUserResource, func(object runtimeclient.Object) {})
		fedGlobalRoleCache, fedGlobalRoleCacheController = util.NewResourceInformer(fedGlobalRoleClient, "", &iamv1alpha2.FedGlobalRoleResource, func(object runtimeclient.Object) {})
		fedGlobalRoleBindingCache, fedGlobalRoleBindingCacheController = util.NewResourceInformer(fedGlobalRoleBindingClient, "", &iamv1alpha2.FedGlobalRoleBindingResource, func(object runtimeclient.Object) {})

		go fedUserCacheController.Run(stopCh)
		go fedGlobalRoleCacheController.Run(stopCh)
		go fedGlobalRoleBindingCacheController.Run(stopCh)
	}

	userController := user.NewUserController(client.Kubernetes(), client.KubeSphere(), client.Config(),
		kubesphereInformer.Iam().V1alpha2().Users(),
		kubesphereInformer.Iam().V1alpha2().LoginRecords(),
		fedUserCache, fedUserCacheController,
		kubernetesInformer.Core().V1().ConfigMaps(),
		ldapClient, devopsClient,
		authenticationOptions, multiClusterEnabled)

	loginRecordController := loginrecord.NewLoginRecordController(
		client.Kubernetes(),
		client.KubeSphere(),
		kubesphereInformer.Iam().V1alpha2().LoginRecords(),
		kubesphereInformer.Iam().V1alpha2().Users(),
		authenticationOptions.LoginHistoryRetentionPeriod,
		authenticationOptions.LoginHistoryMaximumEntries)

	csrController := certificatesigningrequest.NewController(client.Kubernetes(),
		kubernetesInformer.Certificates().V1().CertificateSigningRequests(),
		kubernetesInformer.Core().V1().ConfigMaps(), client.Config())

	clusterRoleBindingController := clusterrolebinding.NewController(client.Kubernetes(),
		kubernetesInformer.Rbac().V1().ClusterRoleBindings(),
		kubernetesInformer.Apps().V1().Deployments(),
		kubernetesInformer.Core().V1().Pods(),
		kubesphereInformer.Iam().V1alpha2().Users(),
		kubectlImage)

	globalRoleController := globalrole.NewController(client.Kubernetes(), client.KubeSphere(),
		kubesphereInformer.Iam().V1alpha2().GlobalRoles(), fedGlobalRoleCache, fedGlobalRoleCacheController)

	globalRoleBindingController := globalrolebinding.NewController(client.Kubernetes(), client.KubeSphere(),
		kubesphereInformer.Iam().V1alpha2().GlobalRoleBindings(),
		fedGlobalRoleBindingCache, fedGlobalRoleBindingCacheController,
		multiClusterEnabled)

	groupBindingController := groupbinding.NewController(client.Kubernetes(), client.KubeSphere(),
		kubesphereInformer.Iam().V1alpha2().GroupBindings(),
		kubesphereInformer.Types().V1beta1().FederatedGroupBindings(),
		multiClusterEnabled)

	groupController := group.NewController(client.Kubernetes(), client.KubeSphere(),
		kubesphereInformer.Iam().V1alpha2().Groups(),
		kubesphereInformer.Types().V1beta1().FederatedGroups(),
		multiClusterEnabled)

	var clusterController manager.Runnable
	if multiClusterEnabled {
		clusterController = cluster.NewClusterController(
			client.Kubernetes(),
			client.Config(),
			kubesphereInformer.Cluster().V1alpha1().Clusters(),
			client.KubeSphere().ClusterV1alpha1().Clusters(),
			multiClusterOptions.ClusterControllerResyncPeriod,
			multiClusterOptions.HostClusterName)
	}

	var nsnpController manager.Runnable
	if networkOptions.EnableNetworkPolicy {
		nsnpProvider, err := provider.NewNsNetworkPolicyProvider(client.Kubernetes(), kubernetesInformer.Networking().V1().NetworkPolicies())
		if err != nil {
			return err
		}

		nsnpController = nsnetworkpolicy.NewNSNetworkPolicyController(client.Kubernetes(),
			client.KubeSphere().NetworkV1alpha1(),
			kubesphereInformer.Network().V1alpha1().NamespaceNetworkPolicies(),
			kubernetesInformer.Core().V1().Services(),
			kubernetesInformer.Core().V1().Nodes(),
			kubesphereInformer.Tenant().V1alpha1().Workspaces(),
			kubernetesInformer.Core().V1().Namespaces(), nsnpProvider, networkOptions.NSNPOptions)
	}

	var ippoolController manager.Runnable
	ippoolProvider := ippoolclient.NewProvider(kubernetesInformer, client.KubeSphere(), client.Kubernetes(), networkOptions.IPPoolType, options)
	if ippoolProvider != nil {
		ippoolController = ippool.NewIPPoolController(kubesphereInformer, kubernetesInformer, client.Kubernetes(), client.KubeSphere(), ippoolProvider)
	}

	controllers := map[string]manager.Runnable{
		"virtualservice-controller":     vsController,
		"destinationrule-controller":    drController,
		"job-controller":                jobController,
		"storagecapability-controller":  storageCapabilityController,
		"volumesnapshot-controller":     volumeSnapshotController,
		"user-controller":               userController,
		"loginrecord-controller":        loginRecordController,
		"cluster-controller":            clusterController,
		"nsnp-controller":               nsnpController,
		"csr-controller":                csrController,
		"clusterrolebinding-controller": clusterRoleBindingController,
		"globalrolebinding-controller":  globalRoleBindingController,
		"ippool-controller":             ippoolController,
		"groupbinding-controller":       groupBindingController,
		"group-controller":              groupController,
	}

	if multiClusterEnabled {
		controllers["globalrole-controller"] = globalRoleController
		notificationController, err := notification.NewController(client.Kubernetes(), mgr.GetClient(), mgr.GetCache())
		if err != nil {
			return err
		}
		controllers["notification-controller"] = notificationController
	}

	for name, ctrl := range controllers {
		if ctrl == nil {
			klog.V(4).Infof("%s is not going to run due to dependent component disabled.", name)
			continue
		}

		if err := mgr.Add(ctrl); err != nil {
			klog.Error(err, "add controller to manager failed", "name", name)
			return err
		}
	}

	return nil
}

package cluster

import (
	"context"

	"fmt"
	"net/http"

	"crypto/rand"
	"encoding/hex"

	devopsv1 "github.com/gostship/kunkka/pkg/apis/devops/v1"
	"github.com/gostship/kunkka/pkg/constants"
	"github.com/gostship/kunkka/pkg/controllers/common"
	"github.com/gostship/kunkka/pkg/provider/addons/coredns"
	"github.com/gostship/kunkka/pkg/provider/addons/flannel"
	"github.com/gostship/kunkka/pkg/provider/addons/kubeproxy"
	"github.com/gostship/kunkka/pkg/provider/addons/metricsserver"
	"github.com/gostship/kunkka/pkg/provider/certs"
	"github.com/gostship/kunkka/pkg/provider/phases/kubeadm"
	"github.com/gostship/kunkka/pkg/provider/phases/kubeconfig"
	"github.com/gostship/kunkka/pkg/util/k8sutil"
	"github.com/gostship/kunkka/pkg/util/pkiutil"
	"github.com/pkg/errors"
	"github.com/segmentio/ksuid"
	"k8s.io/apimachinery/pkg/runtime"
	bootstraputil "k8s.io/cluster-bootstrap/token/util"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	tokenFileTemplate = `%s,admin,admin,system:masters
`
	additPolicy = `
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
- level: Metadata
`
)

func completeK8sVersion(cluster *common.Cluster) error {
	cluster.Cluster.Status.Version = cluster.Spec.Version
	return nil
}

func completeNetworking(cluster *common.Cluster) error {
	var (
		serviceCIDR      string
		nodeCIDRMaskSize int32
		err              error
	)

	if cluster.Spec.ServiceCIDR != nil {
		serviceCIDR = *cluster.Spec.ServiceCIDR
		nodeCIDRMaskSize, err = k8sutil.GetNodeCIDRMaskSize(cluster.Spec.ClusterCIDR, *cluster.Spec.Properties.MaxNodePodNum)
		if err != nil {
			return errors.Wrap(err, "GetNodeCIDRMaskSize error")
		}
	} else {
		serviceCIDR, nodeCIDRMaskSize, err = k8sutil.GetServiceCIDRAndNodeCIDRMaskSize(cluster.Spec.ClusterCIDR, *cluster.Spec.Properties.MaxClusterServiceNum, *cluster.Spec.Properties.MaxNodePodNum)
		if err != nil {
			return errors.Wrap(err, "GetServiceCIDRAndNodeCIDRMaskSize error")
		}
	}
	cluster.Cluster.Status.ServiceCIDR = serviceCIDR
	cluster.Cluster.Status.NodeCIDRMaskSize = nodeCIDRMaskSize

	return nil
}

func completeDNS(cluster *common.Cluster) error {
	ip, err := k8sutil.GetIndexedIP(cluster.Cluster.Status.ServiceCIDR, constants.DNSIPIndex)
	if err != nil {
		return errors.Wrap(err, "get DNS IP error")
	}
	cluster.Cluster.Status.DNSIP = ip.String()

	return nil
}

func completeAddresses(cluster *common.Cluster) error {
	for _, m := range cluster.Spec.Machines {
		cluster.AddAddress(devopsv1.AddressReal, m.IP, 6443)
	}

	if cluster.Spec.Features.HA != nil {
		if cluster.Spec.Features.HA.DKEHA != nil {
			cluster.AddAddress(devopsv1.AddressAdvertise, cluster.Spec.Features.HA.DKEHA.VIP, 6443)
		}
		if cluster.Spec.Features.HA.ThirdPartyHA != nil {
			cluster.AddAddress(devopsv1.AddressAdvertise, cluster.Spec.Features.HA.ThirdPartyHA.VIP, cluster.Spec.Features.HA.ThirdPartyHA.VPort)
		}
	}

	return nil
}

func completeCredential(cluster *common.Cluster) error {
	token := ksuid.New().String()
	cluster.ClusterCredential.Token = &token

	bootstrapToken, err := bootstraputil.GenerateBootstrapToken()
	if err != nil {
		return err
	}
	cluster.ClusterCredential.BootstrapToken = &bootstrapToken

	certBytes := make([]byte, 32)
	if _, err := rand.Read(certBytes); err != nil {
		return err
	}
	certificateKey := hex.EncodeToString(certBytes)
	cluster.ClusterCredential.CertificateKey = &certificateKey

	return nil
}

func (p *Provider) ping(resp http.ResponseWriter, req *http.Request) {
	fmt.Fprint(resp, "pong")
}

func (p *Provider) EnsureCopyFiles(ctx context.Context, c *common.Cluster) error {
	return nil
}

func (p *Provider) EnsurePreInstallHook(ctx context.Context, c *common.Cluster) error {
	return nil
}

func (p *Provider) EnsurePostInstallHook(ctx context.Context, c *common.Cluster) error {
	return nil
}

func (p *Provider) EnsureClusterComplete(ctx context.Context, cluster *common.Cluster) error {
	funcs := []func(cluster *common.Cluster) error{
		completeK8sVersion,
		completeNetworking,
		completeDNS,
		completeAddresses,
		completeCredential,
	}
	for _, f := range funcs {
		if err := f(cluster); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) EnsureCerts(ctx context.Context, c *common.Cluster) error {
	apiserver := certs.BuildApiserverEndpoint(constants.KubeApiServer, 6443)
	err := kubeadm.InitCerts(kubeadm.GetKubeadmConfig(c, p.Cfg, apiserver), c, true)
	if err != nil {
		return err
	}

	return nil
}

func (p *Provider) EnsureKubeconfig(ctx context.Context, c *common.Cluster) error {
	apiserver := certs.BuildApiserverEndpoint(constants.KubeApiServer, kubeconfig.GetBindPort(c.Cluster))
	err := kubeconfig.ApplyMasterKubeconfig(c, apiserver)
	if err != nil {
		return err
	}

	//
	return nil
}

func (p *Provider) EnsureEtcd(ctx context.Context, c *common.Cluster) error {
	return nil
}

func (p *Provider) EnsureKubeMaster(ctx context.Context, c *common.Cluster) error {
	r := &Reconciler{
		Obj:      c,
		Provider: p,
	}

	var fs []func() runtime.Object
	fs = append(fs, r.apiServerDeployment)
	fs = append(fs, r.apiServerSvc)
	fs = append(fs, r.controllerManagerDeployment)
	fs = append(fs, r.schedulerDeployment)

	logger := ctrl.Log.WithValues("cluster", c.Name)
	for _, f := range fs {
		obj := f()
		err := k8sutil.Reconcile(logger, c.Client, obj, k8sutil.DesiredStatePresent)
		if err != nil {
			return errors.Wrapf(err, "create kubeconfig err: %v", err)
		}
	}

	return nil
}

func (p *Provider) EnsureExtKubeconfig(ctx context.Context, c *common.Cluster) error {
	if c.ClusterCredential.ExtData == nil {
		c.ClusterCredential.ExtData = make(map[string]string)
	}

	apiserver := certs.BuildApiserverEndpoint(c.Cluster.Spec.Features.HA.ThirdPartyHA.VIP, kubeconfig.GetBindPort(c.Cluster))
	cfgMaps, err := certs.CreateApiserverKubeConfigFile(c.ClusterCredential.CAKey, c.ClusterCredential.CACert,
		apiserver, c.Cluster.Name)
	if err != nil {
		klog.Errorf("create kubeconfg err: %+v", err)
		return err
	}
	klog.Infof("[%s/%s] start build kubeconfig ...", c.Cluster.Namespace, c.Cluster.Name)
	for _, v := range cfgMaps {
		by, err := certs.BuildKubeConfigByte(v)
		if err != nil {
			return err
		}
		c.ClusterCredential.ExtData[pkiutil.ExternalAdminKubeConfigFileName] = string(by)
	}

	return nil
}

func (p *Provider) EnsureAddons(ctx context.Context, c *common.Cluster) error {
	clusterCtx, err := c.ClusterManager.Get(c.Name)
	if err != nil {
		return nil
	}
	kubeproxyObjs, err := kubeproxy.BuildKubeproxyAddon(p.Cfg, c)
	if err != nil {
		return errors.Wrapf(err, "build kube-proxy err: %v", err)
	}

	logger := ctrl.Log.WithValues("cluster", c.Name)
	for _, obj := range kubeproxyObjs {
		err = k8sutil.Reconcile(logger, clusterCtx.Client, obj, k8sutil.DesiredStatePresent)
		if err != nil {
			return errors.Wrapf(err, "Reconcile  err: %v", err)
		}
	}

	corednsObjs, err := coredns.BuildCoreDNSAddon(p.Cfg, c)
	if err != nil {
		return errors.Wrapf(err, "build kube-proxy err: %v", err)
	}
	for _, obj := range corednsObjs {
		err = k8sutil.Reconcile(logger, clusterCtx.Client, obj, k8sutil.DesiredStatePresent)
		if err != nil {
			return errors.Wrapf(err, "Reconcile  err: %v", err)
		}
	}
	return nil
}

func (p *Provider) EnsureFlannel(ctx context.Context, c *common.Cluster) error {
	clusterCtx, err := c.ClusterManager.Get(c.Name)
	if err != nil {
		return nil
	}
	objs, err := flannel.BuildFlannelAddon(p.Cfg, c)
	if err != nil {
		return errors.Wrapf(err, "build flannel err: %v", err)
	}

	logger := ctrl.Log.WithValues("cluster", c.Name, "component", "flannel")
	logger.Info("start reconcile ...")
	for _, obj := range objs {
		err = k8sutil.Reconcile(logger, clusterCtx.Client, obj, k8sutil.DesiredStatePresent)
		if err != nil {
			return errors.Wrapf(err, "Reconcile  err: %v", err)
		}
	}

	return nil
}

func (p *Provider) EnsureMetricsServer(ctx context.Context, c *common.Cluster) error {
	clusterCtx, err := c.ClusterManager.Get(c.Name)
	if err != nil {
		return nil
	}
	objs, err := metricsserver.BuildMetricsServerAddon(c)
	if err != nil {
		return errors.Wrapf(err, "build metrics-server err: %v", err)
	}

	logger := ctrl.Log.WithValues("cluster", c.Name, "component", "metrics-server")
	logger.Info("start reconcile ...")
	for _, obj := range objs {
		err = k8sutil.Reconcile(logger, clusterCtx.Client, obj, k8sutil.DesiredStatePresent)
		if err != nil {
			return errors.Wrapf(err, "Reconcile  err: %v", err)
		}
	}

	return nil
}

package transform

import (
	"log/slog"
	"strings"
	"time"

	"github.com/mariomac/pipes/pipe"

	"github.com/grafana/beyla/pkg/internal/export/metric/attr"
	"github.com/grafana/beyla/pkg/internal/kube"
	"github.com/grafana/beyla/pkg/internal/pipe/global"
	"github.com/grafana/beyla/pkg/internal/request"
	"github.com/grafana/beyla/pkg/internal/svc"
)

type KubeEnableFlag string

const (
	EnabledTrue       = KubeEnableFlag("true")
	EnabledFalse      = KubeEnableFlag("false")
	EnabledAutodetect = KubeEnableFlag("autodetect")
	EnabledDefault    = EnabledFalse

	// TODO: let the user decide which attributes to add, as in https://opentelemetry.io/docs/kubernetes/collector/components/#kubernetes-attributes-processor
)

func klog() *slog.Logger {
	return slog.With("component", "transform.KubernetesDecorator")
}

type KubernetesDecorator struct {
	Enable KubeEnableFlag `yaml:"enable" env:"BEYLA_KUBE_METADATA_ENABLE"`

	// ClusterName overrides cluster name. If empty, the NetO11y module will try to retrieve
	// it from the Cloud Provider Metadata (EC2, GCP and Azure), and leave it empty if it fails to.
	ClusterName string `yaml:"cluster_name" env:"BEYLA_KUBE_CLUSTER_NAME"`

	// KubeconfigPath is optional. If unset, it will look in the usual location.
	KubeconfigPath string `yaml:"kubeconfig_path" env:"KUBECONFIG"`

	InformersSyncTimeout time.Duration `yaml:"informers_sync_timeout" env:"BEYLA_KUBE_INFORMERS_SYNC_TIMEOUT"`

	// DropExternal will drop, in NetO11y component, any flow where the source or destination
	// IPs are not matched to any kubernetes entity, assuming they are cluster-external
	DropExternal bool `yaml:"drop_external" env:"BEYLA_NETWORK_DROP_EXTERNAL"`
}

func (d KubernetesDecorator) Enabled() bool {
	switch strings.ToLower(string(d.Enable)) {
	case string(EnabledTrue):
		return true
	case string(EnabledFalse), "": // empty value is disabled
		return false
	case string(EnabledAutodetect):
		// We autodetect that we are in a kubernetes if we can properly load a K8s configuration file
		_, err := kube.LoadConfig(d.KubeconfigPath)
		if err != nil {
			klog().Debug("kubeconfig can't be detected. Assuming we are not in Kubernetes", "error", err)
			return false
		}
		return true
	default:
		klog().Warn("invalid value for Enable value. Ignoring stage", "value", d.Enable)
		return false
	}
}

func KubeDecoratorProvider(
	ctxInfo *global.ContextInfo, kubeDecorator *KubernetesDecorator,
) pipe.MiddleProvider[[]request.Span, []request.Span] {
	return func() (pipe.MiddleFunc[[]request.Span, []request.Span], error) {
		if !kubeDecorator.Enabled() {
			// if kubernetes decoration is disabled, we just bypass the node
			return pipe.Bypass[[]request.Span](), nil
		}
		decorator := &metadataDecorator{db: ctxInfo.AppO11y.K8sDatabase}
		return decorator.nodeLoop, nil
	}
}

// production implementer: kube.Database
type kubeDatabase interface {
	OwnerPodInfo(pidNamespace uint32) (*kube.PodInfo, bool)
}

type metadataDecorator struct {
	db kubeDatabase
}

func (md *metadataDecorator) nodeLoop(in <-chan []request.Span, out chan<- []request.Span) {
	klog().Debug("starting kubernetes decoration loop")
	for spans := range in {
		// in-place decoration and forwarding
		for i := range spans {
			md.do(&spans[i])
		}
		out <- spans
	}
	klog().Debug("stopping kubernetes decoration loop")
}

func (md *metadataDecorator) do(span *request.Span) {
	if podInfo, ok := md.db.OwnerPodInfo(span.Pid.Namespace); ok {
		appendMetadata(span, podInfo)
	} else {
		// do not leave the service attributes map as nil
		span.ServiceID.Metadata = map[attr.Name]string{}
	}
}

func appendMetadata(span *request.Span, info *kube.PodInfo) {
	// If the user has not defined criteria values for the reported
	// service name and namespace, we will automatically set it from
	// the kubernetes metadata
	if span.ServiceID.AutoName {
		span.ServiceID.Name = info.ServiceName()
	}
	if span.ServiceID.Namespace == "" {
		span.ServiceID.Namespace = info.Namespace
	}
	span.ServiceID.UID = svc.UID(info.UID)

	// if, in the future, other pipeline steps modify the service metadata, we should
	// replace the map literal by individual entry insertions
	span.ServiceID.Metadata = map[attr.Name]string{
		attr.K8sNamespaceName: info.Namespace,
		attr.K8sPodName:       info.Name,
		attr.K8sNodeName:      info.NodeName,
		attr.K8sPodUID:        string(info.UID),
		attr.K8sPodStartTime:  info.StartTimeStr,
	}
	owner := info.Owner
	for owner != nil {
		span.ServiceID.Metadata[owner.Type.LabelName()] = owner.Name
		owner = owner.Owner
	}
}

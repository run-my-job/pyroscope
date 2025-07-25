package discovery

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/hashicorp/raft"
	"google.golang.org/grpc/resolver"

	"github.com/grafana/pyroscope/pkg/metastore/discovery/kuberesolver"
)

type KubeDiscovery struct {
	l        log.Logger
	resolver *kuberesolver.KResolver
	ti       targetInfo

	servers []Server
	updLock sync.Mutex
	upd     Updates
}

func (g *KubeDiscovery) Rediscover() {

}

func NewKubeResolverDiscovery(l log.Logger, target string, client kuberesolver.K8sClient) (*KubeDiscovery, error) {
	var err error
	l = log.With(l, "target", target, "component", "kuberesolver-discovery")
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	gt := resolver.Target{URL: *u}
	ti, err := parseResolverTarget(gt)
	if err != nil {
		return nil, err
	}
	level.Info(l).Log("msg", "parsed target", "target_namespace", ti.namespace, "target_service", ti.service, "target_port", ti.port)

	res := &KubeDiscovery{
		l:  l,
		ti: ti,
	}
	ku := kuberesolver.ResolveUpdatesFunc(func(e kuberesolver.Endpoints) {
		res.resolved(e)
	})

	r, err := kuberesolver.Build(l, client, ku, kuberesolver.TargetInfo{
		ServiceName:      ti.service,
		ServiceNamespace: ti.namespace,
	})
	if err != nil {
		return nil, err
	}

	res.resolver = r

	return res, nil
}

func (g *KubeDiscovery) Subscribe(upd Updates) {
	g.updLock.Lock()
	defer g.updLock.Unlock()
	g.upd = upd
	g.upd.Servers(g.servers)
}

func (g *KubeDiscovery) Close() {
	g.updLock.Lock()
	defer g.updLock.Unlock()
	g.upd = nil
	g.resolver.Close()
}

func (g *KubeDiscovery) resolved(e kuberesolver.Endpoints) {
	for _, subset := range e.Subsets {
		for _, addr := range subset.Addresses {
			level.Debug(g.l).Log("msg", "resolved", "ip", addr.IP, "targetRef", fmt.Sprintf("%+v", addr.TargetRef))
		}
	}
	g.updLock.Lock()
	defer g.updLock.Unlock()
	g.servers = convertEndpoints(e, g.ti)
	if g.upd != nil {
		g.upd.Servers(g.servers)
	}
}

func convertEndpoints(e kuberesolver.Endpoints, ti targetInfo) []Server {
	var servers []Server
	for _, ep := range e.Subsets {
		for _, addr := range ep.Addresses {
			for _, port := range ep.Ports {
				if fmt.Sprintf("%d", port.Port) != ti.port {
					continue
				}
				podName := addr.TargetRef.Name
				raftServerId := fmt.Sprintf("%s.%s.%s.svc.cluster.local.:%d", podName, ti.service, ti.namespace, port.Port)

				servers = append(servers, Server{
					ResolvedAddress: net.JoinHostPort(addr.IP, strconv.Itoa(port.Port)),
					Raft: raft.Server{
						ID:      raft.ServerID(raftServerId),
						Address: raft.ServerAddress(raftServerId),
					},
				})

			}
		}
	}
	return servers
}

type targetInfo struct {
	namespace, service, port string
}

func parseResolverTarget(target resolver.Target) (targetInfo, error) {
	var service, port, namespace string
	if target.URL.Host == "" {
		// kubernetes:///service.namespace:port
		service, port, namespace = splitServicePortNamespace(target.Endpoint())
	} else if target.URL.Port() == "" && target.Endpoint() != "" {
		// kubernetes://namespace/service:port
		service, port, _ = splitServicePortNamespace(target.Endpoint())
		namespace = target.URL.Hostname()
	} else {
		// kubernetes://service.namespace:port
		service, port, namespace = splitServicePortNamespace(target.URL.Host)
	}

	if service == "" {
		return targetInfo{}, fmt.Errorf("target %s must specify a service", &target.URL)
	}
	if namespace == "" {
		return targetInfo{}, fmt.Errorf("target %s must specify a namespace", &target.URL)
	}
	if port == "" {
		return targetInfo{}, fmt.Errorf("target %s must specify a port", &target.URL)
	}
	return targetInfo{
		service:   service,
		namespace: namespace,
		port:      port,
	}, nil
}

func splitServicePortNamespace(hpn string) (service, port, namespace string) {
	service = hpn

	colon := strings.LastIndexByte(service, ':')
	if colon != -1 {
		service, port = service[:colon], service[colon+1:]
	}

	// we want to split into the service name, namespace, and whatever else is left
	// this will support fully qualified service names, e.g. {service-name}.<namespace>.svc.<cluster-domain-name>.
	// Note that since we lookup the endpoints by service name and namespace, we don't care about the
	// cluster-domain-name, only that we can parse out the service name and namespace properly.
	parts := strings.SplitN(service, ".", 3)
	if len(parts) >= 2 {
		service, namespace = parts[0], parts[1]
	}

	return
}

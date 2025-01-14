package consul

import (
	"crypto/x509"
	"sync"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/command/connect/proxy"
	log "github.com/sirupsen/logrus"
)

const (
	defaultDownstreamBindAddr = "0.0.0.0"
	defaultUpstreamBindAddr   = "127.0.0.1"

	errorWaitTime = 5 * time.Second
)

type upstream struct {
	LocalBindAddress string
	LocalBindPort    int
	Service          string
	Datacenter       string
	Nodes            []*api.ServiceEntry

	done bool
}

type downstream struct {
	LocalBindAddress string
	LocalBindPort    int
	TargetAddress    string
	TargetPort       int
}

type certLeaf struct {
	Cert []byte
	Key  []byte

	done bool
}

type Watcher struct {
	service     string
	serviceName string
	consul      *api.Client
	token       string
	C           chan Config

	lock  sync.Mutex
	ready sync.WaitGroup

	upstreams  map[string]*upstream
	downstream downstream
	certCAs    [][]byte
	certCAPool *x509.CertPool
	leaf       *certLeaf

	update chan struct{}
}

func New(service string, consul *api.Client) *Watcher {
	return &Watcher{
		service: service,
		consul:  consul,

		C:         make(chan Config),
		upstreams: make(map[string]*upstream),
		update:    make(chan struct{}, 1),
	}
}

func (w *Watcher) Run() error {
	proxyID, err := proxy.LookupProxyIDForSidecar(w.consul, w.service)
	if err != nil {
		return err
	}

	svc, _, err := w.consul.Agent().Service(w.service, &api.QueryOptions{})
	if err != nil {
		return err
	}

	w.serviceName = svc.Service

	w.ready.Add(4)

	go w.watchCA()
	go w.watchLeaf(w.serviceName)
	go w.watchService(proxyID, w.handleProxyChange)
	go w.watchService(w.service, func(first bool, srv *api.AgentService) {
		w.downstream.TargetPort = srv.Port
		if first {
			w.ready.Done()
		}
	})

	w.ready.Wait()

	for range w.update {
		w.C <- w.genCfg()
	}

	return nil
}

func (w *Watcher) handleProxyChange(first bool, srv *api.AgentService) {
	w.downstream.LocalBindAddress = defaultDownstreamBindAddr
	w.downstream.LocalBindPort = srv.Port
	w.downstream.TargetAddress = defaultUpstreamBindAddr
	if srv.Connect != nil && srv.Connect.Proxy != nil && srv.Connect.Proxy.Config != nil {
		if b, ok := srv.Connect.Proxy.Config["bind_address"].(string); ok {
			w.downstream.LocalBindAddress = b
		}
		if a, ok := srv.Connect.Proxy.Config["local_service_address"].(string); ok {
			w.downstream.TargetAddress = a
		}
	}

	keep := make(map[string]bool)

	if srv.Proxy != nil {
		for _, up := range srv.Proxy.Upstreams {
			keep[up.DestinationName] = true
			w.lock.Lock()
			_, ok := w.upstreams[up.DestinationName]
			w.lock.Unlock()
			if !ok {
				w.startUpstream(up)
			}
		}
	}

	for name := range w.upstreams {
		if !keep[name] {
			w.removeUpstream(name)
		}
	}

	if first {
		w.ready.Done()
	}
}

func (w *Watcher) startUpstream(up api.Upstream) {
	log.Infof("consul: watching upstream for service %s", up.DestinationName)

	u := &upstream{
		LocalBindAddress: up.LocalBindAddress,
		LocalBindPort:    up.LocalBindPort,
		Service:          up.DestinationName,
		Datacenter:       up.Datacenter,
	}

	w.lock.Lock()
	w.upstreams[up.DestinationName] = u
	w.lock.Unlock()

	go func() {
		index := uint64(0)
		for {
			if u.done {
				return
			}
			nodes, meta, err := w.consul.Health().Connect(up.DestinationName, "", true, &api.QueryOptions{
				Datacenter: up.Datacenter,
				WaitTime:   10 * time.Minute,
				WaitIndex:  index,
			})
			if err != nil {
				log.Errorf("consul: error fetching service definition for service %s: %s", up.DestinationName, err)
				time.Sleep(errorWaitTime)
				index = 0
				continue
			}
			changed := index != meta.LastIndex
			index = meta.LastIndex

			if changed {
				w.lock.Lock()
				u.Nodes = nodes
				w.lock.Unlock()
				w.notifyChanged()
			}
		}
	}()
}

func (w *Watcher) removeUpstream(name string) {
	log.Infof("consul: removing upstream for service %s", name)

	w.lock.Lock()
	w.upstreams[name].done = true
	delete(w.upstreams, name)
	w.lock.Unlock()
}

func (w *Watcher) watchLeaf(service string) {
	log.Debugf("consul: watching leaf cert for %s", service)

	var lastIndex uint64
	first := true
	for {
		// if the upsteam was removed, stop watching its leaf
		_, upstreamRunning := w.upstreams[service]
		if service != w.serviceName && !upstreamRunning {
			log.Debugf("consul: stopping watching leaf cert for %s", service)
			return
		}

		cert, meta, err := w.consul.Agent().ConnectCALeaf(service, &api.QueryOptions{
			WaitTime:  10 * time.Minute,
			WaitIndex: lastIndex,
		})
		if err != nil {
			log.Errorf("consul error fetching leaf cert for service %s: %s", service, err)
			time.Sleep(errorWaitTime)
			lastIndex = 0
			continue
		}

		changed := lastIndex != meta.LastIndex
		lastIndex = meta.LastIndex

		if changed {
			log.Debugf("consul: leaf cert for service %s changed", service)
			w.lock.Lock()
			if w.leaf == nil {
				w.leaf = &certLeaf{}
			}
			w.leaf.Cert = []byte(cert.CertPEM)
			w.leaf.Key = []byte(cert.PrivateKeyPEM)
			w.lock.Unlock()
			w.notifyChanged()
		}

		if first {
			log.Debugf("consul: leaf cert for %s ready", service)
			w.ready.Done()
			first = false
		}
	}
}

func (w *Watcher) watchService(service string, handler func(first bool, srv *api.AgentService)) {
	log.Infof("consul: wacthing service %s", service)

	hash := ""
	first := true
	for {
		srv, meta, err := w.consul.Agent().Service(service, &api.QueryOptions{
			WaitHash: hash,
			WaitTime: 10 * time.Minute,
		})
		if err != nil {
			log.Errorf("consul: error fetching service definition: %s", err)
			time.Sleep(errorWaitTime)
			hash = ""
			continue
		}

		changed := hash != meta.LastContentHash
		hash = meta.LastContentHash

		if changed {
			log.Debugf("consul: service %s changed", service)
			handler(first, srv)
			w.notifyChanged()
		}

		first = false
	}
}

func (w *Watcher) watchCA() {
	log.Debugf("consul: watching ca certs")

	first := true
	var lastIndex uint64
	for {
		caList, meta, err := w.consul.Agent().ConnectCARoots(&api.QueryOptions{
			WaitIndex: lastIndex,
			WaitTime:  10 * time.Minute,
		})
		if err != nil {
			log.Errorf("consul: error fetching cas: %s", err)
			time.Sleep(errorWaitTime)
			lastIndex = 0
			continue
		}

		changed := lastIndex != meta.LastIndex
		lastIndex = meta.LastIndex

		if changed {
			log.Debugf("consul: CA certs changed")
			w.lock.Lock()
			w.certCAs = w.certCAs[:0]
			w.certCAPool = x509.NewCertPool()
			for _, ca := range caList.Roots {
				w.certCAs = append(w.certCAs, []byte(ca.RootCertPEM))
				ok := w.certCAPool.AppendCertsFromPEM([]byte(ca.RootCertPEM))
				if !ok {
					log.Warn("consul: unable to add CA certificate to pool")
				}
			}
			w.lock.Unlock()
			w.notifyChanged()
		}

		if first {
			log.Debugf("consul: CA certs ready")
			w.ready.Done()
			first = false
		}
	}
}

func (w *Watcher) genCfg() Config {
	w.lock.Lock()
	defer w.lock.Unlock()

	config := Config{
		ServiceName: w.serviceName,
		ServiceID:   w.service,
		CAsPool:     w.certCAPool,
		Downstream: Downstream{
			LocalBindAddress: w.downstream.LocalBindAddress,
			LocalBindPort:    w.downstream.LocalBindPort,
			TargetAddress:    w.downstream.TargetAddress,
			TargetPort:       w.downstream.TargetPort,

			TLS: TLS{
				CAs:  w.certCAs,
				Cert: w.leaf.Cert,
				Key:  w.leaf.Key,
			},
		},
	}

	for _, up := range w.upstreams {
		upstream := Upstream{
			Service:          up.Service,
			LocalBindAddress: up.LocalBindAddress,
			LocalBindPort:    up.LocalBindPort,

			TLS: TLS{
				CAs:  w.certCAs,
				Cert: w.leaf.Cert,
				Key:  w.leaf.Key,
			},
		}

		for _, s := range up.Nodes {
			host := s.Service.Address
			if host == "" {
				host = s.Node.Address
			}

			weight := 1
			switch s.Checks.AggregatedStatus() {
			case api.HealthPassing:
				weight = s.Service.Weights.Passing
			case api.HealthWarning:
				weight = s.Service.Weights.Warning
			default:
				continue
			}
			if weight == 0 {
				continue
			}

			upstream.Nodes = append(upstream.Nodes, UpstreamNode{
				Host:   host,
				Port:   s.Service.Port,
				Weight: weight,
			})
		}

		config.Upstreams = append(config.Upstreams, upstream)
	}

	return config
}

func (w *Watcher) notifyChanged() {
	select {
	case w.update <- struct{}{}:
	default:
	}
}

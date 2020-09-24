package agent

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/hashicorp/consul/agent/config"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/logging"
	"github.com/hashicorp/go-hclog"
)

// metaExternalSource is the key name for the service instance meta that
// defines the external syncing source. This is used by the UI APIs below
// to extract this.
const metaExternalSource = "external-source"

type GatewayConfig struct {
	Addresses []string `json:",omitempty"`
	// internal to track uniqueness
	addressesSet map[string]struct{}
}

// ServiceSummary is used to summarize a service
type ServiceSummary struct {
	Kind              structs.ServiceKind `json:",omitempty"`
	Name              string
	Tags              []string
	Nodes             []string
	InstanceCount     int
	ProxyFor          []string            `json:",omitempty"`
	proxyForSet       map[string]struct{} // internal to track uniqueness
	ChecksPassing     int
	ChecksWarning     int
	ChecksCritical    int
	ExternalSources   []string
	externalSourceSet map[string]struct{} // internal to track uniqueness
	GatewayConfig     GatewayConfig       `json:",omitempty"`

	structs.EnterpriseMeta
}

// UINodes is used to list the nodes in a given datacenter. We return a
// NodeDump which provides overview information for all the nodes
func (s *HTTPHandlers) UINodes(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Parse arguments
	args := structs.DCSpecificRequest{}
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	if err := s.parseEntMeta(req, &args.EnterpriseMeta); err != nil {
		return nil, err
	}

	s.parseFilter(req, &args.Filter)

	// Make the RPC request
	var out structs.IndexedNodeDump
	defer setMeta(resp, &out.QueryMeta)
RPC:
	if err := s.agent.RPC("Internal.NodeDump", &args, &out); err != nil {
		// Retry the request allowing stale data if no leader
		if strings.Contains(err.Error(), structs.ErrNoLeader.Error()) && !args.AllowStale {
			args.AllowStale = true
			goto RPC
		}
		return nil, err
	}

	// Use empty list instead of nil
	for _, info := range out.Dump {
		if info.Services == nil {
			info.Services = make([]*structs.NodeService, 0)
		}
		if info.Checks == nil {
			info.Checks = make([]*structs.HealthCheck, 0)
		}
	}
	if out.Dump == nil {
		out.Dump = make(structs.NodeDump, 0)
	}
	return out.Dump, nil
}

// UINodeInfo is used to get info on a single node in a given datacenter. We return a
// NodeInfo which provides overview information for the node
func (s *HTTPHandlers) UINodeInfo(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Parse arguments
	args := structs.NodeSpecificRequest{}
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	if err := s.parseEntMeta(req, &args.EnterpriseMeta); err != nil {
		return nil, err
	}

	// Verify we have some DC, or use the default
	args.Node = strings.TrimPrefix(req.URL.Path, "/v1/internal/ui/node/")
	if args.Node == "" {
		resp.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(resp, "Missing node name")
		return nil, nil
	}

	// Make the RPC request
	var out structs.IndexedNodeDump
	defer setMeta(resp, &out.QueryMeta)
RPC:
	if err := s.agent.RPC("Internal.NodeInfo", &args, &out); err != nil {
		// Retry the request allowing stale data if no leader
		if strings.Contains(err.Error(), structs.ErrNoLeader.Error()) && !args.AllowStale {
			args.AllowStale = true
			goto RPC
		}
		return nil, err
	}

	// Return only the first entry
	if len(out.Dump) > 0 {
		info := out.Dump[0]
		if info.Services == nil {
			info.Services = make([]*structs.NodeService, 0)
		}
		if info.Checks == nil {
			info.Checks = make([]*structs.HealthCheck, 0)
		}
		return info, nil
	}

	resp.WriteHeader(http.StatusNotFound)
	return nil, nil
}

// UIServices is used to list the services in a given datacenter. We return a
// ServiceSummary which provides overview information for the service
func (s *HTTPHandlers) UIServices(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Parse arguments
	args := structs.ServiceDumpRequest{}
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	if err := s.parseEntMeta(req, &args.EnterpriseMeta); err != nil {
		return nil, err
	}

	s.parseFilter(req, &args.Filter)

	// Make the RPC request
	var out structs.IndexedCheckServiceNodes
	defer setMeta(resp, &out.QueryMeta)
RPC:
	if err := s.agent.RPC("Internal.ServiceDump", &args, &out); err != nil {
		// Retry the request allowing stale data if no leader
		if strings.Contains(err.Error(), structs.ErrNoLeader.Error()) && !args.AllowStale {
			args.AllowStale = true
			goto RPC
		}
		return nil, err
	}

	// Generate the summary
	// TODO (gateways) (freddy) Have Internal.ServiceDump return ServiceDump instead. Need to add bexpr filtering for type.
	return summarizeServices(out.Nodes.ToServiceDump(), s.agent.config, args.Datacenter), nil
}

// UIGatewayServices is used to query all the nodes for services associated with a gateway along with their gateway config
func (s *HTTPHandlers) UIGatewayServicesNodes(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Parse arguments
	args := structs.ServiceSpecificRequest{}
	if err := s.parseEntMetaNoWildcard(req, &args.EnterpriseMeta); err != nil {
		return nil, err
	}
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	// Pull out the service name
	args.ServiceName = strings.TrimPrefix(req.URL.Path, "/v1/internal/ui/gateway-services-nodes/")
	if args.ServiceName == "" {
		resp.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(resp, "Missing gateway name")
		return nil, nil
	}

	// Make the RPC request
	var out structs.IndexedServiceDump
	defer setMeta(resp, &out.QueryMeta)
RPC:
	if err := s.agent.RPC("Internal.GatewayServiceDump", &args, &out); err != nil {
		// Retry the request allowing stale data if no leader
		if strings.Contains(err.Error(), structs.ErrNoLeader.Error()) && !args.AllowStale {
			args.AllowStale = true
			goto RPC
		}
		return nil, err
	}

	return summarizeServices(out.Dump, s.agent.config, args.Datacenter), nil
}

func summarizeServices(dump structs.ServiceDump, cfg *config.RuntimeConfig, datacenter string) []*ServiceSummary {
	// Collect the summary information
	var services []structs.ServiceID
	summary := make(map[structs.ServiceID]*ServiceSummary)
	getService := func(service structs.ServiceID) *ServiceSummary {
		serv, ok := summary[service]
		if !ok {
			serv = &ServiceSummary{
				Name:           service.ID,
				EnterpriseMeta: service.EnterpriseMeta,
				// the other code will increment this unconditionally so we
				// shouldn't initialize it to 1
				InstanceCount: 0,
			}
			summary[service] = serv
			services = append(services, service)
		}
		return serv
	}

	for _, csn := range dump {
		if csn.GatewayService != nil {
			gwsvc := csn.GatewayService
			sum := getService(gwsvc.Service.ToServiceID())
			modifySummaryForGatewayService(cfg, datacenter, sum, gwsvc)
		}

		// Will happen in cases where we only have the GatewayServices mapping
		if csn.Service == nil {
			continue
		}
		sid := structs.NewServiceID(csn.Service.Service, &csn.Service.EnterpriseMeta)
		sum := getService(sid)

		svc := csn.Service
		sum.Nodes = append(sum.Nodes, csn.Node.Node)
		sum.Kind = svc.Kind
		sum.InstanceCount += 1
		if svc.Kind == structs.ServiceKindConnectProxy {
			if _, ok := sum.proxyForSet[svc.Proxy.DestinationServiceName]; !ok {
				if sum.proxyForSet == nil {
					sum.proxyForSet = make(map[string]struct{})
				}
				sum.proxyForSet[svc.Proxy.DestinationServiceName] = struct{}{}
				sum.ProxyFor = append(sum.ProxyFor, svc.Proxy.DestinationServiceName)
			}
		}
		for _, tag := range svc.Tags {
			found := false
			for _, existing := range sum.Tags {
				if existing == tag {
					found = true
					break
				}
			}

			if !found {
				sum.Tags = append(sum.Tags, tag)
			}
		}

		// If there is an external source, add it to the list of external
		// sources. We only want to add unique sources so there is extra
		// accounting here with an unexported field to maintain the set
		// of sources.
		if len(svc.Meta) > 0 && svc.Meta[metaExternalSource] != "" {
			source := svc.Meta[metaExternalSource]
			if sum.externalSourceSet == nil {
				sum.externalSourceSet = make(map[string]struct{})
			}
			if _, ok := sum.externalSourceSet[source]; !ok {
				sum.externalSourceSet[source] = struct{}{}
				sum.ExternalSources = append(sum.ExternalSources, source)
			}
		}

		for _, check := range csn.Checks {
			switch check.Status {
			case api.HealthPassing:
				sum.ChecksPassing++
			case api.HealthWarning:
				sum.ChecksWarning++
			case api.HealthCritical:
				sum.ChecksCritical++
			}
		}
	}

	// Return the services in sorted order
	sort.Slice(services, func(i, j int) bool {
		return services[i].LessThan(&services[j])
	})
	output := make([]*ServiceSummary, len(summary))
	for idx, service := range services {
		// Sort the nodes and tags
		sum := summary[service]
		sort.Strings(sum.Nodes)
		sort.Strings(sum.Tags)
		output[idx] = sum
	}
	return output
}

func modifySummaryForGatewayService(
	cfg *config.RuntimeConfig,
	datacenter string,
	sum *ServiceSummary,
	gwsvc *structs.GatewayService,
) {
	var dnsAddresses []string
	for _, domain := range []string{cfg.DNSDomain, cfg.DNSAltDomain} {
		// If the domain is empty, do not use it to construct a valid DNS
		// address
		if domain == "" {
			continue
		}
		dnsAddresses = append(dnsAddresses, serviceIngressDNSName(
			gwsvc.Service.Name,
			datacenter,
			domain,
			&gwsvc.Service.EnterpriseMeta,
		))
	}

	for _, addr := range gwsvc.Addresses(dnsAddresses) {
		// check for duplicates, a service will have a ServiceInfo struct for
		// every instance that is registered.
		if _, ok := sum.GatewayConfig.addressesSet[addr]; !ok {
			if sum.GatewayConfig.addressesSet == nil {
				sum.GatewayConfig.addressesSet = make(map[string]struct{})
			}
			sum.GatewayConfig.addressesSet[addr] = struct{}{}
			sum.GatewayConfig.Addresses = append(
				sum.GatewayConfig.Addresses, addr,
			)
		}
	}
}

// GET /v1/internal/ui/gateway-intentions/:gateway
func (s *HTTPHandlers) UIGatewayIntentions(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	var args structs.IntentionQueryRequest
	if done := s.parse(resp, req, &args.Datacenter, &args.QueryOptions); done {
		return nil, nil
	}

	var entMeta structs.EnterpriseMeta
	if err := s.parseEntMetaNoWildcard(req, &entMeta); err != nil {
		return nil, err
	}

	// Pull out the service name
	name := strings.TrimPrefix(req.URL.Path, "/v1/internal/ui/gateway-intentions/")
	if name == "" {
		resp.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(resp, "Missing gateway name")
		return nil, nil
	}
	args.Match = &structs.IntentionQueryMatch{
		Type: structs.IntentionMatchDestination,
		Entries: []structs.IntentionMatchEntry{
			{
				Namespace: entMeta.NamespaceOrEmpty(),
				Name:      name,
			},
		},
	}

	var reply structs.IndexedIntentions

	defer setMeta(resp, &reply.QueryMeta)
	if err := s.agent.RPC("Internal.GatewayIntentions", args, &reply); err != nil {
		return nil, err
	}

	return reply.Intentions, nil
}

// UIMetricsProxy handles the /v1/internal/ui/metrics-proxy/ endpoint which, if
// configured, provides a simple read-only HTTP proxy to a single metrics
// backend to expose it to the UI.
func (s *HTTPHandlers) UIMetricsProxy(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	// Check the UI was enabled at agent startup (note this is not reloadable
	// currently).
	if !s.IsUIEnabled() {
		return nil, NotFoundError{Reason: "UI is not enabled"}
	}

	// Load reloadable proxy config
	cfg, ok := s.metricsProxyCfg.Load().(config.UIMetricsProxy)
	if !ok || cfg.BaseURL == "" {
		// Proxy not configured
		return nil, NotFoundError{Reason: "Metrics proxy is not enabled"}
	}

	log := s.agent.logger.Named(logging.UIMetricsProxy)

	// Construct the new URL from the path and the base path. Note we do this here
	// not in the Director function below because we can handle any errors cleanly
	// here.

	// Replace prefix in the path
	subPath := strings.TrimPrefix(req.URL.Path, "/v1/internal/ui/metrics-proxy")

	// Append that to the BaseURL (which might contain a path prefix component)
	newURL := cfg.BaseURL + subPath

	// Parse it into a new URL
	u, err := url.Parse(newURL)
	if err != nil {
		log.Error("couldn't parse target URL", "base_url", cfg.BaseURL, "path", subPath)
		return nil, BadRequestError{Reason: "Invalid path."}
	}

	// Clean the new URL path to prevent path traversal attacks and remove any
	// double slashes etc.
	u.Path = path.Clean(u.Path)

	// Validate that the full BaseURL is still a prefix - if there was a path
	// prefix on the BaseURL but an attacker tried to circumvent it with path
	// traversal then the Clean above would have resolve the /../ components back
	// to the actual path which means part of the prefix will now be missing.
	//
	// Note that in practice this is not currently possible since any /../ in the
	// path would have already been resolved by the API server mux and so not even
	// hit this handler. Any /../ that are far enough into the path to hit this
	// handler, can't backtrack far enough to eat into the BaseURL either. But we
	// leave this in anyway in case something changes in the future.
	if !strings.HasPrefix(u.String(), cfg.BaseURL) {
		log.Error("target URL escaped from base path",
			"base_url", cfg.BaseURL,
			"path", subPath,
			"target_url", u.String(),
		)
		return nil, BadRequestError{Reason: "Invalid path."}
	}

	proxy := httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL = u
		},
		ErrorLog: log.StandardLogger(&hclog.StandardLoggerOptions{
			InferLevels: true,
		}),
	}

	proxy.ServeHTTP(resp, req)
	return nil, nil
}

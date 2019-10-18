// Copyright 2019 Google Cloud Platform Proxy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package configgenerator

import (
	"fmt"
	"strings"

	"cloudesf.googlesource.com/gcpproxy/src/go/options"
	"cloudesf.googlesource.com/gcpproxy/src/go/util"
	"github.com/golang/glog"
	"github.com/golang/protobuf/ptypes"

	sc "cloudesf.googlesource.com/gcpproxy/src/go/configinfo"
	bapb "cloudesf.googlesource.com/gcpproxy/src/go/proto/api/envoy/http/backend_auth"
	brpb "cloudesf.googlesource.com/gcpproxy/src/go/proto/api/envoy/http/backend_routing"
	commonpb "cloudesf.googlesource.com/gcpproxy/src/go/proto/api/envoy/http/common"
	pmpb "cloudesf.googlesource.com/gcpproxy/src/go/proto/api/envoy/http/path_matcher"
	scpb "cloudesf.googlesource.com/gcpproxy/src/go/proto/api/envoy/http/service_control"
	v2pb "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	corepb "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	listenerpb "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	jwtpb "github.com/envoyproxy/go-control-plane/envoy/config/filter/http/jwt_authn/v2alpha"
	routerpb "github.com/envoyproxy/go-control-plane/envoy/config/filter/http/router/v2"
	transcoderpb "github.com/envoyproxy/go-control-plane/envoy/config/filter/http/transcoder/v2"
	hcmpb "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	anypb "github.com/golang/protobuf/ptypes/any"
	durationpb "github.com/golang/protobuf/ptypes/duration"
	structpb "github.com/golang/protobuf/ptypes/struct"
	wrapperspb "github.com/golang/protobuf/ptypes/wrappers"
	confpb "google.golang.org/genproto/googleapis/api/serviceconfig"
	smpb "google.golang.org/genproto/googleapis/api/servicemanagement/v1"
)

const (
	statPrefix = "ingress_http"
)

// MakeListener provides a dynamic listener for Envoy
func MakeListener(serviceInfo *sc.ServiceInfo) (*v2pb.Listener, error) {
	httpFilters := []*hcmpb.HttpFilter{}

	if serviceInfo.Options.CorsPreset == "basic" || serviceInfo.Options.CorsPreset == "cors_with_regex" {
		corsFilter := &hcmpb.HttpFilter{
			Name: util.CORS,
		}
		httpFilters = append(httpFilters, corsFilter)
		glog.Infof("adding CORS Filter config: %v", corsFilter)
	}

	// Add Path Matcher filter. The following filters rely on the dynamic
	// metadata populated by Path Matcher filter.
	// * Jwt Authentication filter
	// * Service Control filter
	// * Backend Authentication filter
	// * Backend Routing filter
	pathMathcherFilter := makePathMatcherFilter(serviceInfo)
	if pathMathcherFilter != nil {
		httpFilters = append(httpFilters, pathMathcherFilter)
		glog.Infof("adding Path Matcher Filter config: %v", pathMathcherFilter)
	}

	// Add JWT Authn filter if needed.
	if !serviceInfo.Options.SkipJwtAuthnFilter {
		jwtAuthnFilter := makeJwtAuthnFilter(serviceInfo)
		if jwtAuthnFilter != nil {
			httpFilters = append(httpFilters, jwtAuthnFilter)
			glog.Infof("adding JWT Authn Filter config: %v", jwtAuthnFilter)
		}
	}

	// Add Service Control filter if needed.
	if !serviceInfo.Options.SkipServiceControlFilter {
		serviceControlFilter := makeServiceControlFilter(serviceInfo)
		if serviceControlFilter != nil {
			httpFilters = append(httpFilters, serviceControlFilter)
			glog.Infof("adding Service Control Filter config: %v", serviceControlFilter)
		}
	}

	// Add gRPC Transcoder filter and gRPCWeb filter configs for gRPC backend.
	if serviceInfo.BackendProtocol == util.GRPC {
		transcoderFilter := makeTranscoderFilter(serviceInfo)
		if transcoderFilter != nil {
			httpFilters = append(httpFilters, transcoderFilter)
			glog.Infof("adding Transcoder Filter config...")
		}

		grpcWebFilter := &hcmpb.HttpFilter{
			Name:       util.GRPCWeb,
			ConfigType: &hcmpb.HttpFilter_Config{Config: &structpb.Struct{}},
		}
		httpFilters = append(httpFilters, grpcWebFilter)
	}

	// Add Backend Auth filter and Backend Routing if needed.
	if serviceInfo.Options.EnableBackendRouting {
		if serviceInfo.Options.ServiceAccountKey != "" {
			return nil, fmt.Errorf("ServiceAccountKey is set(proxy runs on Non-GCP) while backendRouting is not allowed on Non-GCP")
		}
		backendAuthFilter := makeBackendAuthFilter(serviceInfo)
		httpFilters = append(httpFilters, backendAuthFilter)
		glog.Infof("adding Backend Auth Filter config: %v", backendAuthFilter)
		backendRoutingFilter := makeBackendRoutingFilter(serviceInfo)
		httpFilters = append(httpFilters, backendRoutingFilter)
		glog.Infof("adding Backend Routing Filter config: %v", backendRoutingFilter)
	}

	// Add Envoy Router filter so requests are routed upstream.
	// Router filter should be the last.

	routerFilter := makeRouterFilter(serviceInfo.Options)
	httpFilters = append(httpFilters, routerFilter)

	route, err := MakeRouteConfig(serviceInfo)

	if err != nil {
		return nil, fmt.Errorf("makeHttpConnectionManagerRouteConfig got err: %s", err)
	}

	httpConMgr := &hcmpb.HttpConnectionManager{
		CodecType:  hcmpb.HttpConnectionManager_AUTO,
		StatPrefix: statPrefix,
		RouteSpecifier: &hcmpb.HttpConnectionManager_RouteConfig{
			RouteConfig: route,
		},

		UseRemoteAddress:  &wrapperspb.BoolValue{Value: serviceInfo.Options.EnvoyUseRemoteAddress},
		XffNumTrustedHops: uint32(serviceInfo.Options.EnvoyXffNumTrustedHops),
	}
	if serviceInfo.Options.EnableTracing {
		httpConMgr.Tracing = &hcmpb.HttpConnectionManager_Tracing{}
	}

	glog.Infof("adding Http Connection Manager config: %v", httpConMgr)
	httpConMgr.HttpFilters = httpFilters

	// HTTP filter configuration
	httpFilterConfig, err := util.MessageToStruct(httpConMgr)
	if err != nil {
		return nil, err
	}

	return &v2pb.Listener{
		Address: &corepb.Address{
			Address: &corepb.Address_SocketAddress{
				SocketAddress: &corepb.SocketAddress{
					Address: serviceInfo.Options.ListenerAddress,
					PortSpecifier: &corepb.SocketAddress_PortValue{
						PortValue: uint32(serviceInfo.Options.ListenerPort),
					},
				},
			},
		},
		FilterChains: []*listenerpb.FilterChain{
			{
				Filters: []*listenerpb.Filter{
					{
						Name:       util.HTTPConnectionManager,
						ConfigType: &listenerpb.Filter_Config{httpFilterConfig},
					},
				},
			},
		},
	}, nil
}

func makePathMatcherFilter(serviceInfo *sc.ServiceInfo) *hcmpb.HttpFilter {
	rules := []*pmpb.PathMatcherRule{}
	for _, operation := range serviceInfo.Operations {
		method := serviceInfo.Methods[operation]
		// Adds PathMatcherRule for gRPC method.
		if serviceInfo.BackendProtocol == util.GRPC {
			newGrpcRule := &pmpb.PathMatcherRule{
				Operation: operation,
				Pattern: &commonpb.Pattern{
					UriTemplate: fmt.Sprintf("/%s/%s", serviceInfo.ApiName, method.ShortName),
					HttpMethod:  util.POST,
				},
			}
			rules = append(rules, newGrpcRule)
		}

		// Adds PathMatcherRule for HTTP method, whose HttpRule is not empty.
		for _, httpRule := range method.HttpRule {
			if httpRule.UriTemplate != "" && httpRule.HttpMethod != "" {
				newHttpRule := &pmpb.PathMatcherRule{
					Operation: operation,
					Pattern:   httpRule,
				}
				if method.BackendRule.TranslationType == confpb.BackendRule_CONSTANT_ADDRESS && hasPathParameter(newHttpRule.Pattern.UriTemplate) {
					newHttpRule.ExtractPathParameters = true
				}
				rules = append(rules, newHttpRule)
			}
		}
	}

	if len(rules) == 0 {
		return nil
	}

	pathMathcherConfig := &pmpb.FilterConfig{Rules: rules}
	if len(serviceInfo.SegmentNames) > 0 {
		pathMathcherConfig.SegmentNames = serviceInfo.SegmentNames
	}

	pathMathcherConfigStruct, _ := util.MessageToStruct(pathMathcherConfig)
	pathMatcherFilter := &hcmpb.HttpFilter{
		Name:       util.PathMatcher,
		ConfigType: &hcmpb.HttpFilter_Config{pathMathcherConfigStruct},
	}
	return pathMatcherFilter
}

func hasPathParameter(httpPattern string) bool {
	return strings.ContainsRune(httpPattern, '{')
}

func makeJwtAuthnFilter(serviceInfo *sc.ServiceInfo) *hcmpb.HttpFilter {
	auth := serviceInfo.ServiceConfig().GetAuthentication()
	if len(auth.GetProviders()) == 0 {
		return nil
	}
	providers := make(map[string]*jwtpb.JwtProvider)
	for _, provider := range auth.GetProviders() {
		jp := &jwtpb.JwtProvider{
			Issuer: provider.GetIssuer(),
			JwksSourceSpecifier: &jwtpb.JwtProvider_RemoteJwks{
				RemoteJwks: &jwtpb.RemoteJwks{
					HttpUri: &corepb.HttpUri{
						Uri: provider.GetJwksUri(),
						HttpUpstreamType: &corepb.HttpUri_Cluster{
							Cluster: provider.GetIssuer(),
						},
					},
					CacheDuration: &durationpb.Duration{
						Seconds: int64(serviceInfo.Options.JwksCacheDurationInS),
					},
				},
			},
			FromHeaders: []*jwtpb.JwtHeader{
				{
					Name:        "Authorization",
					ValuePrefix: "Bearer ",
				},
				{
					Name: "X-Goog-Iap-Jwt-Assertion",
				},
			},
			FromParams: []string{
				"access_token",
			},
			ForwardPayloadHeader: "X-Endpoint-API-UserInfo",
		}
		if len(provider.GetAudiences()) != 0 {
			for _, a := range strings.Split(provider.GetAudiences(), ",") {
				jp.Audiences = append(jp.Audiences, strings.TrimSpace(a))
			}
		}
		// TODO(taoxuy): add unit test
		// the JWT Payload will be send to metadata by envoy and it will be used by service control filter
		// for logging and setting credential_id
		jp.PayloadInMetadata = util.JwtPayloadMetadataName
		providers[provider.GetId()] = jp
	}

	if len(providers) == 0 {
		return nil
	}

	requirements := make(map[string]*jwtpb.JwtRequirement)
	for _, rule := range auth.GetRules() {
		if len(rule.GetRequirements()) > 0 {
			requirements[rule.GetSelector()] = makeJwtRequirement(rule.GetRequirements())
		}
	}

	jwtAuthentication := &jwtpb.JwtAuthentication{
		Providers: providers,
		FilterStateRules: &jwtpb.FilterStateRule{
			Name:     "envoy.filters.http.path_matcher.operation",
			Requires: requirements,
		},
	}

	jas, _ := util.MessageToStruct(jwtAuthentication)
	jwtAuthnFilter := &hcmpb.HttpFilter{
		Name:       util.JwtAuthn,
		ConfigType: &hcmpb.HttpFilter_Config{jas},
	}
	return jwtAuthnFilter
}

func makeJwtRequirement(requirements []*confpb.AuthRequirement) *jwtpb.JwtRequirement {
	// By default, if there are multi requirements, treat it as RequireAny.
	requires := &jwtpb.JwtRequirement{
		RequiresType: &jwtpb.JwtRequirement_RequiresAny{
			RequiresAny: &jwtpb.JwtRequirementOrList{},
		},
	}

	for _, r := range requirements {
		var require *jwtpb.JwtRequirement
		if r.GetAudiences() == "" {
			require = &jwtpb.JwtRequirement{
				RequiresType: &jwtpb.JwtRequirement_ProviderName{
					ProviderName: r.GetProviderId(),
				},
			}
		} else {
			var audiences []string
			for _, a := range strings.Split(r.GetAudiences(), ",") {
				audiences = append(audiences, strings.TrimSpace(a))
			}
			require = &jwtpb.JwtRequirement{
				RequiresType: &jwtpb.JwtRequirement_ProviderAndAudiences{
					ProviderAndAudiences: &jwtpb.ProviderWithAudiences{
						ProviderName: r.GetProviderId(),
						Audiences:    audiences,
					},
				},
			}
		}
		if len(requirements) == 1 {
			requires = require
		} else {
			requires.GetRequiresAny().Requirements = append(requires.GetRequiresAny().GetRequirements(), require)
		}
	}

	return requires
}

func makeServiceControlCallingConfig(opts options.ConfigGeneratorOptions) *scpb.ServiceControlCallingConfig {
	setting := &scpb.ServiceControlCallingConfig{}
	setting.NetworkFailOpen = &wrapperspb.BoolValue{Value: opts.ServiceControlNetworkFailOpen}

	if opts.ScCheckTimeoutMs > 0 {
		setting.CheckTimeoutMs = &wrapperspb.UInt32Value{Value: uint32(opts.ScCheckTimeoutMs)}
	}
	if opts.ScQuotaTimeoutMs > 0 {
		setting.QuotaTimeoutMs = &wrapperspb.UInt32Value{Value: uint32(opts.ScQuotaTimeoutMs)}
	}
	if opts.ScReportTimeoutMs > 0 {
		setting.ReportTimeoutMs = &wrapperspb.UInt32Value{Value: uint32(opts.ScReportTimeoutMs)}
	}

	if opts.ScCheckRetries > -1 {
		setting.CheckRetries = &wrapperspb.UInt32Value{Value: uint32(opts.ScCheckRetries)}
	}
	if opts.ScQuotaRetries > -1 {
		setting.QuotaRetries = &wrapperspb.UInt32Value{Value: uint32(opts.ScQuotaRetries)}
	}
	if opts.ScReportRetries > -1 {
		setting.ReportRetries = &wrapperspb.UInt32Value{Value: uint32(opts.ScReportRetries)}
	}
	return setting
}

func makeServiceControlFilter(serviceInfo *sc.ServiceInfo) *hcmpb.HttpFilter {
	if serviceInfo == nil || serviceInfo.ServiceConfig().GetControl().GetEnvironment() == "" {
		return nil
	}

	lowercaseProtocol := strings.ToLower(serviceInfo.Options.BackendProtocol)
	serviceName := serviceInfo.ServiceConfig().GetName()
	service := &scpb.Service{
		ServiceName:       serviceName,
		ServiceConfigId:   serviceInfo.ConfigID,
		ProducerProjectId: serviceInfo.ServiceConfig().GetProducerProjectId(),
		ServiceConfig:     copyServiceConfigForReportMetrics(serviceInfo.ServiceConfig()),
		BackendProtocol:   lowercaseProtocol,
	}

	if serviceInfo.Options.LogRequestHeaders != "" {
		service.LogRequestHeaders = strings.Split(serviceInfo.Options.LogRequestHeaders, ",")
		for i := range service.LogRequestHeaders {
			service.LogRequestHeaders[i] = strings.TrimSpace(service.LogRequestHeaders[i])
		}
	}
	if serviceInfo.Options.LogResponseHeaders != "" {
		service.LogResponseHeaders = strings.Split(serviceInfo.Options.LogResponseHeaders, ",")
		for i := range service.LogResponseHeaders {
			service.LogResponseHeaders[i] = strings.TrimSpace(service.LogResponseHeaders[i])
		}
	}
	if serviceInfo.Options.LogJwtPayloads != "" {
		service.LogJwtPayloads = strings.Split(serviceInfo.Options.LogJwtPayloads, ",")
		for i := range service.LogJwtPayloads {
			service.LogJwtPayloads[i] = strings.TrimSpace(service.LogJwtPayloads[i])
		}
	}
	if serviceInfo.Options.MinStreamReportIntervalMs != 0 {
		service.MinStreamReportIntervalMs = serviceInfo.Options.MinStreamReportIntervalMs
	}
	service.JwtPayloadMetadataName = util.JwtPayloadMetadataName

	filterConfig := &scpb.FilterConfig{
		Services:        []*scpb.Service{service},
		AccessToken:     serviceInfo.AccessToken,
		ScCallingConfig: makeServiceControlCallingConfig(serviceInfo.Options),
		ServiceControlUri: &commonpb.HttpUri{
			Uri:     serviceInfo.ServiceControlURI,
			Cluster: util.ServiceControlClusterName,
			Timeout: &durationpb.Duration{Seconds: 5},
		},
	}

	if serviceInfo.GcpAttributes != nil {
		filterConfig.GcpAttributes = serviceInfo.GcpAttributes
	}

	for _, operation := range serviceInfo.Operations {
		method := serviceInfo.Methods[operation]
		requirement := &scpb.Requirement{
			ServiceName:        serviceName,
			OperationName:      operation,
			SkipServiceControl: method.SkipServiceControl,
			MetricCosts:        method.MetricCosts,
		}

		// For these OPTIONS methods, auth should be disabled and AllowWithoutApiKey
		// should be true for each CORS.
		if method.IsGeneratedOption || method.AllowUnregisteredCalls {
			requirement.ApiKey = &scpb.APIKeyRequirement{
				AllowWithoutApiKey: true,
			}
		}

		if method.APIKeyLocations != nil {
			if requirement.ApiKey == nil {
				requirement.ApiKey = &scpb.APIKeyRequirement{}
			}
			requirement.ApiKey.Locations = method.APIKeyLocations
		}

		filterConfig.Requirements = append(filterConfig.Requirements, requirement)
	}

	scs, err := util.MessageToStruct(filterConfig)
	if err != nil {
		glog.Warningf("failed to convert message to struct: %v", err)
	}
	filter := &hcmpb.HttpFilter{
		Name:       util.ServiceControl,
		ConfigType: &hcmpb.HttpFilter_Config{scs},
	}
	return filter
}

func copyServiceConfigForReportMetrics(src *confpb.Service) *anypb.Any {
	// Logs and metrics fields are needed by the Envoy HTTP filter
	// to generate proper Metrics for Report calls.
	copy := &confpb.Service{
		Logs:               src.GetLogs(),
		Metrics:            src.GetMetrics(),
		MonitoredResources: src.GetMonitoredResources(),
		Monitoring:         src.GetMonitoring(),
		Logging:            src.GetLogging(),
	}
	a, err := ptypes.MarshalAny(copy)
	if err != nil {
		glog.Warningf("failed to copy certain service config, error: %v", err)
		return nil
	}
	return a
}

func makeTranscoderFilter(serviceInfo *sc.ServiceInfo) *hcmpb.HttpFilter {
	for _, sourceFile := range serviceInfo.ServiceConfig().GetSourceInfo().GetSourceFiles() {
		configFile := &smpb.ConfigFile{}
		ptypes.UnmarshalAny(sourceFile, configFile)
		glog.Infof("got proto descriptor: %v", string(configFile.GetFileContents()))

		if configFile.GetFileType() == smpb.ConfigFile_FILE_DESCRIPTOR_SET_PROTO {
			configContent := configFile.GetFileContents()
			transcodeConfig := &transcoderpb.GrpcJsonTranscoder{
				DescriptorSet: &transcoderpb.GrpcJsonTranscoder_ProtoDescriptorBin{
					ProtoDescriptorBin: configContent,
				},
				Services:               []string{serviceInfo.ApiName},
				IgnoredQueryParameters: []string{"api_key", "key", "access_token"},
				ConvertGrpcStatus:      true,
			}
			transcodeConfigStruct, _ := util.MessageToStruct(transcodeConfig)
			transcodeFilter := &hcmpb.HttpFilter{
				Name:       util.GRPCJSONTranscoder,
				ConfigType: &hcmpb.HttpFilter_Config{transcodeConfigStruct},
			}
			return transcodeFilter
		}
	}
	return nil
}

func makeBackendAuthFilter(serviceInfo *sc.ServiceInfo) *hcmpb.HttpFilter {
	rules := []*bapb.BackendAuthRule{}
	for _, operation := range serviceInfo.Operations {
		method := serviceInfo.Methods[operation]
		if method.BackendRule.JwtAudience == "" {
			continue
		}
		rules = append(rules,
			&bapb.BackendAuthRule{
				Operation:   operation,
				JwtAudience: method.BackendRule.JwtAudience,
			})
	}
	backendAuthConfig := &bapb.FilterConfig{
		Rules: rules,
	}
	if serviceInfo.Options.IamServiceAccount != "" {
		backendAuthConfig.IdTokenInfo = &bapb.FilterConfig_IamToken{
			IamToken: &bapb.IamIdTokenInfo{
				IamUri: &commonpb.HttpUri{
					Uri:     fmt.Sprintf("%s%s", serviceInfo.Options.IamURL, util.IamIdentityTokenSuffix(serviceInfo.Options.IamServiceAccount)),
					Cluster: util.IamServerClusterName,
					// TODO(taoxuy): make token_subscriber use this timeout
					Timeout: &durationpb.Duration{Seconds: 5},
				},
				// Currently only support fetching access token from instance metadata
				// server, not by service account file.
				AccessToken:         serviceInfo.AccessToken,
				ServiceAccountEmail: serviceInfo.Options.IamServiceAccount,
			},
		}

	} else {
		backendAuthConfig.IdTokenInfo = &bapb.FilterConfig_ImdsToken{
			ImdsToken: &bapb.ImdsIdTokenInfo{
				ImdsServerUri: &commonpb.HttpUri{
					Uri:     fmt.Sprintf("%s%s", serviceInfo.Options.MetadataURL, util.IdentityTokenSuffix),
					Cluster: util.MetadataServerClusterName,
					// TODO(taoxuy): make token_subscriber use this timeout
					Timeout: &durationpb.Duration{Seconds: 5},
				},
			},
		}
	}
	backendAuthConfigStruct, _ := util.MessageToStruct(backendAuthConfig)
	backendAuthFilter := &hcmpb.HttpFilter{
		Name:       util.BackendAuth,
		ConfigType: &hcmpb.HttpFilter_Config{backendAuthConfigStruct},
	}
	return backendAuthFilter
}

func makeBackendRoutingFilter(serviceInfo *sc.ServiceInfo) *hcmpb.HttpFilter {
	rules := []*brpb.BackendRoutingRule{}
	for _, operation := range serviceInfo.Operations {
		method := serviceInfo.Methods[operation]
		if method.BackendRule.TranslationType != confpb.BackendRule_PATH_TRANSLATION_UNSPECIFIED {
			rules = append(rules, &brpb.BackendRoutingRule{
				Operation:      operation,
				IsConstAddress: method.BackendRule.TranslationType == confpb.BackendRule_CONSTANT_ADDRESS,
				PathPrefix:     method.BackendRule.Uri,
			})
		}
	}

	backendRoutingConfig := &brpb.FilterConfig{Rules: rules}
	backendRoutingConfigStruct, _ := util.MessageToStruct(backendRoutingConfig)
	backendRoutingFilter := &hcmpb.HttpFilter{
		Name:       util.BackendRouting,
		ConfigType: &hcmpb.HttpFilter_Config{backendRoutingConfigStruct},
	}
	return backendRoutingFilter
}

func makeRouterFilter(opts options.ConfigGeneratorOptions) *hcmpb.HttpFilter {
	router, _ := util.MessageToStruct(&routerpb.Router{
		SuppressEnvoyHeaders: opts.SuppressEnvoyHeaders,
		StartChildSpan:       opts.EnableTracing,
	})
	routerFilter := &hcmpb.HttpFilter{
		Name:       util.Router,
		ConfigType: &hcmpb.HttpFilter_Config{Config: router},
	}
	return routerFilter
}

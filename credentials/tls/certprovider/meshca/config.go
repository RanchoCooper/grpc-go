// +build go1.13

/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package meshca

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"path"
	"strings"
	"time"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/ptypes"

	"google.golang.org/grpc/credentials/sts"
	configpb "google.golang.org/grpc/credentials/tls/certprovider/meshca/internal/meshca_experimental"
)

const (
	// GKE metadata server endpoint.
	mdsBaseURI        = "http://metadata.google.internal/"
	mdsRequestTimeout = 5 * time.Second

	// The following are default values used in the interaction with MeshCA.
	defaultMeshCaEndpoint = "meshca.googleapis.com"
	defaultCallTimeout    = 10 * time.Second
	defaultCertLifetime   = 24 * time.Hour
	defaultCertGraceTime  = 12 * time.Hour
	defaultKeyTypeRSA     = "RSA"
	defaultKeySize        = 2048

	// The following are default values used in the interaction with STS or
	// Secure Token Service, which is used to exchange the JWT token for an
	// access token.
	defaultSTSEndpoint        = "securetoken.googleapis.com"
	defaultCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	defaultRequestedTokenType = "urn:ietf:params:oauth:token-type:access_token"
	defaultSubjectTokenType   = "urn:ietf:params:oauth:token-type:jwt"
)

// For overriding in unit tests.
var (
	makeHTTPDoer     = makeHTTPClient
	readZoneFunc     = readZone
	readAudienceFunc = readAudience
)

// Implements the certprovider.StableConfig interface.
type pluginConfig struct {
	serverURI     string
	stsOpts       sts.Options
	callTimeout   time.Duration
	certLifetime  time.Duration
	certGraceTime time.Duration
	keyType       string
	keySize       int
	location      string
}

// pluginConfigFromJSON parses the provided config in JSON.
//
// For certain values missing in the config, we use default values defined at
// the top of this file.
//
// If the location field or STS audience field is missing, we try talking to the
// GKE Metadata server and try to infer these values. If this attempt does not
// succeed, we let those fields have empty values.
func pluginConfigFromJSON(data json.RawMessage) (*pluginConfig, error) {
	cfgProto := &configpb.GoogleMeshCaConfig{}
	m := jsonpb.Unmarshaler{AllowUnknownFields: true}
	if err := m.Unmarshal(bytes.NewReader(data), cfgProto); err != nil {
		return nil, fmt.Errorf("meshca: failed to unmarshal config: %v", err)
	}

	if api := cfgProto.GetServer().GetApiType(); api != v3corepb.ApiConfigSource_GRPC {
		return nil, fmt.Errorf("meshca: server has apiType %s, want %s", api, v3corepb.ApiConfigSource_GRPC)
	}

	pc := &pluginConfig{}
	gs := cfgProto.GetServer().GetGrpcServices()
	if l := len(gs); l != 1 {
		return nil, fmt.Errorf("meshca: number of gRPC services in config is %d, expected 1", l)
	}
	grpcService := gs[0]
	googGRPC := grpcService.GetGoogleGrpc()
	if googGRPC == nil {
		return nil, errors.New("meshca: missing google gRPC service in config")
	}
	pc.serverURI = googGRPC.GetTargetUri()
	if pc.serverURI == "" {
		pc.serverURI = defaultMeshCaEndpoint
	}

	callCreds := googGRPC.GetCallCredentials()
	if len(callCreds) == 0 {
		return nil, errors.New("meshca: missing call credentials in config")
	}
	var stsCallCreds *v3corepb.GrpcService_GoogleGrpc_CallCredentials_StsService
	for _, cc := range callCreds {
		if stsCallCreds = cc.GetStsService(); stsCallCreds != nil {
			break
		}
	}
	if stsCallCreds == nil {
		return nil, errors.New("meshca: missing STS call credentials in config")
	}
	if stsCallCreds.GetSubjectTokenPath() == "" {
		return nil, errors.New("meshca: missing subjectTokenPath in STS call credentials config")
	}
	pc.stsOpts = makeStsOptsWithDefaults(stsCallCreds)

	var err error
	if pc.callTimeout, err = ptypes.Duration(grpcService.GetTimeout()); err != nil {
		pc.callTimeout = defaultCallTimeout
	}
	if pc.certLifetime, err = ptypes.Duration(cfgProto.GetCertificateLifetime()); err != nil {
		pc.certLifetime = defaultCertLifetime
	}
	if pc.certGraceTime, err = ptypes.Duration(cfgProto.GetRenewalGracePeriod()); err != nil {
		pc.certGraceTime = defaultCertGraceTime
	}
	switch cfgProto.GetKeyType() {
	case configpb.GoogleMeshCaConfig_KEY_TYPE_UNKNOWN, configpb.GoogleMeshCaConfig_KEY_TYPE_RSA:
		pc.keyType = defaultKeyTypeRSA
	default:
		return nil, fmt.Errorf("meshca: unsupported key type: %s, only support RSA keys", pc.keyType)
	}
	pc.keySize = int(cfgProto.GetKeySize())
	if pc.keySize == 0 {
		pc.keySize = defaultKeySize
	}
	pc.location = cfgProto.GetLocation()
	if pc.location == "" {
		pc.location = readZoneFunc(makeHTTPDoer())
	}

	return pc, nil
}

func (pc *pluginConfig) canonical() []byte {
	return []byte(fmt.Sprintf("%s:%s:%s:%s:%s:%s:%d:%s", pc.serverURI, pc.stsOpts, pc.callTimeout, pc.certLifetime, pc.certGraceTime, pc.keyType, pc.keySize, pc.location))
}

func makeStsOptsWithDefaults(stsCallCreds *v3corepb.GrpcService_GoogleGrpc_CallCredentials_StsService) sts.Options {
	opts := sts.Options{
		TokenExchangeServiceURI: stsCallCreds.GetTokenExchangeServiceUri(),
		Resource:                stsCallCreds.GetResource(),
		Audience:                stsCallCreds.GetAudience(),
		Scope:                   stsCallCreds.GetScope(),
		RequestedTokenType:      stsCallCreds.GetRequestedTokenType(),
		SubjectTokenPath:        stsCallCreds.GetSubjectTokenPath(),
		SubjectTokenType:        stsCallCreds.GetSubjectTokenType(),
		ActorTokenPath:          stsCallCreds.GetActorTokenPath(),
		ActorTokenType:          stsCallCreds.GetActorTokenType(),
	}

	// Use sane defaults for unspecified fields.
	if opts.TokenExchangeServiceURI == "" {
		opts.TokenExchangeServiceURI = defaultSTSEndpoint
	}
	if opts.Audience == "" {
		opts.Audience = readAudienceFunc(makeHTTPDoer())
	}
	if opts.Scope == "" {
		opts.Scope = defaultCloudPlatformScope
	}
	if opts.RequestedTokenType == "" {
		opts.RequestedTokenType = defaultRequestedTokenType
	}
	if opts.SubjectTokenType == "" {
		opts.SubjectTokenType = defaultSubjectTokenType
	}
	return opts
}

// httpDoer wraps the single method on the http.Client type that we use. This
// helps with overriding in unit tests.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

func makeHTTPClient() httpDoer {
	return &http.Client{Timeout: mdsRequestTimeout}
}

func readMetadata(client httpDoer, uriPath string) (string, error) {
	req, err := http.NewRequest("GET", mdsBaseURI+uriPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		dump, err := httputil.DumpRequestOut(req, false)
		if err != nil {
			logger.Warningf("Failed to dump HTTP request: %v", err)
		}
		logger.Warningf("Request %q returned status %v", dump, resp.StatusCode)
	}
	return string(body), err
}

func readZone(client httpDoer) string {
	zoneURI := "computeMetadata/v1/instance/zone"
	data, err := readMetadata(client, zoneURI)
	if err != nil {
		logger.Warningf("GET %s failed: %v", path.Join(mdsBaseURI, zoneURI), err)
		return ""
	}

	// The output returned by the metadata server looks like this:
	// projects/<PROJECT-NUMBER>/zones/<ZONE>
	parts := strings.Split(data, "/")
	if len(parts) == 0 {
		logger.Warningf("GET %s returned {%s}, does not match expected format {projects/<PROJECT-NUMBER>/zones/<ZONE>}", path.Join(mdsBaseURI, zoneURI))
		return ""
	}
	return parts[len(parts)-1]
}

// readAudience constructs the audience field to be used in the STS request, if
// it is not specified in the plugin configuration.
//
// "identitynamespace:{TRUST_DOMAIN}:{GKE_CLUSTER_URL}" is the format of the
// audience field. When workload identity is enabled on a GCP project, a default
// trust domain is created whose value is "{PROJECT_ID}.svc.id.goog". The format
// of the GKE_CLUSTER_URL is:
// https://container.googleapis.com/v1/projects/{PROJECT_ID}/zones/{ZONE}/clusters/{CLUSTER_NAME}.
func readAudience(client httpDoer) string {
	projURI := "computeMetadata/v1/project/project-id"
	project, err := readMetadata(client, projURI)
	if err != nil {
		logger.Warningf("GET %s failed: %v", path.Join(mdsBaseURI, projURI), err)
		return ""
	}
	trustDomain := fmt.Sprintf("%s.svc.id.goog", project)

	clusterURI := "computeMetadata/v1/instance/attributes/cluster-name"
	cluster, err := readMetadata(client, clusterURI)
	if err != nil {
		logger.Warningf("GET %s failed: %v", path.Join(mdsBaseURI, clusterURI), err)
		return ""
	}
	zone := readZoneFunc(client)
	clusterURL := fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/zones/%s/clusters/%s", project, zone, cluster)
	audience := fmt.Sprintf("identitynamespace:%s:%s", trustDomain, clusterURL)
	return audience
}

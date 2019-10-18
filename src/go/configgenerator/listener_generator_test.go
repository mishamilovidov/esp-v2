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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cloudesf.googlesource.com/gcpproxy/src/go/configinfo"
	"cloudesf.googlesource.com/gcpproxy/src/go/options"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/ptypes"

	anypb "github.com/golang/protobuf/ptypes/any"
	annotationspb "google.golang.org/genproto/googleapis/api/annotations"
	confpb "google.golang.org/genproto/googleapis/api/serviceconfig"
	smpb "google.golang.org/genproto/googleapis/api/servicemanagement/v1"
	apipb "google.golang.org/genproto/protobuf/api"
	ptypepb "google.golang.org/genproto/protobuf/ptype"
)

var (
	fakeProtoDescriptor = base64.StdEncoding.EncodeToString([]byte("rawDescriptor"))

	sourceFile = &smpb.ConfigFile{
		FilePath:     "api_descriptor.pb",
		FileContents: []byte("rawDescriptor"),
		FileType:     smpb.ConfigFile_FILE_DESCRIPTOR_SET_PROTO,
	}
	content, _ = ptypes.MarshalAny(sourceFile)
)

func TestTranscoderFilter(t *testing.T) {
	testData := []struct {
		desc                 string
		fakeServiceConfig    *confpb.Service
		wantTranscoderFilter string
	}{
		{
			desc: "Success for gRPC backend with transcoding",
			fakeServiceConfig: &confpb.Service{
				Name: testProjectName,
				Apis: []*apipb.Api{
					{
						Name: testApiName,
					},
				},
				SourceInfo: &confpb.SourceInfo{
					SourceFiles: []*anypb.Any{content},
				},
			},
			wantTranscoderFilter: fmt.Sprintf(`
        {
          "config":{
            "convert_grpc_status":true,
            "ignored_query_parameters": [
                "api_key",
                "key",
                "access_token"
              ],
            "proto_descriptor_bin":"%s",
            "services":[
              "%s"
            ]
          },
          "name":"envoy.grpc_json_transcoder"
        }
      `, fakeProtoDescriptor, testApiName),
		},
	}

	for i, tc := range testData {
		opts := options.DefaultConfigGeneratorOptions()
		opts.BackendProtocol = "gRPC"
		fakeServiceInfo, err := configinfo.NewServiceInfoFromServiceConfig(tc.fakeServiceConfig, testConfigID, opts)
		if err != nil {
			t.Fatal(err)
		}

		marshaler := &jsonpb.Marshaler{}
		gotFilter, err := marshaler.MarshalToString(makeTranscoderFilter(fakeServiceInfo))

		// Normalize both path matcher filter and gotListeners.
		gotFilter = normalizeJson(gotFilter)
		want := normalizeJson(tc.wantTranscoderFilter)
		if !strings.Contains(gotFilter, want) {
			t.Errorf("Test Desc(%d): %s, makeTranscoderFilter failed, got: %s, want: %s", i, tc.desc, gotFilter, want)
		}
	}
}

func TestBackendAuthFilter(t *testing.T) {
	testdata := []struct {
		desc                  string
		iamServiceAccount     string
		fakeServiceConfig     *confpb.Service
		wantBackendAuthFilter string
	}{
		{
			desc: "Success, generate backend auth filter in general",
			fakeServiceConfig: &confpb.Service{
				Name: testProjectName,
				Apis: []*apipb.Api{
					{
						Name: "testapi",
						Methods: []*apipb.Method{
							{
								Name: "foo",
							},
							{
								Name: "bar",
							},
						},
					},
				},
				Backend: &confpb.Backend{
					Rules: []*confpb.BackendRule{
						{
							Selector: "ignore_me",
						},
						{
							Selector:        "testapipb.foo",
							Address:         "https://testapipb.com/foo",
							PathTranslation: confpb.BackendRule_CONSTANT_ADDRESS,
							Authentication: &confpb.BackendRule_JwtAudience{
								JwtAudience: "foo.com",
							},
						},
						{
							Selector:        "testapipb.bar",
							Address:         "https://testapipb.com/foo",
							PathTranslation: confpb.BackendRule_CONSTANT_ADDRESS,
							Authentication: &confpb.BackendRule_JwtAudience{
								JwtAudience: "bar.com",
							},
						},
					},
				},
			},
			wantBackendAuthFilter: `{
        "config": {
          "rules": [
            {
              "jwt_audience": "bar.com",
              "operation": "testapipb.bar"
            },
            {
              "jwt_audience": "foo.com",
              "operation": "testapipb.foo"
            }
          ],
          "imds_token": {
            "imds_server_uri": {
              "cluster": "metadata-cluster",
              "uri": "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/identity",
              "timeout":"5s"
            }
          }
        },
      "name": "envoy.filters.http.backend_auth"
    }`,
		},
		{
			desc:              "Success, set iamIdToken when iam service account is set",
			iamServiceAccount: "service-account@google.com",
			fakeServiceConfig: &confpb.Service{
				Name: testProjectName,
				Apis: []*apipb.Api{
					{
						Name: "testapi",
					},
				},
				Backend: &confpb.Backend{
					Rules: []*confpb.BackendRule{
						{
							Selector:        "testapipb.bar",
							Address:         "https://testapipb.com/foo",
							PathTranslation: confpb.BackendRule_CONSTANT_ADDRESS,
							Authentication: &confpb.BackendRule_JwtAudience{
								JwtAudience: "bar.com",
							},
						},
					},
				},
			},
			wantBackendAuthFilter: `{
        "config": {
          "rules": [
            {
              "jwt_audience": "bar.com",
              "operation": "testapipb.bar"
            }
          ],
          "iam_token": {
            "iam_uri": {
              "cluster": "iam-cluster",
              "uri": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/service-account@google.com:generateIdToken",
              "timeout":"5s"
            },
            "access_token": {
              "remote_token":{
               "cluster": "metadata-cluster",
               "uri": "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token",
               "timeout":"5s"
             }
            },
            "service_account_email": "service-account@google.com"
          }
        },
      "name": "envoy.filters.http.backend_auth"
    }`,
		},
	}

	for _, tc := range testdata {

		opts := options.DefaultConfigGeneratorOptions()
		opts.BackendProtocol = "http2"
		opts.EnableBackendRouting = true
		opts.IamServiceAccount = tc.iamServiceAccount
		fakeServiceInfo, err := configinfo.NewServiceInfoFromServiceConfig(tc.fakeServiceConfig, testConfigID, opts)
		if err != nil {
			t.Fatal(err)
		}

		marshaler := &jsonpb.Marshaler{}
		gotFilter, err := marshaler.MarshalToString(makeBackendAuthFilter(fakeServiceInfo))
		gotFilter = normalizeJson(gotFilter)
		want := normalizeJson(tc.wantBackendAuthFilter)

		if !strings.Contains(gotFilter, want) {
			t.Errorf("makeBackendAuthFilter failed,\ngot: %s, \nwant: %s", gotFilter, want)
		}
	}
}

func TestPathMatcherFilter(t *testing.T) {
	testData := []struct {
		desc                  string
		fakeServiceConfig     *confpb.Service
		backendProtocol       string
		wantPathMatcherFilter string
	}{
		{
			desc: "Path Matcher filter - gRPC backend",
			fakeServiceConfig: &confpb.Service{
				Name: testProjectName,
				Apis: []*apipb.Api{
					{
						Name: testApiName,
						Methods: []*apipb.Method{
							{
								Name: "ListShelves",
							},
							{
								Name: "CreateShelf",
							},
						},
					},
				},
			},
			backendProtocol: "GRPC",
			wantPathMatcherFilter: `
			        {
			          "config": {
			            "rules": [
			              {
			                "operation": "endpoints.examples.bookstore.Bookstore.CreateShelf",
			                "pattern": {
			                  "http_method": "POST",
			                  "uri_template": "/endpoints.examples.bookstore.Bookstore/CreateShelf"
			                }
			              },
			              {
			                "operation": "endpoints.examples.bookstore.Bookstore.ListShelves",
			                "pattern": {
			                  "http_method": "POST",
			                  "uri_template": "/endpoints.examples.bookstore.Bookstore/ListShelves"
			                }
			              }
			            ]
			          },
			          "name": "envoy.filters.http.path_matcher"
			        }
			      `,
		},
		{
			desc: "Path Matcher filter - HTTP backend",
			fakeServiceConfig: &confpb.Service{
				Name: testProjectName,
				Apis: []*apipb.Api{
					{
						Name: "1.echo_api_endpoints_cloudesf_testing_cloud_goog",
						Methods: []*apipb.Method{
							{
								Name: "Echo_Auth_Jwt",
							},
							{
								Name: "Echo",
							},
						},
					},
				},
				Http: &annotationspb.Http{
					Rules: []*annotationspb.HttpRule{
						{
							Selector: "1.echo_api_endpoints_cloudesf_testing_cloud_goog.Echo_Auth_Jwt",
							Pattern: &annotationspb.HttpRule_Get{
								Get: "/auth/info/googlejwt",
							},
						},
						{
							Selector: "1.echo_api_endpoints_cloudesf_testing_cloud_goog.Echo",
							Pattern: &annotationspb.HttpRule_Post{
								Post: "/echo",
							},
							Body: "message",
						},
					},
				},
			},
			backendProtocol: "HTTP1",
			wantPathMatcherFilter: `
			        {
			          "config": {
			            "rules": [
			              {
			                "operation": "1.echo_api_endpoints_cloudesf_testing_cloud_goog.Echo",
			                "pattern": {
			                  "http_method": "POST",
			                  "uri_template": "/echo"
			                }
			              },
			              {
			                "operation": "1.echo_api_endpoints_cloudesf_testing_cloud_goog.Echo_Auth_Jwt",
			                "pattern": {
			                  "http_method": "GET",
			                  "uri_template": "/auth/info/googlejwt"
			                }
			              }
			            ]
			          },
			          "name": "envoy.filters.http.path_matcher"
			        }
			      `,
		},
		{
			desc: "Path Matcher filter - HTTP backend with path parameters",
			fakeServiceConfig: &confpb.Service{
				Name: "foo.endpoints.bar.cloud.goog",
				Apis: []*apipb.Api{
					{
						Name: "1.cloudesf_testing_cloud_goog",
						Methods: []*apipb.Method{
							{
								Name: "Foo",
							},
							{
								Name: "Bar",
							},
						},
					},
				},
				Backend: &confpb.Backend{
					Rules: []*confpb.BackendRule{
						{
							Address:         "https://mybackend.com",
							Selector:        "1.cloudesf_testing_cloud_goog.Foo",
							PathTranslation: confpb.BackendRule_CONSTANT_ADDRESS,
							Authentication: &confpb.BackendRule_JwtAudience{
								JwtAudience: "mybackend.com",
							},
						},
						{
							Address:         "https://mybackend.com",
							Selector:        "1.cloudesf_testing_cloud_goog.Bar",
							PathTranslation: confpb.BackendRule_APPEND_PATH_TO_ADDRESS,
							Authentication: &confpb.BackendRule_JwtAudience{
								JwtAudience: "mybackend.com",
							},
						},
					},
				},
				Http: &annotationspb.Http{
					Rules: []*annotationspb.HttpRule{
						{
							Selector: "1.cloudesf_testing_cloud_goog.Foo",
							Pattern: &annotationspb.HttpRule_Get{
								Get: "foo/{id}",
							},
						},
						{
							Selector: "1.cloudesf_testing_cloud_goog.Bar",
							Pattern: &annotationspb.HttpRule_Get{
								Get: "foo",
							},
						},
					},
				},
			},
			backendProtocol: "HTTP1",
			wantPathMatcherFilter: `
			        {
			          "config": {
			            "rules": [
			              {
			                "operation": "1.cloudesf_testing_cloud_goog.Bar",
			                "pattern": {
			                  "http_method": "GET",
			                  "uri_template": "foo"
			                }
			              },
			              {
			                "extract_path_parameters": true,
			                "operation": "1.cloudesf_testing_cloud_goog.Foo",
			                "pattern": {
			                  "http_method": "GET",
			                  "uri_template": "foo/{id}"
			                }
			              }
			            ]
			          },
			          "name": "envoy.filters.http.path_matcher"
			        }
			      `,
		},
		{
			desc: "Path Matcher filter - CORS support",
			fakeServiceConfig: &confpb.Service{
				Name: "foo.endpoints.bar.cloud.goog",
				Apis: []*apipb.Api{
					{
						Name: "1.cloudesf_testing_cloud_goog",
						Methods: []*apipb.Method{
							{
								Name: "Foo",
							},
						},
					},
				},
				Endpoints: []*confpb.Endpoint{
					{
						Name:      "foo.endpoints.bar.cloud.goog",
						AllowCors: true,
					},
				},
				Http: &annotationspb.Http{
					Rules: []*annotationspb.HttpRule{
						{
							Selector: "1.cloudesf_testing_cloud_goog.Foo",
							Pattern: &annotationspb.HttpRule_Get{
								Get: "foo",
							},
						},
					},
				},
			},
			backendProtocol: "HTTP1",
			wantPathMatcherFilter: `
			        {
			         "config": {
			            "rules": [
			              {
			                "operation": "1.cloudesf_testing_cloud_goog.CORS_0",
			                "pattern": {
			                  "http_method": "OPTIONS",
			                  "uri_template": "foo"
			                }
			              },
			              {
			                "operation": "1.cloudesf_testing_cloud_goog.Foo",
			                "pattern": {
			                  "http_method": "GET",
			                  "uri_template": "foo"
			                }
			              }
			            ]
			          },
			          "name": "envoy.filters.http.path_matcher"
			        }
			      `,
		},
		{
			desc: "Path Matcher filter - Segment Name Mapping for snake-case field",
			fakeServiceConfig: &confpb.Service{
				Name: "foo.endpoints.bar.cloud.goog",
				Apis: []*apipb.Api{
					{
						Name: "1.cloudesf_testing_cloud_goog",
						Methods: []*apipb.Method{
							{
								Name: "Foo",
							},
						},
					},
				},
				Types: []*ptypepb.Type{
					{
						Fields: []*ptypepb.Field{
							&ptypepb.Field{
								JsonName: "fooBar",
								Name:     "foo_bar",
							},
						},
					},
				},
				Backend: &confpb.Backend{
					Rules: []*confpb.BackendRule{
						{
							Address:         "https://mybackend.com",
							Selector:        "1.cloudesf_testing_cloud_goog.Foo",
							PathTranslation: confpb.BackendRule_CONSTANT_ADDRESS,
							Authentication: &confpb.BackendRule_JwtAudience{
								JwtAudience: "mybackend.com",
							},
						},
					},
				},
				Http: &annotationspb.Http{
					Rules: []*annotationspb.HttpRule{
						{
							Selector: "1.cloudesf_testing_cloud_goog.Foo",
							Pattern: &annotationspb.HttpRule_Get{
								Get: "foo/{foo_bar}",
							},
						},
					},
				},
			},
			backendProtocol: "http1",
			wantPathMatcherFilter: `
			        {
			          "config": {
			          "segment_names": [
			            {
			              "json_name": "fooBar",
			              "snake_name": "foo_bar"
			            }
			          ],
			          "rules": [
			            {
			              "extract_path_parameters": true,
			              "operation": "1.cloudesf_testing_cloud_goog.Foo",
			              "pattern": {
			                "http_method": "GET",
			                "uri_template": "foo/{foo_bar}"
			              }
			            }
			          ]
			        },
			        "name": "envoy.filters.http.path_matcher"
			      }`,
		},
	}

	for i, tc := range testData {
		opts := options.DefaultConfigGeneratorOptions()
		opts.BackendProtocol = tc.backendProtocol
		opts.EnableBackendRouting = true
		fakeServiceInfo, err := configinfo.NewServiceInfoFromServiceConfig(tc.fakeServiceConfig, testConfigID, opts)
		if err != nil {
			t.Fatal(err)
		}
		marshaler := &jsonpb.Marshaler{}
		gotFilter, err := marshaler.MarshalToString(makePathMatcherFilter(fakeServiceInfo))

		// Normalize both path matcher filter and gotListeners.
		gotFilter = normalizeJson(gotFilter)
		want := normalizeJson(tc.wantPathMatcherFilter)
		if !strings.Contains(gotFilter, want) {
			t.Errorf("Test Desc(%d): %s, makePathMatcherFilter failed, got: %s, want: %s", i, tc.desc, gotFilter, want)
		}
	}
}

func normalizeJson(input string) string {
	var jsonObject map[string]interface{}
	json.Unmarshal([]byte(input), &jsonObject)
	outputString, _ := json.Marshal(jsonObject)
	return string(outputString)
}

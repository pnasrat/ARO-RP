package frontend

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/sirupsen/logrus"

	"github.com/Azure/ARO-RP/pkg/api"
	"github.com/Azure/ARO-RP/pkg/api/v20191231preview"
	"github.com/Azure/ARO-RP/pkg/database"
	"github.com/Azure/ARO-RP/pkg/database/cosmosdb"
	"github.com/Azure/ARO-RP/pkg/env"
	"github.com/Azure/ARO-RP/pkg/metrics/noop"
	"github.com/Azure/ARO-RP/pkg/util/clientauthorizer"
	mock_database "github.com/Azure/ARO-RP/pkg/util/mocks/database"
	utiltls "github.com/Azure/ARO-RP/pkg/util/tls"
	"github.com/Azure/ARO-RP/test/util/listener"
)

func TestPostOpenShiftClusterCredentials(t *testing.T) {
	ctx := context.Background()

	clientkey, clientcerts, err := utiltls.GenerateKeyAndCertificate("client", nil, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}

	serverkey, servercerts, err := utiltls.GenerateKeyAndCertificate("server", nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(servercerts[0])

	cli := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
				Certificates: []tls.Certificate{
					{
						Certificate: [][]byte{clientcerts[0].Raw},
						PrivateKey:  clientkey,
					},
				},
			},
		},
	}

	mockSubID := "00000000-0000-0000-0000-000000000000"

	type test struct {
		name           string
		resourceID     string
		mocks          func(*test, *mock_database.MockOpenShiftClusters)
		wantStatusCode int
		wantResponse   func(*test) *v20191231preview.OpenShiftClusterCredentials
		wantError      string
	}

	for _, tt := range []*test{
		{
			name:       "cluster exists in db",
			resourceID: fmt.Sprintf("/subscriptions/%s/resourcegroups/resourceGroup/providers/Microsoft.RedHatOpenShift/openShiftClusters/resourceName", mockSubID),
			mocks: func(tt *test, openshiftClusters *mock_database.MockOpenShiftClusters) {
				openshiftClusters.EXPECT().
					Get(gomock.Any(), strings.ToLower(tt.resourceID)).
					Return(&api.OpenShiftClusterDocument{
						OpenShiftCluster: &api.OpenShiftCluster{
							ID:   tt.resourceID,
							Name: "resourceName",
							Type: "Microsoft.RedHatOpenShift/openshiftClusters",
							Properties: api.Properties{
								ProvisioningState: api.ProvisioningStateSucceeded,
								ServicePrincipalProfile: api.ServicePrincipalProfile{
									ClientSecret: "clientSecret",
								},
								KubeadminPassword: "password",
							},
						},
					}, nil)
			},
			wantStatusCode: http.StatusOK,
			wantResponse: func(tt *test) *v20191231preview.OpenShiftClusterCredentials {
				return &v20191231preview.OpenShiftClusterCredentials{
					KubeadminUsername: "kubeadmin",
					KubeadminPassword: "password",
				}
			},
		},
		{
			name:       "cluster exists in db in creating state",
			resourceID: fmt.Sprintf("/subscriptions/%s/resourcegroups/resourceGroup/providers/Microsoft.RedHatOpenShift/openShiftClusters/resourceName", mockSubID),
			mocks: func(tt *test, openshiftClusters *mock_database.MockOpenShiftClusters) {
				openshiftClusters.EXPECT().
					Get(gomock.Any(), strings.ToLower(tt.resourceID)).
					Return(&api.OpenShiftClusterDocument{
						OpenShiftCluster: &api.OpenShiftCluster{
							ID:   tt.resourceID,
							Name: "resourceName",
							Type: "Microsoft.RedHatOpenShift/openshiftClusters",
							Properties: api.Properties{
								ProvisioningState: api.ProvisioningStateCreating,
								ServicePrincipalProfile: api.ServicePrincipalProfile{
									ClientSecret: "clientSecret",
								},
							},
						},
					}, nil)
			},
			wantStatusCode: http.StatusBadRequest,
			wantError:      `400: RequestNotAllowed: : Request is not allowed in provisioningState 'Creating'.`,
		},
		{
			name:       "cluster not found in db",
			resourceID: fmt.Sprintf("/subscriptions/%s/resourcegroups/resourceGroup/providers/Microsoft.RedHatOpenShift/openShiftClusters/resourceName", mockSubID),
			mocks: func(tt *test, openshiftClusters *mock_database.MockOpenShiftClusters) {
				openshiftClusters.EXPECT().
					Get(gomock.Any(), strings.ToLower(tt.resourceID)).
					Return(nil, &cosmosdb.Error{StatusCode: http.StatusNotFound})
			},
			wantStatusCode: http.StatusNotFound,
			wantError:      `404: ResourceNotFound: : The Resource 'openshiftclusters/resourcename' under resource group 'resourcegroup' was not found.`,
		},
		{
			name:       "internal error",
			resourceID: fmt.Sprintf("/subscriptions/%s/resourcegroups/resourceGroup/providers/Microsoft.RedHatOpenShift/openShiftClusters/resourceName", mockSubID),
			mocks: func(tt *test, openshiftClusters *mock_database.MockOpenShiftClusters) {
				openshiftClusters.EXPECT().
					Get(gomock.Any(), strings.ToLower(tt.resourceID)).
					Return(nil, errors.New("random error"))
			},
			wantStatusCode: http.StatusInternalServerError,
			wantError:      `500: InternalServerError: : Internal server error.`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cli.CloseIdleConnections()

			l := listener.NewListener()
			defer l.Close()

			env := &env.Test{
				L:        l,
				TLSKey:   serverkey,
				TLSCerts: servercerts,
			}
			env.SetClientAuthorizer(clientauthorizer.NewOne(clientcerts[0].Raw))

			cli.Transport.(*http.Transport).Dial = l.Dial

			controller := gomock.NewController(t)
			defer controller.Finish()

			openshiftClusters := mock_database.NewMockOpenShiftClusters(controller)
			subscriptions := mock_database.NewMockSubscriptions(controller)

			subscriptions.EXPECT().
				Get(gomock.Any(), mockSubID).
				Return(&api.SubscriptionDocument{
					Subscription: &api.Subscription{
						State: api.SubscriptionStateRegistered,
						Properties: &api.SubscriptionProperties{
							TenantID: "11111111-1111-1111-1111-111111111111",
						},
					},
				}, nil)

			tt.mocks(tt, openshiftClusters)

			f, err := NewFrontend(ctx, logrus.NewEntry(logrus.StandardLogger()), env, &database.Database{
				OpenShiftClusters: openshiftClusters,
				Subscriptions:     subscriptions,
			}, api.APIs, &noop.Noop{})
			if err != nil {
				t.Fatal(err)
			}

			go f.Run(ctx, nil, nil)

			req, err := http.NewRequest(http.MethodPost, "https://server"+tt.resourceID+"/listcredentials?api-version=2019-12-31-preview", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := cli.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatusCode {
				t.Error(resp.StatusCode)
			}

			if tt.wantError == "" {
				var oc *v20191231preview.OpenShiftClusterCredentials
				err = json.NewDecoder(resp.Body).Decode(&oc)
				if err != nil {
					t.Fatal(err)
				}

				if !reflect.DeepEqual(oc, tt.wantResponse(tt)) {
					b, _ := json.Marshal(oc)
					t.Error(string(b))
				}

			} else {
				cloudErr := &api.CloudError{StatusCode: resp.StatusCode}
				err = json.NewDecoder(resp.Body).Decode(&cloudErr)
				if err != nil {
					t.Fatal(err)
				}

				if cloudErr.Error() != tt.wantError {
					t.Error(cloudErr)
				}
			}
		})
	}
}

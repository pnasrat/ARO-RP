package deploy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/authorization/mgmt/2015-07-01/authorization"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-03-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/dns/mgmt/2018-05-01/dns"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2019-07-01/network"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-05-01/resources"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/openshift/installer/pkg/asset/ignition/machine"
	"github.com/openshift/installer/pkg/asset/installconfig"
	"github.com/openshift/installer/pkg/asset/machines"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/jim-minter/rp/pkg/api"
)

func (d *Deployer) deployResources(ctx context.Context, doc *api.OpenShiftClusterDocument) error {
	g, err := d.getGraph(ctx, doc)
	if err != nil {
		return err
	}

	clusterID := g[reflect.TypeOf(&installconfig.ClusterID{})].(*installconfig.ClusterID)
	installConfig := g[reflect.TypeOf(&installconfig.InstallConfig{})].(*installconfig.InstallConfig)
	machinesMaster := g[reflect.TypeOf(&machines.Master{})].(*machines.Master)
	machineMaster := g[reflect.TypeOf(&machine.Master{})].(*machine.Master)

	masterSubnetCIDR, err := cidr.Subnet(&installConfig.Config.Networking.MachineCIDR.IPNet, 3, 0)
	if err != nil {
		return err
	}

	workerSubnetCIDR, err := cidr.Subnet(&installConfig.Config.Networking.MachineCIDR.IPNet, 3, 1)
	if err != nil {
		return err
	}

	var lbIP net.IP
	{
		_, last := cidr.AddressRange(masterSubnetCIDR)
		lbIP = cidr.Dec(cidr.Dec(last))
	}

	srvRecords := make([]dns.SrvRecord, len(machinesMaster.MachineFiles))
	for i := 0; i < len(machinesMaster.MachineFiles); i++ {
		srvRecords[i] = dns.SrvRecord{
			Priority: to.Int32Ptr(10),
			Weight:   to.Int32Ptr(10),
			Port:     to.Int32Ptr(2380),
			Target:   to.StringPtr(fmt.Sprintf("etcd-%d.%s", i, installConfig.Config.ObjectMeta.Name+"."+installConfig.Config.BaseDomain)),
		}
	}

	{
		t := &Template{
			Schema:         "https://schema.management.azure.com/schemas/2015-01-01/deploymentTemplate.json#",
			ContentVersion: "1.0.0.0",
			Parameters: map[string]Parameter{
				"sas": {
					Type: "object",
				},
			},
			Resources: []Resource{
				{
					Resource: &authorization.RoleAssignment{
						Name: to.StringPtr("[guid(resourceId('Microsoft.ManagedIdentity/userAssignedIdentities', '" + clusterID.InfraID + "-identity'), 'contributor')]"),
						Type: to.StringPtr("Microsoft.Authorization/roleAssignments"),
						Properties: &authorization.RoleAssignmentPropertiesWithScope{
							Scope:            to.StringPtr("[resourceGroup().id]"),
							RoleDefinitionID: to.StringPtr("[resourceId('Microsoft.Authorization/roleDefinitions', 'b24988ac-6180-42a0-ab88-20f7382dd24c')]"), // Contributor
							PrincipalID:      to.StringPtr("[reference(resourceId('Microsoft.ManagedIdentity/userAssignedIdentities', '" + clusterID.InfraID + "-identity'), '2018-11-30').principalId]"),
						},
					},
					APIVersion: apiVersions["authorization"],
				},
				{
					Resource: &network.SecurityGroup{
						SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
							SecurityRules: &[]network.SecurityRule{
								{
									SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
										Protocol:                 network.SecurityRuleProtocolTCP,
										SourcePortRange:          to.StringPtr("*"),
										DestinationPortRange:     to.StringPtr("6443"),
										SourceAddressPrefix:      to.StringPtr("*"),
										DestinationAddressPrefix: to.StringPtr("*"),
										Access:                   network.SecurityRuleAccessAllow,
										Priority:                 to.Int32Ptr(101),
										Direction:                network.SecurityRuleDirectionInbound,
									},
									Name: to.StringPtr("apiserver_in"),
								},
								{
									SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
										Protocol:                 network.SecurityRuleProtocolTCP,
										SourcePortRange:          to.StringPtr("*"),
										DestinationPortRange:     to.StringPtr("22"),
										SourceAddressPrefix:      to.StringPtr("*"),
										DestinationAddressPrefix: to.StringPtr("*"),
										Access:                   network.SecurityRuleAccessAllow,
										Priority:                 to.Int32Ptr(103),
										Direction:                network.SecurityRuleDirectionInbound,
									},
									Name: to.StringPtr("bootstrap_ssh_in"),
								},
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-controlplane-nsg"),
						Type:     to.StringPtr("Microsoft.Network/networkSecurityGroups"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
				},
				{
					Resource: &network.SecurityGroup{
						Name:     to.StringPtr(clusterID.InfraID + "-node-nsg"),
						Type:     to.StringPtr("Microsoft.Network/networkSecurityGroups"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
				},
				{
					Resource: &network.VirtualNetwork{
						VirtualNetworkPropertiesFormat: &network.VirtualNetworkPropertiesFormat{
							AddressSpace: &network.AddressSpace{
								AddressPrefixes: &[]string{
									installConfig.Config.Networking.MachineCIDR.String(),
								},
							},
							Subnets: &[]network.Subnet{
								{
									SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
										AddressPrefix: to.StringPtr(masterSubnetCIDR.String()),
										NetworkSecurityGroup: &network.SecurityGroup{
											ID: to.StringPtr("[resourceId('Microsoft.Network/networkSecurityGroups', '" + clusterID.InfraID + "-controlplane-nsg')]"),
										},
									},
									Name: to.StringPtr(clusterID.InfraID + "-master-subnet"),
								},
								{
									SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
										AddressPrefix: to.StringPtr(workerSubnetCIDR.String()),
										NetworkSecurityGroup: &network.SecurityGroup{
											ID: to.StringPtr("[resourceId('Microsoft.Network/networkSecurityGroups', '" + clusterID.InfraID + "-node-nsg')]"),
										},
									},
									Name: to.StringPtr(clusterID.InfraID + "-worker-subnet"),
								},
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-vnet"),
						Type:     to.StringPtr("Microsoft.Network/virtualNetworks"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
					DependsOn: []string{
						"Microsoft.Network/networkSecurityGroups/" + clusterID.InfraID + "-controlplane-nsg",
						"Microsoft.Network/networkSecurityGroups/" + clusterID.InfraID + "-node-nsg",
					},
				},
				{
					Resource: &dns.Zone{
						ZoneProperties: &dns.ZoneProperties{
							ZoneType: dns.Private,
							ResolutionVirtualNetworks: &[]dns.SubResource{
								{
									ID: to.StringPtr("[resourceId('Microsoft.Network/virtualNetworks', '" + clusterID.InfraID + "-vnet')]"),
								},
							},
						},
						Name:     to.StringPtr(installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain),
						Type:     to.StringPtr("Microsoft.Network/dnszones"),
						Location: to.StringPtr("global"),
					},
					APIVersion: apiVersions["dns"],
					DependsOn: []string{
						"Microsoft.Network/virtualNetworks/" + clusterID.InfraID + "-vnet",
					},
				},
				{
					Resource: &dns.RecordSet{
						Name: to.StringPtr(installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain + "/api-int"),
						Type: to.StringPtr("Microsoft.Network/dnszones/a"),
						RecordSetProperties: &dns.RecordSetProperties{
							TTL: to.Int64Ptr(300),
							ARecords: &[]dns.ARecord{
								{
									Ipv4Address: to.StringPtr(lbIP.String()),
								},
							},
						},
					},
					APIVersion: apiVersions["dns"],
					DependsOn: []string{
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
					},
				},
				{
					Resource: &dns.RecordSet{
						Name: to.StringPtr(installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain + "/api"),
						Type: to.StringPtr("Microsoft.Network/dnszones/a"),
						RecordSetProperties: &dns.RecordSetProperties{
							TTL: to.Int64Ptr(300),
							ARecords: &[]dns.ARecord{
								{
									Ipv4Address: to.StringPtr(lbIP.String()),
								},
							},
						},
					},
					APIVersion: apiVersions["dns"],
					DependsOn: []string{
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
					},
				},
				{
					Resource: &dns.RecordSet{
						Name: to.StringPtr(installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain + "/_etcd-server-ssl._tcp"),
						Type: to.StringPtr("Microsoft.Network/dnszones/srv"),
						RecordSetProperties: &dns.RecordSetProperties{
							TTL:        to.Int64Ptr(60),
							SrvRecords: &srvRecords,
						},
					},
					APIVersion: apiVersions["dns"],
					DependsOn: []string{
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
					},
				},
				{
					Resource: &dns.RecordSet{
						Name: to.StringPtr("[concat('" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain + "/etcd-', copyIndex())]"),
						Type: to.StringPtr("Microsoft.Network/dnszones/a"),
						RecordSetProperties: &dns.RecordSetProperties{
							TTL: to.Int64Ptr(60),
							ARecords: &[]dns.ARecord{
								{
									Ipv4Address: to.StringPtr("[reference(resourceId('Microsoft.Network/networkInterfaces', concat('" + clusterID.InfraID + "-master', copyIndex(), '-nic')), '2019-07-01').ipConfigurations[0].properties.privateIPAddress]"),
								},
							},
						},
					},
					APIVersion: apiVersions["dns"],
					Copy: &Copy{
						Name:  "copy",
						Count: len(machinesMaster.MachineFiles),
					},
					DependsOn: []string{
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
						"[concat('Microsoft.Network/networkInterfaces/" + clusterID.InfraID + "-master', copyIndex(), '-nic')]",
					},
				},
				{
					Resource: &network.RouteTable{
						Name:     to.StringPtr(clusterID.InfraID + "-node-routetable"),
						Type:     to.StringPtr("Microsoft.Network/routeTables"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
				},
				{
					Resource: &network.PublicIPAddress{
						Sku: &network.PublicIPAddressSku{
							Name: network.PublicIPAddressSkuNameStandard,
						},
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							PublicIPAllocationMethod: network.Static,
						},
						Name:     to.StringPtr(clusterID.InfraID + "-bootstrap-pip"),
						Type:     to.StringPtr("Microsoft.Network/publicIPAddresses"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
				},
				{
					Resource: &network.PublicIPAddress{
						Sku: &network.PublicIPAddressSku{
							Name: network.PublicIPAddressSkuNameStandard,
						},
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							PublicIPAllocationMethod: network.Static,
							DNSSettings: &network.PublicIPAddressDNSSettings{
								DomainNameLabel: &clusterID.InfraID,
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-pip"),
						Type:     to.StringPtr("Microsoft.Network/publicIPAddresses"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
				},
				{
					Resource: &network.LoadBalancer{
						Sku: &network.LoadBalancerSku{
							Name: network.LoadBalancerSkuNameStandard,
						},
						LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
							FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
								{
									FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
										PublicIPAddress: &network.PublicIPAddress{
											ID: to.StringPtr("[resourceId('Microsoft.Network/publicIPAddresses', '" + clusterID.InfraID + "-pip')]"),
										},
									},
									Name: to.StringPtr("public-lb-ip"),
								},
							},
							BackendAddressPools: &[]network.BackendAddressPool{
								{
									Name: to.StringPtr(clusterID.InfraID + "-public-lb-control-plane"),
								},
							},
							LoadBalancingRules: &[]network.LoadBalancingRule{
								{
									LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
										FrontendIPConfiguration: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', '" + clusterID.InfraID + "-public-lb', 'public-lb-ip')]"),
										},
										BackendAddressPool: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-public-lb', '" + clusterID.InfraID + "-public-lb-control-plane')]"),
										},
										Probe: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/probes', '" + clusterID.InfraID + "-public-lb', 'api-internal-probe')]"),
										},
										Protocol:             network.TransportProtocolTCP,
										LoadDistribution:     network.LoadDistributionDefault,
										FrontendPort:         to.Int32Ptr(6443),
										BackendPort:          to.Int32Ptr(6443),
										IdleTimeoutInMinutes: to.Int32Ptr(30),
									},
									Name: to.StringPtr("api-internal"),
								},
							},
							Probes: &[]network.Probe{
								{
									ProbePropertiesFormat: &network.ProbePropertiesFormat{
										Protocol:          network.ProbeProtocolTCP,
										Port:              to.Int32Ptr(6443),
										IntervalInSeconds: to.Int32Ptr(10),
										NumberOfProbes:    to.Int32Ptr(3),
									},
									Name: to.StringPtr("api-internal-probe"),
									Type: to.StringPtr("Microsoft.Network/loadBalancers/probes"),
								},
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-public-lb"),
						Type:     to.StringPtr("Microsoft.Network/loadBalancers"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
					DependsOn: []string{
						"Microsoft.Network/publicIPAddresses/" + clusterID.InfraID + "-pip",
					},
				},
				{
					Resource: &network.LoadBalancer{
						Sku: &network.LoadBalancerSku{
							Name: network.LoadBalancerSkuNameStandard,
						},
						LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
							FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
								{
									FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
										PrivateIPAddress:          to.StringPtr(lbIP.String()),
										PrivateIPAllocationMethod: network.Static,
										Subnet: &network.Subnet{
											ID: to.StringPtr("[resourceId('Microsoft.Network/virtualNetworks/subnets', '" + clusterID.InfraID + "-vnet', '" + clusterID.InfraID + "-master-subnet')]"),
										},
									},
									Name: to.StringPtr("internal-lb-ip"),
								},
							},
							BackendAddressPools: &[]network.BackendAddressPool{
								{
									Name: to.StringPtr(clusterID.InfraID + "-internal-controlplane"),
								},
							},
							LoadBalancingRules: &[]network.LoadBalancingRule{
								{
									LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
										FrontendIPConfiguration: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', '" + clusterID.InfraID + "-internal-lb', 'internal-lb-ip')]"),
										},
										BackendAddressPool: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-internal-lb', '" + clusterID.InfraID + "-internal-controlplane')]"),
										},
										Probe: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/probes', '" + clusterID.InfraID + "-internal-lb', 'api-internal-probe')]"),
										},
										Protocol:             network.TransportProtocolTCP,
										LoadDistribution:     network.LoadDistributionDefault,
										FrontendPort:         to.Int32Ptr(6443),
										BackendPort:          to.Int32Ptr(6443),
										IdleTimeoutInMinutes: to.Int32Ptr(30),
									},
									Name: to.StringPtr("api-internal"),
								},
								{
									LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
										FrontendIPConfiguration: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', '" + clusterID.InfraID + "-internal-lb', 'internal-lb-ip')]"),
										},
										BackendAddressPool: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-internal-lb', '" + clusterID.InfraID + "-internal-controlplane')]"),
										},
										Probe: &network.SubResource{
											ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/probes', '" + clusterID.InfraID + "-internal-lb', 'sint-probe')]"),
										},
										Protocol:             network.TransportProtocolTCP,
										LoadDistribution:     network.LoadDistributionDefault,
										FrontendPort:         to.Int32Ptr(22623),
										BackendPort:          to.Int32Ptr(22623),
										IdleTimeoutInMinutes: to.Int32Ptr(30),
									},
									Name: to.StringPtr("sint"),
								},
							},
							Probes: &[]network.Probe{
								{
									ProbePropertiesFormat: &network.ProbePropertiesFormat{
										Protocol:          network.ProbeProtocolTCP,
										Port:              to.Int32Ptr(6443),
										IntervalInSeconds: to.Int32Ptr(10),
										NumberOfProbes:    to.Int32Ptr(3),
									},
									Name: to.StringPtr("api-internal-probe"),
								},
								{
									ProbePropertiesFormat: &network.ProbePropertiesFormat{
										Protocol:          network.ProbeProtocolTCP,
										Port:              to.Int32Ptr(22623),
										IntervalInSeconds: to.Int32Ptr(10),
										NumberOfProbes:    to.Int32Ptr(3),
									},
									Name: to.StringPtr("sint-probe"),
								},
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-internal-lb"),
						Type:     to.StringPtr("Microsoft.Network/loadBalancers"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
					DependsOn: []string{
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
					},
				},
				{
					Resource: &network.Interface{
						InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
							IPConfigurations: &[]network.InterfaceIPConfiguration{
								{
									InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
										LoadBalancerBackendAddressPools: &[]network.BackendAddressPool{
											{
												ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-public-lb', '" + clusterID.InfraID + "-public-lb-control-plane')]"),
											},
											{
												ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-internal-lb', '" + clusterID.InfraID + "-internal-controlplane')]"),
											},
										},
										Subnet: &network.Subnet{
											ID: to.StringPtr("[resourceId('Microsoft.Network/virtualNetworks/subnets', '" + clusterID.InfraID + "-vnet', '" + clusterID.InfraID + "-master-subnet')]"),
										},
										PublicIPAddress: &network.PublicIPAddress{
											ID: to.StringPtr("[resourceId('Microsoft.Network/publicIPAddresses', '" + clusterID.InfraID + "-bootstrap-pip')]"),
										},
									},
									Name: to.StringPtr("bootstrap-nic-ip"),
								},
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-bootstrap-nic"),
						Type:     to.StringPtr("Microsoft.Network/networkInterfaces"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
					DependsOn: []string{
						"Microsoft.Network/publicIPAddresses/" + clusterID.InfraID + "-bootstrap-pip",
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
						"Microsoft.Network/loadBalancers/" + clusterID.InfraID + "-internal-lb",
						"Microsoft.Network/loadBalancers/" + clusterID.InfraID + "-public-lb",
					},
				},
				{
					Resource: &network.Interface{
						InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
							IPConfigurations: &[]network.InterfaceIPConfiguration{
								{
									InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
										LoadBalancerBackendAddressPools: &[]network.BackendAddressPool{
											{
												ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-public-lb', '" + clusterID.InfraID + "-public-lb-control-plane')]"),
											},
											{
												ID: to.StringPtr("[resourceId('Microsoft.Network/loadBalancers/backendAddressPools', '" + clusterID.InfraID + "-internal-lb', '" + clusterID.InfraID + "-internal-controlplane')]"),
											},
										},
										Subnet: &network.Subnet{
											ID: to.StringPtr("[resourceId('Microsoft.Network/virtualNetworks/subnets', '" + clusterID.InfraID + "-vnet', '" + clusterID.InfraID + "-master-subnet')]"),
										},
									},
									Name: to.StringPtr("pipConfig"),
								},
							},
						},
						Name:     to.StringPtr("[concat('" + clusterID.InfraID + "-master', copyIndex(), '-nic')]"),
						Type:     to.StringPtr("Microsoft.Network/networkInterfaces"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["network"],
					Copy: &Copy{
						Name:  "copy",
						Count: len(machinesMaster.MachineFiles),
					},
					DependsOn: []string{
						"Microsoft.Network/dnszones/" + installConfig.Config.ObjectMeta.Name + "." + installConfig.Config.BaseDomain,
						"Microsoft.Network/loadBalancers/" + clusterID.InfraID + "-internal-lb",
						"Microsoft.Network/loadBalancers/" + clusterID.InfraID + "-public-lb",
					},
				},
				{
					Resource: &compute.Image{
						ImageProperties: &compute.ImageProperties{
							StorageProfile: &compute.ImageStorageProfile{
								OsDisk: &compute.ImageOSDisk{
									OsType:  compute.Linux,
									BlobURI: to.StringPtr("https://cluster" + doc.OpenShiftCluster.Properties.StorageSuffix + ".blob.core.windows.net/vhd/rhcos" + doc.OpenShiftCluster.Properties.StorageSuffix + ".vhd"),
								},
							},
							HyperVGeneration: compute.HyperVGenerationTypesV1,
						},
						Name:     &clusterID.InfraID,
						Type:     to.StringPtr("Microsoft.Compute/images"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["compute"],
				},
				{
					Resource: &compute.VirtualMachine{
						VirtualMachineProperties: &compute.VirtualMachineProperties{
							HardwareProfile: &compute.HardwareProfile{
								VMSize: compute.VirtualMachineSizeTypesStandardD4sV3,
							},
							StorageProfile: &compute.StorageProfile{
								ImageReference: &compute.ImageReference{
									ID: to.StringPtr("[resourceId('Microsoft.Compute/images', '" + clusterID.InfraID + "')]"),
								},
								OsDisk: &compute.OSDisk{
									Name:         to.StringPtr(clusterID.InfraID + "-bootstrap_OSDisk"),
									Caching:      compute.CachingTypesReadWrite,
									CreateOption: compute.DiskCreateOptionTypesFromImage,
									DiskSizeGB:   to.Int32Ptr(100),
									ManagedDisk: &compute.ManagedDiskParameters{
										StorageAccountType: compute.StorageAccountTypesPremiumLRS,
									},
								},
							},
							OsProfile: &compute.OSProfile{
								ComputerName:  to.StringPtr(clusterID.InfraID + "-bootstrap-vm"),
								AdminUsername: to.StringPtr("core"),
								AdminPassword: to.StringPtr("NotActuallyApplied!"),
								CustomData:    to.StringPtr(`[base64(concat('{"ignition":{"version":"2.2.0","config":{"replace":{"source":"https://cluster` + doc.OpenShiftCluster.Properties.StorageSuffix + `.blob.core.windows.net/ignition/bootstrap.ign?', listAccountSas(resourceId('Microsoft.Storage/storageAccounts', 'cluster` + doc.OpenShiftCluster.Properties.StorageSuffix + `'), '2019-04-01', parameters('sas')).accountSasToken, '"}}}}'))]`),
								LinuxConfiguration: &compute.LinuxConfiguration{
									DisablePasswordAuthentication: to.BoolPtr(false),
								},
							},
							NetworkProfile: &compute.NetworkProfile{
								NetworkInterfaces: &[]compute.NetworkInterfaceReference{
									{
										ID: to.StringPtr("[resourceId('Microsoft.Network/networkInterfaces', '" + clusterID.InfraID + "-bootstrap-nic')]"),
									},
								},
							},
							DiagnosticsProfile: &compute.DiagnosticsProfile{
								BootDiagnostics: &compute.BootDiagnostics{
									Enabled:    to.BoolPtr(true),
									StorageURI: to.StringPtr("https://cluster" + doc.OpenShiftCluster.Properties.StorageSuffix + ".blob.core.windows.net/"),
								},
							},
						},
						Identity: &compute.VirtualMachineIdentity{
							Type: compute.ResourceIdentityTypeUserAssigned,
							UserAssignedIdentities: map[string]*compute.VirtualMachineIdentityUserAssignedIdentitiesValue{
								"[resourceId('Microsoft.ManagedIdentity/userAssignedIdentities', '" + clusterID.InfraID + "-identity')]": &compute.VirtualMachineIdentityUserAssignedIdentitiesValue{},
							},
						},
						Name:     to.StringPtr(clusterID.InfraID + "-bootstrap"),
						Type:     to.StringPtr("Microsoft.Compute/virtualMachines"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["compute"],
					DependsOn: []string{
						"Microsoft.Compute/images/" + clusterID.InfraID,
						"Microsoft.Network/networkInterfaces/" + clusterID.InfraID + "-bootstrap-nic",
					},
				},
				{
					Resource: &compute.VirtualMachine{
						VirtualMachineProperties: &compute.VirtualMachineProperties{
							HardwareProfile: &compute.HardwareProfile{
								VMSize: compute.VirtualMachineSizeTypes(installConfig.Config.ControlPlane.Platform.Azure.InstanceType),
							},
							StorageProfile: &compute.StorageProfile{
								ImageReference: &compute.ImageReference{
									ID: to.StringPtr("[resourceId('Microsoft.Compute/images', '" + clusterID.InfraID + "')]"),
								},
								OsDisk: &compute.OSDisk{
									Name:         to.StringPtr("[concat('" + clusterID.InfraID + "-master-', copyIndex(), '_OSDisk')]"),
									Caching:      compute.CachingTypesReadOnly,
									CreateOption: compute.DiskCreateOptionTypesFromImage,
									DiskSizeGB:   &installConfig.Config.ControlPlane.Platform.Azure.OSDisk.DiskSizeGB,
									ManagedDisk: &compute.ManagedDiskParameters{
										StorageAccountType: compute.StorageAccountTypesPremiumLRS,
									},
								},
							},
							OsProfile: &compute.OSProfile{
								ComputerName:  to.StringPtr("[concat('" + clusterID.InfraID + "-master-', copyIndex())]"),
								AdminUsername: to.StringPtr("core"),
								AdminPassword: to.StringPtr("NotActuallyApplied!"),
								CustomData:    to.StringPtr(base64.StdEncoding.EncodeToString(machineMaster.File.Data)),
								LinuxConfiguration: &compute.LinuxConfiguration{
									DisablePasswordAuthentication: to.BoolPtr(false),
								},
							},
							NetworkProfile: &compute.NetworkProfile{
								NetworkInterfaces: &[]compute.NetworkInterfaceReference{
									{
										ID: to.StringPtr("[resourceId('Microsoft.Network/networkInterfaces', concat('" + clusterID.InfraID + "-master', copyIndex(), '-nic'))]"),
									},
								},
							},
							DiagnosticsProfile: &compute.DiagnosticsProfile{
								BootDiagnostics: &compute.BootDiagnostics{
									Enabled:    to.BoolPtr(true),
									StorageURI: to.StringPtr("https://cluster" + doc.OpenShiftCluster.Properties.StorageSuffix + ".blob.core.windows.net/"),
								},
							},
						},
						Identity: &compute.VirtualMachineIdentity{
							Type: compute.ResourceIdentityTypeUserAssigned,
							UserAssignedIdentities: map[string]*compute.VirtualMachineIdentityUserAssignedIdentitiesValue{
								"[resourceId('Microsoft.ManagedIdentity/userAssignedIdentities', '" + clusterID.InfraID + "-identity')]": &compute.VirtualMachineIdentityUserAssignedIdentitiesValue{},
							},
						},
						Zones: &[]string{
							"[copyIndex(1)]",
						},
						Name:     to.StringPtr("[concat('" + clusterID.InfraID + "-master-', copyIndex())]"),
						Type:     to.StringPtr("Microsoft.Compute/virtualMachines"),
						Location: &installConfig.Config.Azure.Region,
					},
					APIVersion: apiVersions["compute"],
					Copy: &Copy{
						Name:  "copy",
						Count: len(machinesMaster.MachineFiles),
					},
					DependsOn: []string{
						"Microsoft.Compute/images/" + clusterID.InfraID,
						"[concat('Microsoft.Network/networkInterfaces/" + clusterID.InfraID + "-master', copyIndex(), '-nic')]",
					},
				},
			},
		}

		d.log.Print("deploying resources template")
		future, err := d.deployments.CreateOrUpdate(ctx, doc.OpenShiftCluster.Properties.ResourceGroup, "azuredeploy", resources.Deployment{
			Properties: &resources.DeploymentProperties{
				Template: t,
				Parameters: map[string]interface{}{
					"sas": map[string]interface{}{
						"value": map[string]interface{}{
							"signedStart":         doc.OpenShiftCluster.Properties.Installation.Now.UTC().Format(time.RFC3339),
							"signedExpiry":        doc.OpenShiftCluster.Properties.Installation.Now.Add(24 * time.Hour).Format(time.RFC3339),
							"signedPermission":    "rl",
							"signedResourceTypes": "o",
							"signedServices":      "b",
							"signedProtocol":      "https",
						},
					},
				},
				Mode: resources.Incremental,
			},
		})
		if err != nil {
			return err
		}

		d.log.Print("waiting for resources template deployment")
		err = future.WaitForCompletionRef(ctx, d.deployments.Client)
		if err != nil {
			return err
		}
	}

	{
		_, err = d.recordsets.CreateOrUpdate(ctx, installConfig.Config.Azure.BaseDomainResourceGroupName, installConfig.Config.BaseDomain, "api."+installConfig.Config.ObjectMeta.Name, dns.CNAME, dns.RecordSet{
			RecordSetProperties: &dns.RecordSetProperties{
				TTL: to.Int64Ptr(300),
				CnameRecord: &dns.CnameRecord{
					Cname: to.StringPtr(clusterID.InfraID + "." + installConfig.Config.Azure.Region + ".cloudapp.azure.com"),
				},
			},
		}, "", "")
		if err != nil {
			return err
		}
	}

	{
		restConfig, err := restConfig(doc.OpenShiftCluster.Properties.AdminKubeconfig)
		if err != nil {
			return err
		}

		cli, err := corev1client.NewForConfig(restConfig)
		if err != nil {
			return err
		}

		d.log.Print("waiting for bootstrap configmap")
		now := time.Now()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			cm, err := cli.ConfigMaps("kube-system").Get("bootstrap", metav1.GetOptions{})
			if err == nil && cm.Data["status"] == "complete" {
				break
			}

			if time.Now().Sub(now) > 30*time.Minute {
				return fmt.Errorf("timed out waiting for bootstrap configmap")
			}

			<-t.C
		}
	}

	return nil
}
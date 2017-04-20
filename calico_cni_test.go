package main_test

import (
	"fmt"
	"log"
	"math/rand"
	"os"

	"net"

	"syscall"

	"github.com/containernetworking/cni/pkg/ns"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	. "github.com/projectcalico/cni-plugin/test_utils"
	"github.com/projectcalico/libcalico-go/lib/api"
	"github.com/projectcalico/libcalico-go/lib/client"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/vishvananda/netlink"
)

// Some ideas for more tests
// Test that both etcd_endpoints and etcd_authity can be used
// Test k8s
// test bad network name
// badly formatted netconf
// vary the MTU
// Existing endpoint

var calicoClient *client.Client

func init() {
	var err error
	calicoClient, err = client.NewFromEnv()
	if err != nil {
		panic(err)
	}
}

var _ = Describe("CalicoCni", func() {
	hostname, _ := os.Hostname()
	BeforeEach(func() {
		WipeEtcd()
	})

	cniVersion := os.Getenv("CNI_SPEC_VERSION")

	Describe("Run Calico CNI plugin", func() {
		Context("using host-local IPAM", func() {

			netconf := fmt.Sprintf(`
            {
              "cniVersion": "%s",
              "name": "net1",
              "type": "calico",
              "etcd_endpoints": "http://%s:2379",
              "ipam": {
                "type": "host-local",
                "subnet": "10.0.0.0/8"
              }
            }`, cniVersion, os.Getenv("ETCD_IP"))

			It("successfully networks the namespace", func() {
				containerID, netnspath, session, contVeth, contAddresses, contRoutes, err := CreateContainer(netconf, "", "")
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit())

				result, err := GetResultForCurrent(session, cniVersion)
				if err != nil {
					log.Fatalf("Error getting result from the session: %v\n", err)
				}

				mac := contVeth.Attrs().HardwareAddr

				Expect(len(result.IPs)).Should(Equal(1))
				ip := result.IPs[0].Address.IP.String()
				result.IPs[0].Address.IP = result.IPs[0].Address.IP.To4() // Make sure the IP is respresented as 4 bytes
				Expect(result.IPs[0].Address.Mask.String()).Should(Equal("ffffffff"))

				// datastore things:
				// Profile is created with correct details
				profile, err := calicoClient.Profiles().Get(api.ProfileMetadata{Name: "net1"})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(profile.Metadata.Tags).Should(ConsistOf("net1"))
				Expect(profile.Spec.EgressRules).Should(Equal([]api.Rule{{Action: "allow"}}))
				Expect(profile.Spec.IngressRules).Should(Equal([]api.Rule{{Action: "allow", Source: api.EntityRule{Tag: "net1"}}}))

				// The endpoint is created in etcd
				endpoints, err := calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             hostname,
					Name:             "eth0",
					Workload:         containerID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

				Expect(endpoints.Items[0].Spec).Should(Equal(api.WorkloadEndpointSpec{
					InterfaceName: fmt.Sprintf("cali%s", containerID),
					IPNetworks:    []cnet.IPNet{{result.IPs[0].Address}},
					MAC:           &cnet.MAC{HardwareAddr: mac},
					Profiles:      []string{"net1"},
				}))

				// Routes and interface on host - there's is nothing to assert on the routes since felix adds those.
				//fmt.Println(Cmd("ip link show")) // Useful for debugging
				hostVeth, err := netlink.LinkByName("cali" + containerID)
				Expect(err).ToNot(HaveOccurred())
				Expect(hostVeth.Attrs().Flags.String()).Should(ContainSubstring("up"))
				Expect(hostVeth.Attrs().MTU).Should(Equal(1500))

				// Routes and interface in netns
				Expect(contVeth.Attrs().Flags.String()).Should(ContainSubstring("up"))

				// Assume the first IP is the IPv4 address
				Expect(contAddresses[0].IP.String()).Should(Equal(ip))
				Expect(contRoutes).Should(SatisfyAll(ContainElement(netlink.Route{
					LinkIndex: contVeth.Attrs().Index,
					Gw:        net.IPv4(169, 254, 1, 1).To4(),
					Protocol:  syscall.RTPROT_BOOT,
					Table:     syscall.RT_TABLE_MAIN,
					Type:      syscall.RTN_UNICAST,
				}),
					ContainElement(netlink.Route{
						LinkIndex: contVeth.Attrs().Index,
						Scope:     netlink.SCOPE_LINK,
						Dst:       &net.IPNet{IP: net.IPv4(169, 254, 1, 1).To4(), Mask: net.CIDRMask(32, 32)},
						Protocol:  syscall.RTPROT_BOOT,
						Table:     syscall.RT_TABLE_MAIN,
						Type:      syscall.RTN_UNICAST,
					})))

				_, err = DeleteContainer(netconf, netnspath, "")
				Expect(err).ShouldNot(HaveOccurred())

				// Make sure there are no endpoints anymore
				endpoints, err = calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(0))

				// Make sure the interface has been removed from the namespace
				targetNs, _ := ns.GetNS(netnspath)
				err = targetNs.Do(func(_ ns.NetNS) error {
					_, err = netlink.LinkByName("eth0")
					return err
				})
				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(Equal("Link not found"))

				// Make sure the interface has been removed from the host
				_, err = netlink.LinkByName("cali" + containerID)
				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(Equal("Link not found"))

			})

			Context("when the same hostVeth exists", func() {
				It("successfully networks the namespace", func() {
					container_id := fmt.Sprintf("con%d", rand.Uint32())
					if err := CreateHostVeth(container_id, "", ""); err != nil {
						panic(err)
					}
					_, netnspath, session, _, _, _, err := CreateContainerWithId(netconf, "", "", container_id)
					Expect(err).ShouldNot(HaveOccurred())
					Eventually(session).Should(gexec.Exit(0))

					_, err = DeleteContainerWithId(netconf, netnspath, "", container_id)
					Expect(err).ShouldNot(HaveOccurred())
				})
			})
		})
	})

	Describe("Run Calico CNI plugin", func() {
		Context("depricate Hostname for nodename", func() {
			netconf := fmt.Sprintf(`
            {
              "cniVersion": "%s",
              "name": "net1",
              "type": "calico",
              "etcd_endpoints": "http://%s:2379",
              "hostname": "namedHostname",
              "ipam": {
                "type": "host-local",
                "subnet": "10.0.0.0/8"
              }
            }`, cniVersion, os.Getenv("ETCD_IP"))

			It("has hostname even though deprecated", func() {
				containerID, netnspath, session, _, _, _, err := CreateContainer(netconf, "", "")
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit())

				result, err := GetResultForCurrent(session, cniVersion)
				if err != nil {
					log.Fatalf("Error getting result from the session: %v\n", err)
				}

				log.Printf("Unmarshaled result: %v\n", result)

				// The endpoint is created in etcd
				endpoints, err := calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             "namedHostname",
					Name:             "eth0",
					Workload:         containerID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

				_, err = DeleteContainer(netconf, netnspath, "")
				Expect(err).ShouldNot(HaveOccurred())
			})
		})
	})

	Describe("Run Calico CNI plugin", func() {
		Context("depricate Hostname for nodename", func() {
			netconf := fmt.Sprintf(`
            {
              "cniVersion": "%s",
              "name": "net1",
              "type": "calico",
              "etcd_endpoints": "http://%s:2379",
              "hostname": "namedHostname",
              "nodename": "namedNodename",
              "ipam": {
                "type": "host-local",
                "subnet": "10.0.0.0/8"
              }
            }`, cniVersion, os.Getenv("ETCD_IP"))

			It("nodename takes precedence over hostname", func() {
				containerID, netnspath, session, _, _, _, err := CreateContainer(netconf, "", "")
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit())

				result, err := GetResultForCurrent(session, cniVersion)
				if err != nil {
					log.Fatalf("Error getting result from the session: %v\n", err)
				}

				log.Printf("Unmarshaled result: %v\n", result)

				// The endpoint is created in etcd
				endpoints, err := calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             "namedNodename",
					Name:             "eth0",
					Workload:         containerID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

				_, err = DeleteContainer(netconf, netnspath, "")
				Expect(err).ShouldNot(HaveOccurred())
			})
		})
	})

	Describe("DEL", func() {
		netconf := fmt.Sprintf(`
        {
            "cniVersion": "%s",
            "name": "net1",
            "type": "calico",
            "etcd_endpoints": "http://%s:2379",
            "ipam": {
                "type": "host-local",
                "subnet": "10.0.0.0/8"
            }
        }`, cniVersion, os.Getenv("ETCD_IP"))

		Context("when it was never called for SetUP", func() {
			Context("and a namespace does exist", func() {
				It("exits with 'success' error code", func() {
					_, _, netnspath, err := CreateContainerNamespace()
					Expect(err).ShouldNot(HaveOccurred())
					exitCode, err := DeleteContainer(netconf, netnspath, "")
					Expect(err).ShouldNot(HaveOccurred())
					Expect(exitCode).To(Equal(0))
				})
			})

			Context("and no namespace exists", func() {
				It("exits with 'success' error code", func() {
					exitCode, err := DeleteContainer(netconf, "/not/a/real/path1234567890", "")
					Expect(err).ShouldNot(HaveOccurred())
					Expect(exitCode).To(Equal(0))
				})
			})
		})
	})

	Describe("ADD a continer with ContainerID and DEL it with the same ContainerID", func() {
		Context("Use the same CNI_ContainerID to ADD and DEL the container", func() {
			netconf := fmt.Sprintf(`
            {
              "cniVersion": "%s",
              "name": "net1",
              "type": "calico",
              "etcd_endpoints": "http://%s:2379",
              "ipam": {
                "type": "host-local",
                "subnet": "10.0.0.0/8"
              }
            }`, cniVersion, os.Getenv("ETCD_IP"))

			It("should succesfully ADD and DEL the container with CNI_ContainerID", func() {
				cniContainerID := "container-id-001"

				// ADD the container with passing a CNI_ContainerID.
				workloadID, netnspath, session, _, _, _, err := CreateContainerWithId(netconf, "", "", cniContainerID)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit())

				result, err := GetResultForCurrent(session, cniVersion)
				if err != nil {
					log.Fatalf("Error getting result from the session: %v\n", err)
				}

				log.Printf("Unmarshaled result: %v\n", result)

				// The endpoint is created in the backend datastore.
				endpoints, err := calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             hostname,
					Name:             "eth0",
					Workload:         workloadID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

				// Delete the container with the same CNI_ContainerID.
				exitCode, err := DeleteContainerWithId(netconf, netnspath, "", cniContainerID)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(exitCode).Should(Equal(0))

				// The endpoint should not exist in the backend datastore.
				endpoints, err = calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(0))
			})
		})
	})

	// See: https://github.com/kubernetes/kubernetes/issues/44100. This could happen with other orchestrators as well.
	Describe("ADD a continer with a ContainerID and DEL it with the same ContainerID then ADD a new container with a different ContainerID, and send a DEL for the old ContainerID", func() {
		Context("Use different CNI_ContainerIDs to ADD and DEL the container", func() {
			netconf := fmt.Sprintf(`
            {
              "cniVersion": "%s",
              "name": "net1",
              "type": "calico",
              "etcd_endpoints": "http://%s:2379",
              "ipam": {
                "type": "host-local",
                "subnet": "10.0.0.0/8"
              }
            }`, cniVersion, os.Getenv("ETCD_IP"))

			It("should succesfully ADD and DEL the container with CNI_ContainerID", func() {
				cniContainerIDX := "container-id-00X"
				cniContainerIDY := "container-id-00Y"

				// ADD the container with passing a CNI_ContainerID "X".
				workloadID, netnspath, session, _, _, _, err := CreateContainerWithId(netconf, "", "", cniContainerIDX)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit())

				result, err := GetResultForCurrent(session, cniVersion)
				if err != nil {
					log.Fatalf("Error getting result from the session: %v\n", err)
				}

				log.Printf("Unmarshaled result: %v\n", result)

				// Assert that the endpoint is created in the backend datastore with ContainerID "X".
				endpoints, err := calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             hostname,
					Name:             "eth0",
					Workload:         workloadID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

				// Delete the container with the CNI_ContainerID "X".
				exitCode, err := DeleteContainerWithId(netconf, netnspath, "", cniContainerIDX)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(exitCode).Should(Equal(0))

				// The endpoint for ContainerID "X" should not exist in the backend datastore.
				endpoints, err = calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(0))

				// ADD a new container with passing a CNI_ContainerID "Y".
				workloadID, netnspath, session, _, _, _, err = CreateContainerWithId(netconf, "", "", cniContainerIDY)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(session).Should(gexec.Exit())

				result, err = GetResultForCurrent(session, cniVersion)
				if err != nil {
					log.Fatalf("Error getting result from the session: %v\n", err)
				}

				log.Printf("Unmarshaled result: %v\n", result)

				// Assert that the endpoint is created in the backend datastore with ContainerID "Y".
				endpoints, err = calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             hostname,
					Name:             "eth0",
					Workload:         workloadID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

				// Delete the container with the CNI_ContainerID "X" again.
				exitCode, err = DeleteContainerWithId(netconf, netnspath, "", cniContainerIDX)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(exitCode).Should(Equal(0))

				// Assert that the endpoint with ContainerID "Y" is still in the datastore.
				endpoints, err = calicoClient.WorkloadEndpoints().List(api.WorkloadEndpointMetadata{})
				Expect(err).ShouldNot(HaveOccurred())
				Expect(endpoints.Items).Should(HaveLen(1))

				// Set the Revision to nil since we can't assert it's exact value.
				endpoints.Items[0].Metadata.Revision = nil
				Expect(endpoints.Items[0].Metadata).Should(Equal(api.WorkloadEndpointMetadata{
					Node:             hostname,
					Name:             "eth0",
					Workload:         workloadID,
					ActiveInstanceID: "",
					Orchestrator:     "cni",
				}))

			})
		})
	})
})

package dra

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	helper "github.com/openshift/origin/test/extended/dra/helper"
	"github.com/openshift/origin/test/extended/dra/nvidia"
	exutil "github.com/openshift/origin/test/extended/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	clientgodynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/test/e2e/framework"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

const (
	// NFD will apply this label to the worker node if
	// an Nvidia GPU is present on the node
	nvidiaGPU = "feature.node.kubernetes.io/pci-10de.present"

	// the test applies this label to the gpu worker node that has been
	// selected, the nvidia dra driver will be installed on this node
	gpuNodeLabel = "dra.e2e.openshift.io/nvidia-dra-driver"

	// this is the node label that bears the RHCOS version
	rhcosVersion = "feature.node.kubernetes.io/system-os_release.OSTREE_VERSION"
)

var _ = g.Describe("[sig-node] [Suite:openshift/dra-gpu-validation] [Feature:DynamicResourceAllocation]", g.Ordered, func() {
	defer g.GinkgoRecover()

	// clients used by the setup code inside BeforeAll
	var (
		config    *rest.Config
		clientset *kubernetes.Clientset
		dynamic   *clientgodynamic.DynamicClient
	)

	t := g.GinkgoTB()
	// this framework object used by setup code only
	// TODO: can we avoid BeforeEach, and AfterEach being invoked
	setup := framework.NewDefaultFramework("dra-setup")
	setup.SkipNamespaceCreation = true
	oc := exutil.NewCLIWithFramework(setup)

	g.BeforeAll(func(ctx context.Context) {
		var err error
		config, err = clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
		framework.ExpectNoError(err)
		clientset, err = kubernetes.NewForConfig(config)
		framework.ExpectNoError(err)
		dynamic, err = clientgodynamic.NewForConfig(clientgodynamic.ConfigFor(config))
		framework.ExpectNoError(err)

		// TODO: the framework fills these when BeforeEach is invoked
		// we are using this framework object in setup code
		setup.ClientSet = clientset
		setup.DynamicClient = dynamic
	})

	// Nvidia GPU driver setup and tests follow
	g.Context("[Driver:dra-nvidia-driver]", func() {
		// the gpu worker node we select where the test pods will run
		var node *corev1.Node

		// ensure that node-feature-discovery already labeled the GPU node for the tests
		g.BeforeAll(func(ctx context.Context) {
			g.By("waiting for node-feature-discovery to label the gpu worker node")
			o.Eventually(ctx, func(ctx context.Context) error {
				result, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("%s=true", nvidiaGPU),
				})
				if err != nil || len(result.Items) == 0 {
					return fmt.Errorf("still waiting for node labels to show up: %w", err)
				}
				// choose the first gpu worker node
				node = &result.Items[0]

				err = helper.EnsureNodeLabel(ctx, clientset, node.Name, gpuNodeLabel, "")
				o.Expect(err).Should(o.BeNil())
				t.Logf("node=%s has been selected, the test will be exercised on this node", node.Name)
				return nil
			}).WithPolling(time.Second).Should(o.BeNil(), "timeout while waiting for the expected node label to show up")

			t.Logf("node=%s has been selected, test will be exercised on this node", node.Name)
			t.Logf("labels=%s", framework.PrettyPrintJSON(node.Labels))
		})

		// we build the nvidia gpu driver using the driver toolkit, let's
		// ensure that the driver toolkit is installed properly
		g.BeforeAll(func(ctx context.Context) {
			dtk := nvidia.NewDriverToolkitProber(oc, clientset)
			osrelease, err := dtk.GetOSReleaseInfo(t, node)
			o.Expect(err).Should(o.BeNil())
			o.Expect(osrelease.RHCOSVersion).NotTo(o.BeEmpty())

			t.Logf("node=%s os-release: %+v", node.Name, osrelease)
			// TODO: nfd does not always label the node with the right RHCOS version
			// let's make sure, the selected gpu node has the right RHCOS
			// version, otherwise the nvidia gpu operator will fail to
			// install the gpu driver
			if _, ok := node.Labels[rhcosVersion]; !ok {
				g.By(fmt.Sprintf("adding label: %q to node: %s", rhcosVersion, node.Name))
				err := helper.EnsureNodeLabel(ctx, clientset, node.Name, rhcosVersion, osrelease.RHCOSVersion)
				o.Expect(err).Should(o.BeNil())
			}
		})

		var operator *nvidia.GpuOperator
		// ensure nvidia gpu operator components are ready
		g.BeforeAll(func(ctx context.Context) {
			operator = nvidia.NewGPUOperator(g.GinkgoTB(), clientset, setup, "nvidia-gpu-operator")
			g.By("waiting for nvidia-gpu-operator to be ready")
			o.Expect(operator.Ready(ctx, node)).To(o.Succeed(), "nvidia-gpu-operator should be ready")
		})

		// ensure nvidia-dra-driver-gpu is ready for the tests
		var driver *nvidia.NvidiaDRADriverGPU
		g.BeforeAll(func(ctx context.Context) {
			driver = nvidia.NewNvidiaDRADriverGPU(g.GinkgoTB(), clientset, "nvidia-dra-driver-gpu")
			g.By("waiting for nvidia-dra-driver-gpu to be ready")
			o.Expect(driver.Ready(ctx, node)).To(o.Succeed(), "nvidia-dra-driver-gpu should be ready")
		})

		g.BeforeAll(func(ctx context.Context) {
			// TODO: nvidia gpu operator checks if the following
			// label is present on the node as a precondition, before
			// it installs mig-manager on the gpu node, or other
			// components
			product := "nvidia.com/gpu.product"
			if p, ok := node.Labels[product]; !ok || p == "" {
				t.Logf("node: %s label: %s is not set", node.Name, product)

				g.By(fmt.Sprintf("discovering gpu feature on node: %s", node.Name))
				gpuProductName, err := operator.DiscoverGPUProudct(ctx, node)
				o.Expect(err).Should(o.BeNil())
				o.Expect(gpuProductName).ShouldNot(o.BeEmpty())
				t.Logf("node: %s gpu proudct name: %s", node.Name, gpuProductName)

				err = helper.EnsureNodeLabel(ctx, clientset, node.Name, product, gpuProductName)
				o.Expect(err).Should(o.BeNil())
			}
		})

		// initialize the framework object before any of our BeforeEach func
		var f *framework.Framework = framework.NewDefaultFramework("dra-nvidia")

		g.BeforeEach(func(ctx context.Context) {
			g.By(fmt.Sprintf("waiting for the driver: %s to advertise its resources", driver.Class()))
			dc, slices := driver.EventuallyPublishResources(ctx, node)
			t.Logf("the driver has published deviceclasses: %s", framework.PrettyPrintJSON(dc))
			t.Logf("the driver has published resourceslices: %s", framework.PrettyPrintJSON(slices))
		})

		g.It("one pod, one container, asking for 1 distinct GPU", func(ctx context.Context) {
			advertised, err := driver.ListPublishedDevicesFromResourceSlice(ctx, node)
			o.Expect(err).Should(o.BeNil())
			t.Logf("collected advertised devices from the resourceslices: %v", advertised)

			// we want the pod to use a whole gpu
			gpus := advertised.FilterBy(func(gpu nvidia.NvidiaGPU) bool { return gpu.Type == "gpu" }).Names()
			t.Logf("the following whole gpus are available to be used: %v", gpus)
			o.Expect(len(gpus)).To(o.BeNumerically(">=", 1))

			common := commonSpec{
				f:                     f,
				class:                 driver.Class(),
				node:                  node,
				deviceNamesAdvertised: gpus,
				newContainer: func(name string) corev1.Container {
					return corev1.Container{
						Name:    name,
						Image:   "ubuntu:22.04",
						Command: []string{"bash", "-c"},
						Args:    []string{"nvidia-smi -L; trap 'exit 0' TERM; sleep 9999 & wait"},
					}
				},
			}
			common.Test(ctx, g.GinkgoTB())
		})

		g.It("two pods, one container each, asking for 1 distinct GPU", func(ctx context.Context) {
			advertised, err := driver.ListPublishedDevicesFromResourceSlice(ctx, node)
			o.Expect(err).Should(o.BeNil())
			t.Logf("collected advertised devices from the resourceslices: %v", advertised)

			gpus := advertised.FilterBy(func(gpu nvidia.NvidiaGPU) bool { return gpu.Type == "gpu" })
			t.Logf("the following whole gpus are available to be used: %v", gpus)
			o.Expect(len(gpus)).To(o.BeNumerically(">=", 2), "need at least two whole gpus for this test")

			spec := distinctGPUsSpec{
				f:     f,
				class: driver.Class(),
				node:  node,
				gpu0:  gpus[0].UUID,
				gpu1:  gpus[1].UUID,
			}
			spec.Test(ctx, g.GinkgoTB())
		})

		g.It("two pods, share data using CUDA IPC", func(ctx context.Context) {
			advertised, err := driver.ListPublishedDevicesFromResourceSlice(ctx, node)
			o.Expect(err).Should(o.BeNil())
			t.Logf("collected advertised devices from the resourceslices: %v", advertised)

			gpus := advertised.FilterBy(func(gpu nvidia.NvidiaGPU) bool { return gpu.Type == "gpu" })
			t.Logf("the following whole gpus are available to be used: %v", gpus)
			o.Expect(len(gpus)).To(o.BeNumerically(">=", 2), "need at least two whole gpus for this test")

			cudaIPC := shareDataWithCUDAIPC{
				f:        f,
				class:    driver.Class(),
				node:     node,
				producer: gpus[1].UUID,
				consumer: gpus[0].UUID,
			}
			cudaIPC.Test(ctx, g.GinkgoTB())
		})

		g.Context("[TimeSlicing=true]", func() {
			g.It("one pod, 2 containers, with time slice", func(ctx context.Context) {
				timeSlicing := gpuTimeSlicingWithCUDASpec{
					f:      f,
					class:  driver.Class(),
					node:   node,
					dra:    driver,
					driver: operator,
				}
				timeSlicing.Test(ctx, g.GinkgoTB())
			})
		})

		g.Context("[MPS=true]", func() {
			g.It("one pod, 2 containers share GPU using MPS", func(ctx context.Context) {
				mpsShared := mpsWithCUDASpec{
					f:      f,
					class:  driver.Class(),
					node:   node,
					dra:    driver,
					driver: operator,
				}
				mpsShared.Test(ctx, g.GinkgoTB())
			})
		})

		g.Context("[TimeSlicing,MPS=true]", func() {
			g.It("gpu sharing with both MPS and TimeSlicing", func(ctx context.Context) {
				advertised, err := driver.ListPublishedDevicesFromResourceSlice(ctx, node)
				o.Expect(err).Should(o.BeNil())
				t.Logf("collected advertised devices from the resourceslices: %v", advertised)

				gpus := advertised.FilterBy(func(gpu nvidia.NvidiaGPU) bool { return gpu.Type == "gpu" })
				t.Logf("the following whole gpus are available to be used: %v", gpus)
				o.Expect(len(gpus)).To(o.BeNumerically(">=", 2), "need at least two whole gpus for this test")

				spec := gpuTimeSlicingAndMPSWithCUDASpec{
					f:      f,
					class:  driver.Class(),
					node:   node,
					driver: operator,
				}
				spec.Test(ctx, g.GinkgoTB())
			})
		})

		g.Context("[MIGEnabled=true]", func() {
			g.BeforeAll(func(ctx context.Context) {
				// we will use a custom MIG configuration
				config := corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "dra-e2e-mig-parted-config",
						Namespace: operator.Namespace(),
					},
					Data: map[string]string{
						"config.yaml": `
    version: v1
    mig-configs:
      all-disabled:
        - devices: all
          mig-enabled: false
      gpu-e2e:
        - devices: [0]
          mig-enabled: true
          mig-devices:
            "3g.20gb": 1
            "2g.10gb": 1
            "1g.5gb": 1
        - devices: [1, 2, 3, 4, 5, 6, 7]
          mig-enabled: false
`},
				}
				o.Expect(helper.EnsureConfigMap(ctx, clientset, &config)).Should(o.BeNil())

				g.By("configuring nvidia-mig-manager")
				patchBytes := `
{
  "spec": {
    "mig": {
      "strategy": "mixed"
    },
    "migManager": {
      "config": {
        "default": "all-disabled",
        "name": "dra-e2e-mig-parted-config"
      },
      "env": [
        {
          "name": "WITH_REBOOT",
          "value": "true"
        }
      ]
    },
    "validator": {
      "cuda": {
        "env": [
          {
            "name": "WITH_WORKLOAD",
            "value": "false"
          }
        ]
      }
    }
  }
}
`
				resource := dynamic.Resource(schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"})
				policy, err := resource.Patch(ctx, "cluster-policy", types.MergePatchType, []byte(patchBytes), metav1.PatchOptions{})
				o.Expect(err).Should(o.BeNil())
				t.Logf("gpu operator cluster policy: \n%s\n", framework.PrettyPrintJSON(policy))

				g.By(fmt.Sprintf("waiting for nvidia-mig-manager to be ready"))
				o.Expect(operator.MIGManagerReady(ctx, node)).Should(o.BeNil())
			})

			g.It("one pod, three containers, asking for 3g.20gb, 2g.10gb, and 1g.5gb respectively", func(ctx context.Context) {
				ascending := func(x, y string) bool {
					return strings.Compare(x, y) < 0 // Ascending order
				}

				// MIG devices we want to setup
				want := []string{"3g.20gb", "2g.10gb", "1g.5gb"}

				// apply the desired MIG configuration
				g.By(fmt.Sprintf("labeling node: %s for nvidia.com/mig.config: %s", node.Name, "gpu-e2e"))
				err := helper.EnsureNodeLabel(ctx, clientset, node.Name, "nvidia.com/mig.config", "gpu-e2e")
				o.Expect(err).Should(o.BeNil())

				g.By("waiting for the gpu driver to advertise the expected MIG devices")
				advertised := nvidia.NvidiaGPUs{}
				o.Eventually(ctx, func(ctx context.Context) error {
					got, err := operator.ListMIGDevicesUsingNvidiaSMI(ctx, node)
					if err != nil {
						return err
					}
					if !cmp.Equal(want, got.Names(), cmpopts.SortSlices(ascending)) {
						return fmt.Errorf("still waiting for MIG devices to show up, want: %v, got: %v", want, got)
					}
					advertised = got
					return nil
				}).WithPolling(10*time.Second).Should(o.BeNil(), "timeout waiting for expected mig devices")
				t.Logf("the gpu driver is advertising these mig devices %v", advertised)

				// TODO: the DRA driver does not pick up the MIG slices, restarting the plugin seems to do the trick
				o.Expect(driver.RemovePluginFromNode(ctx, node)).Should(o.BeNil())
				g.By("waiting for nvidia-dra-driver-gpu to be ready")
				o.Expect(driver.Ready(ctx, node)).To(o.Succeed(), "nvidia-dra-driver-gpu should be ready")

				g.By("waiting for the dra driver to advertise the mig devices in its resourceslices")
				o.Eventually(ctx, func(ctx context.Context) error {
					all, err := driver.ListPublishedDevicesFromResourceSlice(ctx, node)
					if err != nil {
						return err
					}
					migs := all.FilterBy(func(gpu nvidia.NvidiaGPU) bool { return gpu.Type == "mig" })

					if want, got := advertised.UUIDs(), migs.UUIDs(); !cmp.Equal(want, got, cmpopts.SortSlices(ascending)) {
						return fmt.Errorf("still waiting for the dra driver to publish the MIG devices, want: %v, got: %v", want, got)
					}
					t.Logf("the dra driver has published the mig devices in its resourceslices: %s", framework.PrettyPrintJSON(migs))
					return nil
				}).WithPolling(time.Second).Should(o.BeNil(), "timeout while waiting for the dra driver to advertise its resources")

				mig := gpuMIGSpec{
					f:       f,
					class:   driver.Class(),
					node:    node,
					uuids:   advertised.UUIDs(),
					devices: advertised.Names(),
				}
				mig.Test(ctx, g.GinkgoTB())
			})
		})
	})
})

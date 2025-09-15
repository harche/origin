package nvidia

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	helper "github.com/openshift/origin/test/extended/dra/helper"
)

func NewGPUOperator(t testing.TB, clientset kubernetes.Interface, f *framework.Framework, namespace string) *GpuOperator {
	return &GpuOperator{
		t:         t,
		clientset: clientset,
		f:         f,
		namespace: namespace,
	}
}

type GpuOperator struct {
	t         testing.TB
	f         *framework.Framework
	clientset kubernetes.Interface
	namespace string
}

func (d *GpuOperator) Namespace() string { return d.namespace }

func (d GpuOperator) Ready(ctx context.Context, node *corev1.Node) error {
	for _, probe := range []struct {
		component string
		enabled   bool
		options   metav1.ListOptions
	}{
		{
			enabled:   true,
			component: "nvidia-driver-daemonset",
			options: metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/component" + "=" + "nvidia-driver",
				FieldSelector: "spec.nodeName" + "=" + node.Name,
			},
		},
		{
			enabled:   true,
			component: "nvidia-container-toolkit-daemonset",
			options: metav1.ListOptions{
				LabelSelector: "app" + "=" + "nvidia-container-toolkit-daemonset",
				FieldSelector: "spec.nodeName" + "=" + node.Name,
			},
		},
		{
			enabled:   true,
			component: "gpu-feature-discovery-daemonset",
			options: metav1.ListOptions{
				LabelSelector: "app" + "=" + "gpu-feature-discovery",
				FieldSelector: "spec.nodeName" + "=" + node.Name,
			},
		},
	} {
		if probe.enabled {
			g.By(fmt.Sprintf("waiting for %s to be ready", probe.component))
			o.Eventually(ctx, func(ctx context.Context) error {
				return helper.PodRunningReady(ctx, d.t, d.clientset, probe.component, d.namespace, probe.options)
			}).WithPolling(5*time.Second).Should(o.BeNil(), fmt.Sprintf("[%s] pod should be ready", probe.component))
		}
	}

	return nil
}

func (d GpuOperator) MIGManagerReady(ctx context.Context, node *corev1.Node) error {
	for _, probe := range []struct {
		component string
		enabled   bool
		options   metav1.ListOptions
	}{
		{
			enabled:   true,
			component: "nvidia-mig-manager-daemonset",
			options: metav1.ListOptions{
				LabelSelector: "app" + "=" + "nvidia-mig-manager",
				FieldSelector: "spec.nodeName" + "=" + node.Name,
			},
		},
	} {
		if probe.enabled {
			g.By(fmt.Sprintf("waiting for %s to be ready", probe.component))
			o.Eventually(ctx, func(ctx context.Context) error {
				return helper.PodRunningReady(ctx, d.t, d.clientset, probe.component, d.namespace, probe.options)
			}).WithPolling(5*time.Second).Should(o.BeNil(), fmt.Sprintf("[%s] pod should be ready", probe.component))
		}
	}

	return nil
}

func (d GpuOperator) DiscoverGPUProudct(ctx context.Context, node *corev1.Node) (string, error) {
	client := d.clientset.CoreV1().Pods(d.namespace)
	result, err := client.List(ctx, metav1.ListOptions{
		LabelSelector: "app" + "=" + "gpu-feature-discovery",
		FieldSelector: "spec.nodeName" + "=" + node.Name,
	})
	if err != nil || len(result.Items) == 0 {
		return "", fmt.Errorf("did not find any pod for %s on node: %s - %w", "gpu-feature-discovery", node.Name, err)
	}
	pod := result.Items[0].Name

	cmd := []string{"cat", "/etc/kubernetes/node-feature-discovery/features.d/gfd"}
	g.By(fmt.Sprintf("exec into pod: %s command: %v", pod, cmd))
	stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, d.f, e2epod.ExecOptions{
		Command:       cmd,
		Namespace:     d.namespace,
		PodName:       pod,
		ContainerName: "gpu-feature-discovery",
		CaptureStdout: true,
		CaptureStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to run command %v on pod %s, stdout: %v, stderr: %v, err: %w", cmd, pod, stdout, stderr, err)
	}
	d.t.Logf("output of pod exec: %s:\n%s\n", pod, stdout)
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		after, found := strings.CutPrefix(strings.TrimSpace(sc.Text()), "nvidia.com/gpu.product=")
		if !found {
			continue
		}
		return strings.Trim(strings.TrimSpace(after), "'"), nil
	}

	return "", fmt.Errorf("nvidia.com/gpu.product not found in output")
}

func (d GpuOperator) RunNvidiSMI(ctx context.Context, node *corev1.Node, options ...string) ([]string, error) {
	client := d.clientset.CoreV1().Pods(d.namespace)
	result, err := client.List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component" + "=" + "nvidia-driver",
		FieldSelector: "spec.nodeName" + "=" + node.Name,
	})
	if err != nil || len(result.Items) == 0 {
		return nil, fmt.Errorf("did not find any pod for %s on node: %s - %w", "nvidia-driver-daemonset", node.Name, err)
	}
	pod := result.Items[0].Name

	cmd := []string{"nvidia-smi"}
	cmd = append(cmd, options...)
	g.By(fmt.Sprintf("exec into pod: %s, command: %v", pod, cmd))
	stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, d.f, e2epod.ExecOptions{
		Command:       cmd,
		Namespace:     d.namespace,
		PodName:       pod,
		ContainerName: "nvidia-driver-ctr",
		CaptureStdout: true,
		CaptureStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to run command %v on pod %s, stdout: %v, stderr: %v, err: %w", cmd, pod, stdout, stderr, err)
	}
	d.t.Logf("output of pod exec: %s:\n%s\n", pod, stdout)
	sc := bufio.NewScanner(strings.NewReader(stdout))
	lines := []string{}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, nil
}

func (d GpuOperator) ListMIGDevicesUsingNvidiaSMI(ctx context.Context, node *corev1.Node) (NvidiaGPUs, error) {
	lines, err := d.RunNvidiSMI(ctx, node, "-L")
	if err != nil {
		return nil, err
	}
	return ExtractMIGDeviceInfoFromNvidiaSMILines(lines), nil
}

func ExtractMIGDeviceInfoFromNvidiaSMILines(lines []string) NvidiaGPUs {
	gpus := NvidiaGPUs{}
	for _, line := range lines {
		gpu := ExtractMIGDeviceInfoFromNvidiaSMI(line)
		if gpu.Type != "mig" {
			continue
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

func ExtractMIGDeviceInfoFromNvidiaSMI(line string) NvidiaGPU {
	// example line
	// GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-fcf41002-68b6-5900-d7d1-74026173bb44)
	//   MIG 3g.20gb     Device  0: (UUID: MIG-e07a497d-1bb6-5a42-b670-1c33bf55ab6e)
	gpu := NvidiaGPU{}
	after, found := strings.CutPrefix(strings.TrimSpace(line), "MIG ")
	if !found {
		return gpu
	}
	gpu.Type = "mig"

	split := strings.Split(after, " ")
	gpu.Name = split[0]
	if len(split) > 0 {
		gpu.UUID = strings.TrimRight(split[len(split)-1], ")")
	}
	return gpu
}

func QueryGPUUsedByContainer(ctx context.Context, t testing.TB, f *framework.Framework, name, namespace, container string) (NvidiaGPUs, error) {
	cmd := []string{"nvidia-smi", "--query-gpu=index,uuid", "--format=csv"}
	t.Logf("exec into pod: %s, container: %s, command: %v", name, container, cmd)
	stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, f, e2epod.ExecOptions{
		Command:       cmd,
		Namespace:     namespace,
		PodName:       name,
		ContainerName: container,
		CaptureStdout: true,
		CaptureStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to run command %v on pod %s, stdout: %v, stderr: %v, err: %w", cmd, name, stdout, stderr, err)
	}
	t.Logf("output of pod exec: %s/%s (container=%s):\n\n%s\n%s", namespace, name, container, stdout, stderr)
	gpus := NvidiaGPUs{}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	var ignored bool
	for sc.Scan() {
		s := strings.Split(sc.Text(), ",")
		// ignore the first line, it's the header
		if !ignored {
			ignored = true
			continue
		}
		if len(s) != 2 {
			continue
		}
		gpus = append(gpus, NvidiaGPU{
			Index: strings.TrimSpace(s[0]),
			UUID:  strings.TrimSpace(s[1]),
		})
	}
	return gpus, nil
}

func (d GpuOperator) QueryCompute(ctx context.Context, node *corev1.Node, gpuIndex string) (NvidiaComputes, error) {
	options := []string{"--query-compute-apps=gpu_uuid,pid,process_name", "--format=csv"}
	lines, err := d.RunNvidiSMI(ctx, node, options...)
	if err != nil {
		return nil, err
	}

	processes := []NvidiaCompute{}
	var firsLineIngonred bool
	for _, line := range lines {
		// ignore the first line, it's the header
		if !firsLineIngonred {
			firsLineIngonred = true
			continue
		}
		s := strings.Split(line, ",")
		if len(s) != 3 {
			continue
		}
		processes = append(processes, NvidiaCompute{
			GPU:  strings.TrimSpace(s[0]),
			PID:  strings.TrimSpace(s[1]),
			Name: strings.TrimSpace(s[2]),
		})
	}
	return processes, nil
}

type NvidiaCompute struct {
	// process name, and pid
	Name string
	PID  string
	// UUID of the GPU on which this process is running
	GPU string
}

func (c NvidiaCompute) String() string {
	return fmt.Sprintf("gpu: %s, name: %s, pid: %s", c.GPU, c.Name, c.PID)
}

type NvidiaComputes []NvidiaCompute

func (s NvidiaComputes) FilterBy(f func(p NvidiaCompute) bool) NvidiaComputes {
	processes := NvidiaComputes{}
	for _, p := range s {
		if f(p) {
			processes = append(processes, p)
		}
	}
	return processes
}

func (s NvidiaComputes) Names() []string {
	names := []string{}
	for _, p := range s {
		names = append(names, p.Name)
	}
	return names
}

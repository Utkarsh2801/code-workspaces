package compute

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestKubernetesProvider(t *testing.T, objects ...runtime.Object) *KubernetesProvider {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	provider, err := NewKubernetesProvider(k8sClient)
	if err != nil {
		t.Fatalf("NewKubernetesProvider() error: %v", err)
	}
	return provider
}

func TestKubernetesProviderCreateGetAndDelete(t *testing.T) {
	provider := newTestKubernetesProvider(t)
	ctx := context.Background()

	instance, err := provider.Create(ctx, CreateRequest{
		UID:       "workspace-uid",
		Name:      "test-workspace",
		Namespace: "default",
		Owner:     "owner@example.com",
		Image:     "codercom/code-server",
		Port:      8080,
		Args:      []string{"--bind-addr", "0.0.0.0:8080"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if instance.Name != "test-workspace" {
		t.Errorf("Create() name = %q, want test-workspace", instance.Name)
	}

	var pod corev1.Pod
	if err := provider.client.Get(ctx, types.NamespacedName{Name: "test-workspace", Namespace: "default"}, &pod); err != nil {
		t.Fatalf("get created Pod: %v", err)
	}
	if pod.Labels[LabeUID] != "workspace-uid" {
		t.Errorf("Pod UID label = %q, want workspace-uid", pod.Labels[LabeUID])
	}
	if pod.Spec.Containers[0].Image != "codercom/code-server" {
		t.Errorf("Pod image = %q, want codercom/code-server", pod.Spec.Containers[0].Image)
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("Pod container must disable privilege escalation")
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != workspaceMountPath {
		t.Errorf("Pod volume mounts = %+v, want %s mount", container.VolumeMounts, workspaceMountPath)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := provider.client.Get(ctx, types.NamespacedName{Name: "test-workspace", Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("get created PVC: %v", err)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != defaultStorageSize {
		t.Errorf("PVC storage = %s, want %s", pvc.Spec.Resources.Requests.Storage().String(), defaultStorageSize)
	}

	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.4"
	if err := provider.client.Status().Update(ctx, &pod); err != nil {
		t.Fatalf("update Pod status: %v", err)
	}

	instance, err = provider.Get(ctx, "workspace-uid")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if instance.State != StateRunning || instance.IP != "10.0.0.4" {
		t.Errorf("Get() = %+v, want running Pod with IP 10.0.0.4", instance)
	}

	if err := provider.Delete(ctx, "workspace-uid"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	_, err = provider.Get(ctx, "workspace-uid")
	if err == nil {
		t.Fatal("Get() after Delete() error = nil, want ErrNotFound")
	}
	var deletedPVC corev1.PersistentVolumeClaim
	if err := provider.client.Get(ctx, types.NamespacedName{Name: "test-workspace", Namespace: "default"}, &deletedPVC); err == nil {
		t.Fatal("PVC remains after Delete(), want deleted PVC")
	}
}

func TestKubernetesProviderGetNotFound(t *testing.T) {
	provider := newTestKubernetesProvider(t)
	_, err := provider.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("Get() error = nil, want not-found error")
	}
}

func TestKubernetesProviderList(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workspace",
			Namespace: "default",
			Labels: map[string]string{
				LabelManagedBy: managedValue,
				LabeUID:        "workspace-uid",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"},
	}
	provider := newTestKubernetesProvider(t, pod)

	instances, err := provider.List(context.Background())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("List() returned %d instances, want 1", len(instances))
	}
	if instances[0].UID != "workspace-uid" || instances[0].State != StateRunning {
		t.Errorf("List()[0] = %+v, want running UID workspace-uid", instances[0])
	}
}
